package control

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/auth"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/store"
)

//go:embed web/dist
var embeddedWeb embed.FS

//go:embed uninstall-edge.sh
var uninstallEdgeScript string

type Server struct {
	Store                     *store.Store
	Cipher                    *Cipher
	CA                        *InternalCA
	Publisher                 Publisher
	DNS                       integrations.DNSProvider
	ZoneResolver              integrations.ZoneResolver
	Cloudflare                *integrations.CloudflareDNS
	Issuer                    integrations.CertificateIssuer
	CertificateManager        *CertificateManager
	HealthManager             *HealthManager
	SiteDeleter               *SiteDeletionManager
	Settings                  *SettingsManager
	BackupValidator           BackupRepositoryValidator
	BackupStatusPath          string
	OnlineRestore             *OnlineRestoreManager
	Notifier                  integrations.Notifier
	Logs                      logstore.Store
	MonitoringHistory         logstore.MonitoringHistoryReader
	MonitoringWriter          logstore.MonitoringHistoryEnqueuer
	smtpNotifierFactory       func(SMTPProfile, string) integrations.Notifier
	ControlURL                string
	EdgeControlURL            string
	EdgeBinaryURL             string
	EdgeBinarySHA256          string
	EdgeBinaryPath            string
	NginxBundleURL            string
	NginxBundleSHA256         string
	NginxBundlePath           string
	NginxVersion              string
	NginxUpdates              *NginxUpdateManager
	StaticAssetDirectory      string
	InitializationTokenPath   string
	SetupAllowCIDRs           []*net.IPNet
	TrustedProxyCIDRs         []*net.IPNet
	Logger                    *slog.Logger
	RestartControl            func()
	WireGuardEndpointResolver func(context.Context, string) ([]net.IP, error)
	machineStatusMu           sync.RWMutex
	machineStatuses           map[string]domain.MachineStatus
	machineStatusSubscribers  map[string]map[uint64]chan domain.MachineStatus
	machineStatusSubscriberID uint64
	edgeSecurityRevisionMu    sync.Mutex
	edgeSecurityRevision      string
	edgeSecurityExpiresAt     time.Time
	edgeSecurityRevisionSet   bool
	staticAssetMu             sync.Mutex
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /install-edge.sh", s.bootstrapEdgeScript)
	mux.HandleFunc("GET /install-edge.service", s.bootstrapEdgeService)
	mux.HandleFunc("GET /install-edge-updater.service", s.bootstrapEdgeUpdaterService)
	mux.HandleFunc("GET /install-edge-nginx.service", s.bootstrapEdgeNginxService)
	mux.HandleFunc("GET /install-origin-wireguard.sh", s.installOriginWireGuard)
	mux.HandleFunc("GET /uninstall-edge.sh", s.uninstallEdgeScript)
	mux.HandleFunc("GET /downloads/cdn-edge-agent-linux-amd64", s.edgeBinary)
	mux.HandleFunc("GET /downloads/cdn-nginx-linux-amd64.tar.gz", s.nginxBundle)
	mux.HandleFunc("GET /downloads/nginx/{sha256}/cdn-nginx-linux-amd64.tar.gz", s.versionedNginxBundle)
	mux.HandleFunc("GET /downloads/nginx/{sha256}/install-edge.sh", s.versionedEdgeInstaller)
	mux.HandleFunc("GET /api/setup/status", s.setupStatus)
	mux.HandleFunc("GET /api/branding", s.getPublicBranding)
	mux.HandleFunc("POST /api/setup/begin", s.beginSetup)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/setup/finish", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/auth/passkey/begin", s.beginPasskeyLogin)
	mux.HandleFunc("POST /api/auth/passkey/finish", s.finishPasskeyLogin)
	mux.HandleFunc("POST /api/logout", s.requireAdmin(s.logout))
	mux.HandleFunc("GET /api/session", s.requireAdmin(s.session))
	mux.HandleFunc("GET /api/system/info", s.requireAdmin(s.systemInfo))
	mux.HandleFunc("GET /api/overview", s.requireAdmin(s.overview))
	mux.HandleFunc("GET /api/health/reconciliation", s.requireAdmin(s.healthReconciliationStatus))
	mux.HandleFunc("GET /api/messages", s.requireAdmin(s.listMessages))
	mux.HandleFunc("POST /api/messages/read-all", s.requireAdmin(s.markAllMessagesRead))
	mux.HandleFunc("POST /api/messages/{id}/read", s.requireAdmin(s.markMessageRead))
	mux.HandleFunc("DELETE /api/messages/{id}", s.requireAdmin(s.deleteMessage))
	mux.HandleFunc("GET /api/logs", s.requireAdmin(s.searchLogs))
	mux.HandleFunc("GET /api/logs/{id}", s.requireAdmin(s.getLog))
	mux.HandleFunc("GET /api/settings", s.requireAdmin(s.getSettings))
	mux.HandleFunc("GET /api/settings/authentication", s.requireAdmin(s.getAuthenticationSettings))
	mux.HandleFunc("POST /api/settings/authentication/reauthenticate", s.requireAdmin(s.reauthenticate))
	mux.HandleFunc("POST /api/settings/authentication/totp/begin", s.requireRecentAdmin(s.beginTOTPReplacement))
	mux.HandleFunc("PUT /api/settings/authentication/totp", s.requireRecentAdmin(s.replaceTOTP))
	mux.HandleFunc("POST /api/settings/authentication/passkeys/begin", s.requireRecentAdmin(s.beginPasskeyRegistration))
	mux.HandleFunc("POST /api/settings/authentication/passkeys/finish", s.requireRecentAdmin(s.finishPasskeyRegistration))
	mux.HandleFunc("PUT /api/settings/authentication/passkeys", s.requireRecentAdmin(s.updatePasskeyEnabled))
	mux.HandleFunc("DELETE /api/settings/authentication/passkeys/{id}", s.requireRecentAdmin(s.deletePasskey))
	mux.HandleFunc("PUT /api/settings/branding", s.requireAdmin(s.updateBrandingSettings))
	mux.HandleFunc("PUT /api/settings/cache", s.requireAdmin(s.updateCacheSettings))
	mux.HandleFunc("PUT /api/settings/dns", s.requireAdmin(s.updateDNSSettings))
	mux.HandleFunc("PUT /api/settings/cloudflare", s.requireAdmin(s.updateCloudflareSettings))
	mux.HandleFunc("DELETE /api/settings/cloudflare", s.requireAdmin(s.clearCloudflareSettings))
	mux.HandleFunc("POST /api/settings/cloudflare/test", s.requireAdmin(s.testCloudflareSettings))
	mux.HandleFunc("PUT /api/settings/smtp", s.requireAdmin(s.updateSMTPSettings))
	mux.HandleFunc("DELETE /api/settings/smtp", s.requireAdmin(s.clearSMTPSettings))
	mux.HandleFunc("POST /api/settings/smtp/test", s.requireAdmin(s.testSMTPSettings))
	mux.HandleFunc("PUT /api/settings/backup", s.requireAdmin(s.updateBackupSettings))
	mux.HandleFunc("DELETE /api/settings/backup", s.requireAdmin(s.clearBackupSettings))
	mux.HandleFunc("POST /api/settings/backup/test", s.requireAdmin(s.testBackupSettings))
	mux.HandleFunc("GET /api/backups/status", s.requireAdmin(s.backupRunStatus))
	mux.HandleFunc("GET /api/backups/snapshots", s.requireAdmin(s.listBackupSnapshots))
	mux.HandleFunc("DELETE /api/backups/snapshots/{id}", s.requireAdmin(s.deleteBackupSnapshot))
	mux.HandleFunc("GET /api/backups/restores/current", s.requireAdmin(s.currentOnlineRestore))
	mux.HandleFunc("POST /api/backups/restores", s.requireAdmin(s.startOnlineRestore))
	mux.HandleFunc("POST /api/backups/restores/{id}/commit", s.requireAdmin(s.commitOnlineRestore))
	mux.HandleFunc("DELETE /api/backups/restores/{id}", s.requireAdmin(s.cancelOnlineRestore))
	mux.HandleFunc("GET /api/security", s.requireAdmin(s.getSecurityOverview))
	mux.HandleFunc("GET /api/monitoring", s.requireAdmin(s.monitoringOverview))
	mux.HandleFunc("GET /api/monitoring/smart-routing", s.requireAdmin(s.smartRoutingOverview))
	mux.HandleFunc("PUT /api/monitoring/nodes/{id}/smart-routing", s.requireAdmin(s.updateNodeSmartRouting))
	mux.HandleFunc("GET /api/monitoring/nodes/{id}/history", s.requireAdmin(s.monitoringNodeHistory))
	mux.HandleFunc("POST /api/monitoring/targets", s.requireAdmin(s.createMonitoringTarget))
	mux.HandleFunc("PUT /api/monitoring/targets/{id}", s.requireAdmin(s.updateMonitoringTarget))
	mux.HandleFunc("DELETE /api/monitoring/targets/{id}", s.requireAdmin(s.deleteMonitoringTarget))
	mux.HandleFunc("POST /api/security/policies", s.requireAdmin(s.createSecurityPolicy))
	mux.HandleFunc("PUT /api/security/policies/{id}", s.requireAdmin(s.updateSecurityPolicy))
	mux.HandleFunc("DELETE /api/security/policies/{id}", s.requireAdmin(s.deleteSecurityPolicy))
	mux.HandleFunc("POST /api/security/policies/{id}/move", s.requireAdmin(s.moveSecurityPolicy))
	mux.HandleFunc("POST /api/security/pow-policies", s.requireAdmin(s.createPOWPolicy))
	mux.HandleFunc("PUT /api/security/pow-policies/{id}", s.requireAdmin(s.updatePOWPolicy))
	mux.HandleFunc("DELETE /api/security/pow-policies/{id}", s.requireAdmin(s.deletePOWPolicy))
	mux.HandleFunc("POST /api/security/rate-limit-policies", s.requireAdmin(s.createRateLimitPolicy))
	mux.HandleFunc("PUT /api/security/rate-limit-policies/{id}", s.requireAdmin(s.updateRateLimitPolicy))
	mux.HandleFunc("DELETE /api/security/rate-limit-policies/{id}", s.requireAdmin(s.deleteRateLimitPolicy))
	mux.HandleFunc("POST /api/security/deploy", s.requireAdmin(s.deploySecurityPolicies))
	mux.HandleFunc("DELETE /api/security/bans/{ip}", s.requireAdmin(s.deleteSecurityBan))
	mux.HandleFunc("GET /api/static-assets", s.requireAdmin(s.listStaticAssets))
	mux.HandleFunc("POST /api/static-assets", s.requireAdmin(s.uploadStaticAsset))
	mux.HandleFunc("PUT /api/static-assets/{id}", s.requireAdmin(s.updateStaticAsset))
	mux.HandleFunc("DELETE /api/static-assets/{id}", s.requireAdmin(s.deleteStaticAsset))
	mux.HandleFunc("POST /api/static-assets/{id}/bindings", s.requireAdmin(s.createStaticAssetBinding))
	mux.HandleFunc("PUT /api/static-assets/{id}/bindings/{bindingID}", s.requireAdmin(s.updateStaticAssetBinding))
	mux.HandleFunc("DELETE /api/static-assets/{id}/bindings/{bindingID}", s.requireAdmin(s.deleteStaticAssetBinding))
	mux.HandleFunc("GET /api/nodes", s.requireAdmin(s.listNodes))
	mux.HandleFunc("GET /api/nginx/artifacts", s.requireAdmin(s.nginxArtifactStatus))
	mux.HandleFunc("POST /api/nginx/artifacts/check", s.requireAdmin(s.checkNginxArtifacts))
	mux.HandleFunc("POST /api/nginx/artifacts/{sha256}/promote", s.requireAdmin(s.promoteNginxArtifact))
	mux.HandleFunc("POST /api/nodes", s.requireAdmin(s.createNode))
	mux.HandleFunc("POST /api/nodes/upgrade-all", s.requireAdmin(s.startAllNodeUpgrades))
	mux.HandleFunc("GET /api/nodes/{id}", s.requireAdmin(s.nodeDetail))
	mux.HandleFunc("GET /api/nodes/{id}/machine-status/events", s.requireAdmin(s.machineStatusEvents))
	mux.HandleFunc("GET /api/nodes/{id}/cache-status", s.requireAdmin(s.nodeCacheStatus))
	mux.HandleFunc("PUT /api/nodes/{id}/cache", s.requireAdmin(s.updateNodeCacheSettings))
	mux.HandleFunc("PUT /api/nodes/{id}/nginx-capacity", s.requireAdmin(s.updateNodeNginxCapacity))
	mux.HandleFunc("POST /api/nodes/{id}/enrollment-token", s.requireAdmin(s.createEnrollmentToken))
	mux.HandleFunc("POST /api/nodes/{id}/status", s.requireAdmin(s.setNodeStatus))
	mux.HandleFunc("GET /api/nodes/{id}/upgrade", s.requireAdmin(s.nodeUpgradeStatus))
	mux.HandleFunc("POST /api/nodes/{id}/upgrade", s.requireAdmin(s.startNodeUpgrade))
	mux.HandleFunc("POST /api/nodes/{id}/uninstall", s.requireAdmin(s.prepareNodeUninstall))
	mux.HandleFunc("GET /api/nodes/{id}/uninstall", s.requireAdmin(s.nodeUninstallStatus))
	mux.HandleFunc("POST /api/nodes/{id}/uninstall/command", s.requireAdmin(s.createNodeUninstallCommand))
	mux.HandleFunc("DELETE /api/nodes/{id}/uninstall", s.requireAdmin(s.cancelNodeUninstall))
	mux.HandleFunc("POST /api/nodes/{id}/uninstall/force-complete", s.requireAdmin(s.forceCompleteNodeUninstall))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.requireAdmin(s.deleteNode))
	mux.HandleFunc("GET /api/wireguard/tunnels", s.requireAdmin(s.listWireGuardTunnels))
	mux.HandleFunc("GET /api/wireguard/suggested-cidr", s.requireAdmin(s.suggestedWireGuardCIDR))
	mux.HandleFunc("POST /api/wireguard/tunnels", s.requireAdmin(s.createWireGuardTunnel))
	mux.HandleFunc("GET /api/wireguard/tunnels/{id}", s.requireAdmin(s.getWireGuardTunnel))
	mux.HandleFunc("PUT /api/wireguard/tunnels/{id}", s.requireAdmin(s.updateWireGuardTunnel))
	mux.HandleFunc("DELETE /api/wireguard/tunnels/{id}", s.requireAdmin(s.deleteWireGuardTunnel))
	mux.HandleFunc("POST /api/wireguard/tunnels/{id}/install-command", s.requireAdmin(s.createWireGuardInstallCommand))
	mux.HandleFunc("GET /api/wireguard/tunnels/{id}/uninstall-command", s.requireAdmin(s.wireGuardUninstallCommand))
	mux.HandleFunc("GET /api/wireguard/performance-tests", s.requireAdmin(s.listWireGuardPerformanceTests))
	mux.HandleFunc("POST /api/wireguard/performance-tests", s.requireAdmin(s.createWireGuardPerformanceTest))
	mux.HandleFunc("GET /api/sites", s.requireAdmin(s.listSites))
	mux.HandleFunc("GET /api/cache/overview", s.requireAdmin(s.cacheOperationsOverview))
	mux.HandleFunc("GET /api/cache/operations", s.requireAdmin(s.listCacheOperations))
	mux.HandleFunc("POST /api/cache/operations", s.requireAdmin(s.createCacheOperation))
	mux.HandleFunc("GET /api/cache/operations/{id}", s.requireAdmin(s.getCacheOperation))
	mux.HandleFunc("POST /api/cache/operations/{id}/retry", s.requireAdmin(s.retryCacheOperation))
	mux.HandleFunc("GET /api/certificates", s.requireAdmin(s.certificatesOverview))
	mux.HandleFunc("POST /api/certificates/{id}/renew", s.requireAdmin(s.renewCertificate))
	mux.HandleFunc("POST /api/sites", s.requireAdmin(s.createSite))
	mux.HandleFunc("PUT /api/sites/{id}", s.requireAdmin(s.updateSite))
	mux.HandleFunc("DELETE /api/sites/{id}", s.requireAdmin(s.deleteSite))
	mux.HandleFunc("POST /api/sites/{id}/publish", s.requireAdmin(s.publishSite))
	mux.HandleFunc("GET /api/sites/{id}/publish-status", s.requireAdmin(s.publishStatus))
	mux.HandleFunc("GET /api/sites/{id}/delete-status", s.requireAdmin(s.deleteSiteStatus))
	mux.HandleFunc("POST /api/sites/{id}/invalidate-cache", s.requireAdmin(s.invalidateCache))
	mux.HandleFunc("POST /api/sites/{id}/certificate", s.requireAdmin(s.issueCertificate))
	mux.HandleFunc("GET /api/sites/{id}/certificate-task", s.requireAdmin(s.latestCertificateTask))
	mux.HandleFunc("GET /api/sites/{id}/tls-status", s.requireAdmin(s.tlsStatus))
	mux.HandleFunc("GET /api/sites/{id}/origin-allowlist", s.requireAdmin(s.originAllowlist))
	mux.HandleFunc("GET /api/sites/{id}/origin-connections", s.requireAdmin(s.siteOriginConnections))
	mux.HandleFunc("GET /api/tasks/{id}", s.requireAdmin(s.getTask))
	mux.HandleFunc("GET /api/sites/{id}/logs", s.requireAdmin(s.siteLogs))
	mux.HandleFunc("GET /api/sites/{id}/metrics", s.requireAdmin(s.siteMetrics))
	mux.HandleFunc("POST /api/edge/v1/enroll", s.enroll)
	mux.HandleFunc("POST /api/edge/v1/renew", s.requireEdge(s.renew))
	mux.HandleFunc("GET /api/edge/v1/desired-state", s.requireEdge(s.desiredState))
	mux.HandleFunc("GET /api/edge/v1/static-assets/{sha256}", s.requireEdge(s.edgeStaticAsset))
	mux.HandleFunc("POST /api/edge/v1/heartbeat", s.requireEdge(s.heartbeat))
	mux.HandleFunc("POST /api/edge/v1/machine-status", s.requireEdge(s.edgeMachineStatus))
	mux.HandleFunc("GET /api/edge/v1/monitoring-targets", s.requireEdge(s.edgeMonitoringTargets))
	mux.HandleFunc("POST /api/edge/v1/monitoring-results", s.requireEdge(s.edgeMonitoringReport))
	mux.HandleFunc("GET /api/edge/v1/upgrade", s.requireEdge(s.edgeUpgradeInstruction))
	mux.HandleFunc("POST /api/edge/v1/upgrade-report", s.requireEdge(s.edgeUpgradeReport))
	mux.HandleFunc("POST /api/edge/v1/security-events", s.requireEdge(s.edgeSecurityEvents))
	mux.HandleFunc("GET /api/edge/v1/wireguard/config", s.requireEdge(s.edgeWireGuardConfig))
	mux.HandleFunc("POST /api/edge/v1/wireguard/status", s.requireEdge(s.edgeWireGuardStatus))
	mux.HandleFunc("GET /api/edge/v1/wireguard/performance-test", s.requireEdge(s.edgeWireGuardPerformanceTest))
	mux.HandleFunc("POST /api/edge/v1/wireguard/performance-tests/{id}", s.requireEdge(s.edgeWireGuardPerformanceResult))
	mux.HandleFunc("GET /api/edge/v1/security-bans", s.requireEdge(s.edgeSecurityBans))
	mux.HandleFunc("POST /api/edge/v1/logs", s.requireEdge(s.writeLogs))
	mux.HandleFunc("POST /api/edge/v1/uninstall/start", s.startNodeUninstall)
	mux.HandleFunc("POST /api/edge/v1/uninstall/fail", s.failNodeUninstall)
	mux.HandleFunc("POST /api/edge/v1/uninstall/complete", s.completeNodeUninstall)
	mux.HandleFunc("POST /api/wireguard/v1/configure", s.configureWireGuardOrigin)
	web, err := fs.Sub(embeddedWeb, "web/dist")
	if err == nil {
		mux.Handle("/", staticWebHandler(http.FileServer(http.FS(web))))
	}
	return s.withSecurityHeaders(s.withRequestLog(s.withAPICacheControl(mux)))
}

func staticWebHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/assets/") {
			response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			response.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	if s.CA != nil {
		pool.AddCert(s.CA.Certificate)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
}

func ResolveEdgeBinarySHA256(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read EDGE_BINARY_PATH: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(contents)), nil
}

func (s *Server) health(response http.ResponseWriter, request *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) healthReconciliationStatus(response http.ResponseWriter, _ *http.Request) {
	if s.HealthManager == nil {
		writeJSON(response, http.StatusOK, HealthRoundStatus{})
		return
	}
	writeJSON(response, http.StatusOK, s.HealthManager.LastRound())
}

func (s *Server) backupRunStatus(response http.ResponseWriter, _ *http.Request) {
	if strings.TrimSpace(s.BackupStatusPath) == "" {
		writeJSON(response, http.StatusOK, nil)
		return
	}
	status, err := ReadBackupRunStatus(s.BackupStatusPath)
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(response, http.StatusOK, nil)
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) setupStatus(response http.ResponseWriter, request *http.Request) {
	hasAdmin, err := s.Store.HasAdmin()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	passkeyEnabled := false
	if hasAdmin {
		admin, adminErr := s.Store.Admin()
		if adminErr == nil {
			passkeyEnabled = s.passkeyLoginEnabled(request, admin)
		}
	}
	writeJSON(response, http.StatusOK, map[string]bool{"initialized": hasAdmin, "passkey_enabled": passkeyEnabled})
}

type setupBeginRequest struct {
	InitializationToken string `json:"initialization_token"`
	Password            string `json:"password"`
}

type setupRequest struct {
	InitializationToken string   `json:"initialization_token"`
	Password            string   `json:"password"`
	TOTPSecret          string   `json:"totp_secret"`
	TOTPCode            string   `json:"totp_code"`
	RecoveryCodes       []string `json:"recovery_codes"`
}

func (s *Server) beginSetup(response http.ResponseWriter, request *http.Request) {
	var input setupBeginRequest
	if !readJSON(response, request, &input) {
		return
	}
	if !s.authorizeSetup(response, request, input.InitializationToken) {
		return
	}
	if _, err := auth.HashPassword(input.Password); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	recoveryCodes, err := newRecoveryCodes(10)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"totp_secret":    secret,
		"otpauth_url":    totpProvisioningURL(secret, "simple_cdn", "admin"),
		"recovery_codes": recoveryCodes,
	})
}

func (s *Server) setup(response http.ResponseWriter, request *http.Request) {
	var input setupRequest
	if !readJSON(response, request, &input) {
		return
	}
	if !s.authorizeSetup(response, request, input.InitializationToken) {
		return
	}
	secret := auth.NormalizeTOTPSecret(input.TOTPSecret)
	if !auth.ValidTOTPSecret(secret) {
		writeError(response, http.StatusBadRequest, errors.New("invalid TOTP secret"))
		return
	}
	counter, valid := auth.MatchTOTP(secret, strings.TrimSpace(input.TOTPCode), time.Now())
	if !valid {
		writeError(response, http.StatusBadRequest, errors.New("TOTP confirmation code is invalid"))
		return
	}
	recoveryCodes, err := normalizeSetupRecoveryCodes(input.RecoveryCodes)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	passwordHash, err := auth.HashPassword(input.Password)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	encryptedTOTPSecret, err := s.encryptTOTPSecret(secret)
	if err != nil {
		writeError(response, http.StatusInternalServerError, errors.New("encrypt TOTP secret"))
		return
	}
	hashes := make([]string, 0, len(recoveryCodes))
	for _, code := range recoveryCodes {
		hashes = append(hashes, auth.RecoveryCodeHash(code))
	}
	if err := s.Store.CreateInitialAdminWithRecoveryCodesAndTOTPCounter(passwordHash, encryptedTOTPSecret, hashes, counter); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := ConsumeInitializationToken(s.InitializationTokenPath, input.InitializationToken); err != nil && s.Logger != nil {
		s.Logger.Error("remove initialization token after administrator activation", "error", err)
	}
	s.audit(request, "bootstrap", "admin", "admin", "admin", "created initial admin after TOTP confirmation")
	writeJSON(response, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) authorizeSetup(response http.ResponseWriter, request *http.Request, initializationToken string) bool {
	if len(s.SetupAllowCIDRs) > 0 && !s.setupIPAllowed(s.requestIP(request)) {
		writeError(response, http.StatusForbidden, errors.New("setup is not allowed from this address"))
		return false
	}
	hasAdmin, err := s.Store.HasAdmin()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return false
	}
	if hasAdmin {
		writeError(response, http.StatusConflict, errors.New("control plane is already initialized"))
		return false
	}
	if strings.TrimSpace(s.InitializationTokenPath) == "" {
		writeError(response, http.StatusServiceUnavailable, errors.New("initialization token is not configured"))
		return false
	}
	if err := VerifyInitializationToken(s.InitializationTokenPath, initializationToken); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("initialization token rejected", "remote", s.requestIP(request))
		}
		writeError(response, http.StatusForbidden, errors.New("invalid initialization token"))
		return false
	}
	return true
}

func normalizeSetupRecoveryCodes(codes []string) ([]string, error) {
	if len(codes) != 10 {
		return nil, errors.New("exactly 10 recovery codes are required")
	}
	normalized := make([]string, 0, len(codes))
	seen := make(map[string]struct{}, len(codes))
	for _, value := range codes {
		code := strings.ToUpper(strings.TrimSpace(value))
		if len(code) != 17 || code[8] != '-' {
			return nil, errors.New("invalid recovery code set")
		}
		for index := 0; index < len(code); index++ {
			if index == 8 {
				continue
			}
			character := code[index]
			if !((character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
				return nil, errors.New("invalid recovery code set")
			}
		}
		if _, exists := seen[code]; exists {
			return nil, errors.New("recovery codes must be unique")
		}
		seen[code] = struct{}{}
		normalized = append(normalized, code)
	}
	return normalized, nil
}

func (s *Server) setupIPAllowed(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	for _, cidr := range s.SetupAllowCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

type loginRequest struct {
	Password     string `json:"password"`
	TOTP         string `json:"totp"`
	RecoveryCode string `json:"recovery_code"`
}

func (s *Server) login(response http.ResponseWriter, request *http.Request) {
	var input loginRequest
	if !readJSON(response, request, &input) {
		return
	}
	admin, err := s.Store.Admin()
	if err != nil {
		writeError(response, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}
	limits, allowed, err := s.reserveAuthenticationAttempt(request, "login", admin.ID, 8, 20)
	if err != nil {
		writeError(response, http.StatusInternalServerError, errors.New("authentication rate limit is unavailable"))
		return
	}
	if !allowed {
		writeError(response, http.StatusTooManyRequests, errors.New("too many login attempts"))
		return
	}
	if !auth.VerifyPassword(admin.PasswordHash, input.Password) {
		writeError(response, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}
	method, validSecondFactor, err := s.verifyCurrentSecondFactor(admin, input.TOTP, input.RecoveryCode)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("verify administrator second factor", "error", err)
		}
		writeError(response, http.StatusInternalServerError, errors.New("two-factor authentication is unavailable"))
		return
	}
	if !validSecondFactor {
		writeError(response, http.StatusUnauthorized, errors.New("invalid second factor"))
		return
	}
	csrf, err := s.createAdminSession(response, request, admin.ID, method, "")
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	_ = s.Store.ClearAuthenticationAttempts(limits)
	s.audit(request, admin.ID, "login", "session", "", "method="+method)
	writeJSON(response, http.StatusOK, map[string]string{"csrf_token": csrf})
}

func (s *Server) createAdminSession(response http.ResponseWriter, request *http.Request, userID, method, authenticatorID string) (string, error) {
	token, err := auth.NewOpaqueToken(32)
	if err != nil {
		return "", err
	}
	csrf, err := auth.NewOpaqueToken(24)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if err := s.Store.CreateAuthenticatedSession(userID, token, csrf, method, authenticatorID, now, now.Add(recentAuthenticationLifetime), now.Add(adminSessionLifetime)); err != nil {
		return "", err
	}
	s.setAdminSessionCookie(response, request, token)
	return csrf, nil
}

func (s *Server) setAdminSessionCookie(response http.ResponseWriter, request *http.Request, token string) {
	http.SetCookie(response, &http.Cookie{
		Name: "cdn_session", Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookie(request),
		SameSite: http.SameSiteStrictMode, MaxAge: int(adminSessionLifetime.Seconds()),
	})
}

func (s *Server) secureCookie(request *http.Request) bool {
	if request.TLS != nil {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(s.ControlURL))
	return err == nil && strings.EqualFold(parsed.Scheme, "https")
}

func (s *Server) logout(response http.ResponseWriter, request *http.Request) {
	cookie, _ := request.Cookie("cdn_session")
	if cookie != nil {
		_ = s.Store.DeleteSession(cookie.Value)
	}
	http.SetCookie(response, &http.Cookie{Name: "cdn_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteStrictMode})
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) session(response http.ResponseWriter, request *http.Request) {
	session := currentAdminSession(request.Context())
	writeJSON(response, http.StatusOK, map[string]any{
		"user": adminID(request.Context()), "csrf_token": session.CSRFToken,
		"auth_method": session.AuthMethod, "authenticated_at": session.AuthenticatedAt,
		"elevated_until": session.ElevatedUntil,
	})
}

func (s *Server) listNodes(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.ReconcileNodeUpgrades(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	result := make([]nodeUpgradeStatusResponse, 0, len(nodes))
	for _, node := range nodes {
		status, err := s.buildNodeUpgradeStatus(node)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		result = append(result, status)
	}
	writeJSON(response, http.StatusOK, result)
}

type nodeRequest struct {
	Name       string `json:"name"`
	PublicIPv4 string `json:"public_ipv4"`
}

func (s *Server) createNode(response http.ResponseWriter, request *http.Request) {
	var input nodeRequest
	if !readJSON(response, request, &input) {
		return
	}
	node, err := s.Store.CreateNode(input.Name, input.PublicIPv4)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.audit(request, adminID(request.Context()), "create", "node", node.ID, node.Name)
	writeJSON(response, http.StatusCreated, node)
}

func (s *Server) createEnrollmentToken(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	digest := strings.TrimSpace(s.EdgeBinarySHA256)
	edgeControlURL := s.edgeControlURL()
	nginxTarget, nginxTargetErr := s.currentNginxArtifactTarget()
	if !validHTTPSURL(s.ControlURL) || !validHTTPSURL(edgeControlURL) || nginxTargetErr != nil || s.validateNodeUpgradeArtifacts() != nil {
		writeError(response, http.StatusConflict, errors.New("CONTROL_PUBLIC_URL, EDGE_CONTROL_URL, EDGE_BINARY_URL, and the current Nginx artifact must be valid before generating an enrollment command"))
		return
	}
	enrollmentRequired, err := s.Store.NodeRequiresEnrollment(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	bootstrapURL := strings.TrimRight(s.ControlURL, "/") + "/install-edge.sh"
	if nginxTarget.Path != "" && validSHA256Digest(nginxTarget.SHA256) {
		bootstrapURL = strings.TrimRight(s.ControlURL, "/") + "/downloads/nginx/" + nginxTarget.SHA256 + "/install-edge.sh"
	}
	serviceDigest := resourceSHA256(bootstrapEdgeService)
	updaterServiceDigest := resourceSHA256(bootstrapEdgeUpdaterService)
	result := map[string]any{"enrollment_required": enrollmentRequired}
	if enrollmentRequired {
		token, err := auth.NewOpaqueToken(32)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		expiresAt := time.Now().UTC().Add(15 * time.Minute)
		if err := s.Store.CreateEnrollmentToken(nodeID, token, expiresAt); err != nil {
			writeStoreError(response, err)
			return
		}
		s.audit(request, adminID(request.Context()), "create_enrollment_token", "node", nodeID, "expires "+expiresAt.Format(time.RFC3339))
		result["token"] = token
		result["expires_at"] = expiresAt
		result["install_command"] = fmt.Sprintf("curl -fsSL %q | sudo bash -s -- --control-url %q --enrollment-token %q --binary-url %q --binary-sha256 %q --service-sha256 %q --updater-service-sha256 %q", bootstrapURL, edgeControlURL, token, s.EdgeBinaryURL, digest, serviceDigest, updaterServiceDigest)
	} else {
		s.audit(request, adminID(request.Context()), "create_upgrade_command", "node", nodeID, "preserve existing mTLS identity")
		result["install_command"] = fmt.Sprintf("curl -fsSL %q | sudo bash -s -- --control-url %q --binary-url %q --binary-sha256 %q --service-sha256 %q --updater-service-sha256 %q", bootstrapURL, edgeControlURL, s.EdgeBinaryURL, digest, serviceDigest, updaterServiceDigest)
	}
	writeJSON(response, http.StatusCreated, result)
}

func validSHA256Digest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validHTTPSURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func (s *Server) bootstrapEdgeScript(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write([]byte(s.renderedEdgeInstaller()))
}

func (s *Server) bootstrapEdgeService(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write([]byte(bootstrapEdgeService))
}

func (s *Server) bootstrapEdgeUpdaterService(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write([]byte(bootstrapEdgeUpdaterService))
}

func (s *Server) bootstrapEdgeNginxService(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write([]byte(bootstrapEdgeNginxService))
}

func (s *Server) renderedEdgeInstaller() string {
	baseURL := strings.TrimRight(s.edgeControlURL(), "/")
	return renderBootstrapEdgeScript(
		strings.TrimSpace(s.NginxBundleURL), strings.TrimSpace(s.NginxBundleSHA256),
		baseURL+"/install-edge-nginx.service", resourceSHA256(bootstrapEdgeNginxService),
	)
}

func (s *Server) edgeBinary(response http.ResponseWriter, request *http.Request) {
	path := strings.TrimSpace(s.EdgeBinaryPath)
	info, err := os.Stat(path)
	if path == "" || err != nil || !info.Mode().IsRegular() {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Content-Disposition", "attachment; filename=cdn-edge-agent-linux-amd64")
	response.Header().Set("Cache-Control", "no-store")
	http.ServeFile(response, request, path)
}

func (s *Server) nginxBundle(response http.ResponseWriter, request *http.Request) {
	path := strings.TrimSpace(s.NginxBundlePath)
	info, err := os.Stat(path)
	if path == "" || err != nil || !info.Mode().IsRegular() {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "application/gzip")
	response.Header().Set("Content-Disposition", "attachment; filename=cdn-nginx-linux-amd64.tar.gz")
	response.Header().Set("Cache-Control", "no-store")
	http.ServeFile(response, request, path)
}

type statusRequest struct {
	Status domain.NodeStatus `json:"status"`
}

func (s *Server) setNodeStatus(response http.ResponseWriter, request *http.Request) {
	var input statusRequest
	if !readJSON(response, request, &input) {
		return
	}
	if input.Status != domain.NodeDraining && input.Status != domain.NodeRevoked && input.Status != domain.NodeActive {
		writeError(response, http.StatusBadRequest, errors.New("status must be active, draining, or revoked"))
		return
	}
	smartRoutingDisabled, err := s.Store.SetNodeStatusManual(request.PathValue("id"), input.Status)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	detail := string(input.Status)
	if smartRoutingDisabled {
		detail += "; smart routing disabled by manual takeover"
	}
	s.audit(request, adminID(request.Context()), "set_status", "node", request.PathValue("id"), detail)
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true, "smart_routing_disabled": smartRoutingDisabled})
}

func (s *Server) listSites(response http.ResponseWriter, request *http.Request) {
	sites, err := s.Store.ListSites()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, sites)
}

type originRequest struct {
	URL               string                    `json:"url"`
	HostHeader        string                    `json:"host_header"`
	TLSServerName     *string                   `json:"tls_server_name"`
	HTTPVersion       *domain.OriginHTTPVersion `json:"http_version"`
	WireGuardTunnelID *string                   `json:"wireguard_tunnel_id"`
	Enabled           bool                      `json:"enabled"`
}

type optionalNullableInt struct {
	Present bool
	Value   *int
}

func (value *optionalNullableInt) UnmarshalJSON(encoded []byte) error {
	value.Present = true
	if strings.TrimSpace(string(encoded)) == "null" {
		value.Value = nil
		return nil
	}
	var decoded int
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return err
	}
	value.Value = &decoded
	return nil
}

func (input originRequest) origin(current *domain.Origin) domain.Origin {
	tlsServerName := ""
	if input.TLSServerName != nil {
		tlsServerName = *input.TLSServerName
	} else if current != nil && strings.TrimSpace(input.URL) == current.URL {
		tlsServerName = current.TLSServerName
	}
	httpVersion := domain.OriginHTTPVersion("")
	if input.HTTPVersion != nil {
		httpVersion = *input.HTTPVersion
	} else if current != nil && strings.TrimSpace(input.URL) == current.URL {
		httpVersion = current.HTTPVersion
	}
	wireGuardTunnelID := ""
	if input.WireGuardTunnelID != nil {
		wireGuardTunnelID = *input.WireGuardTunnelID
	} else if current != nil {
		wireGuardTunnelID = current.WireGuardTunnelID
	}
	return domain.Origin{
		URL: input.URL, HostHeader: input.HostHeader, TLSServerName: tlsServerName,
		HTTPVersion: httpVersion, WireGuardTunnelID: wireGuardTunnelID, Enabled: input.Enabled,
	}
}

type siteRequest struct {
	Name                          string               `json:"name"`
	ZoneID                        string               `json:"zone_id"`
	Domains                       []string             `json:"domains"`
	NodeIDs                       []string             `json:"node_ids"`
	PrimaryOrigin                 originRequest        `json:"primary_origin"`
	BackupOrigin                  *originRequest       `json:"backup_origin"`
	StreamPaths                   *[]string            `json:"stream_paths"`
	Passthrough                   *bool                `json:"passthrough"`
	RequestBodyBuffering          *bool                `json:"request_body_buffering"`
	OriginResponseBuffering       *bool                `json:"origin_response_buffering"`
	DynamicCompressionEnabled     *bool                `json:"dynamic_compression_enabled"`
	CompressionExcludedMIMETypes  *[]string            `json:"compression_excluded_mime_types"`
	HTTP3Enabled                  *bool                `json:"http3_enabled"`
	ClientMaxBodySizeMB           *int                 `json:"client_max_body_size_mb"`
	ClientKeepaliveTimeoutSeconds *int                 `json:"client_keepalive_timeout_seconds"`
	ReadWriteTimeoutSeconds       *int                 `json:"read_write_timeout_seconds"`
	DNSTTLSeconds                 optionalNullableInt  `json:"dns_ttl_seconds"`
	TCPOnly                       *bool                `json:"tcp_only"`
	TCPForwards                   *[]domain.TCPForward `json:"tcp_forwards"`
	Enabled                       *bool                `json:"enabled"`
}

func (input siteRequest) site(id string, current *domain.Site) domain.Site {
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	var streamPaths []string
	if input.StreamPaths != nil {
		streamPaths = *input.StreamPaths
	}
	passthrough := false
	if input.Passthrough != nil {
		passthrough = *input.Passthrough
	}
	requestBodyBuffering := true
	if input.RequestBodyBuffering != nil {
		requestBodyBuffering = *input.RequestBodyBuffering
	}
	originResponseBuffering := true
	if input.OriginResponseBuffering != nil {
		originResponseBuffering = *input.OriginResponseBuffering
	}
	dynamicCompressionEnabled := true
	compressionExcludedMIMETypes := domain.DefaultCompressionExcludedMIMETypes()
	if current != nil {
		dynamicCompressionEnabled = current.DynamicCompressionEnabled
		compressionExcludedMIMETypes = append([]string(nil), current.CompressionExcludedMIMETypes...)
	}
	if input.DynamicCompressionEnabled != nil {
		dynamicCompressionEnabled = *input.DynamicCompressionEnabled
	}
	if input.CompressionExcludedMIMETypes != nil {
		compressionExcludedMIMETypes = append([]string(nil), (*input.CompressionExcludedMIMETypes)...)
	}
	http3Enabled := false
	if input.HTTP3Enabled != nil {
		http3Enabled = *input.HTTP3Enabled
	}
	clientMaxBodySizeMB := domain.DefaultClientMaxBodySizeMB
	if input.ClientMaxBodySizeMB != nil {
		clientMaxBodySizeMB = *input.ClientMaxBodySizeMB
	}
	clientKeepaliveTimeoutSeconds := domain.DefaultClientKeepaliveTimeoutSeconds
	if input.ClientKeepaliveTimeoutSeconds != nil {
		clientKeepaliveTimeoutSeconds = *input.ClientKeepaliveTimeoutSeconds
	}
	readWriteTimeoutSeconds := domain.DefaultReadWriteTimeoutSeconds
	if input.ReadWriteTimeoutSeconds != nil {
		readWriteTimeoutSeconds = *input.ReadWriteTimeoutSeconds
	}
	var currentPrimary *domain.Origin
	var currentBackup *domain.Origin
	if current != nil {
		currentPrimary = &current.PrimaryOrigin
		currentBackup = current.BackupOrigin
	}
	var dnsTTLSeconds *int
	if input.DNSTTLSeconds.Present {
		dnsTTLSeconds = input.DNSTTLSeconds.Value
	} else if current != nil && current.DNSTTLSeconds != nil {
		value := *current.DNSTTLSeconds
		dnsTTLSeconds = &value
	}
	var backupOrigin *domain.Origin
	if input.BackupOrigin != nil {
		backup := input.BackupOrigin.origin(currentBackup)
		backupOrigin = &backup
	}
	tcpOnly := false
	var tcpForwards []domain.TCPForward
	if current != nil {
		tcpOnly = current.TCPOnly
		tcpForwards = append([]domain.TCPForward(nil), current.TCPForwards...)
	}
	if input.TCPOnly != nil {
		tcpOnly = *input.TCPOnly
	}
	if input.TCPForwards != nil {
		tcpForwards = append([]domain.TCPForward(nil), (*input.TCPForwards)...)
	}
	var cacheInvalidations []domain.CacheInvalidationRule
	var cacheWarmups []domain.CacheWarmup
	if current != nil {
		cacheInvalidations = append(cacheInvalidations, current.CacheInvalidations...)
		cacheWarmups = append(cacheWarmups, current.CacheWarmups...)
	}
	return domain.Site{ID: id, Name: input.Name, Domains: input.Domains, Nodes: input.NodeIDs, PrimaryOrigin: input.PrimaryOrigin.origin(currentPrimary), BackupOrigin: backupOrigin, StreamPaths: streamPaths, Passthrough: passthrough, RequestBodyBuffering: requestBodyBuffering, OriginResponseBuffering: originResponseBuffering, DynamicCompressionEnabled: dynamicCompressionEnabled, CompressionExcludedMIMETypes: compressionExcludedMIMETypes, HTTP3Enabled: http3Enabled, ClientMaxBodySizeMB: clientMaxBodySizeMB, ClientKeepaliveTimeoutSeconds: clientKeepaliveTimeoutSeconds, ReadWriteTimeoutSeconds: readWriteTimeoutSeconds, DNSTTLSeconds: dnsTTLSeconds, TCPOnly: tcpOnly, TCPForwards: tcpForwards, CacheInvalidations: cacheInvalidations, CacheWarmups: cacheWarmups, Enabled: enabled}
}

func (input siteRequest) validateClientMaxBodySize() error {
	if input.ClientMaxBodySizeMB == nil {
		return nil
	}
	return domain.ValidateClientMaxBodySizeMB(*input.ClientMaxBodySizeMB)
}

func (input siteRequest) validateReadWriteTimeout() error {
	if input.ReadWriteTimeoutSeconds == nil {
		return nil
	}
	return domain.ValidateReadWriteTimeoutSeconds(*input.ReadWriteTimeoutSeconds)
}

func (input siteRequest) validateClientKeepaliveTimeout() error {
	if input.ClientKeepaliveTimeoutSeconds == nil {
		return nil
	}
	return domain.ValidateClientKeepaliveTimeoutSeconds(*input.ClientKeepaliveTimeoutSeconds)
}

func (s *Server) createSite(response http.ResponseWriter, request *http.Request) {
	var input siteRequest
	if !readJSON(response, request, &input) {
		return
	}
	if err := input.validateClientMaxBodySize(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := input.validateReadWriteTimeout(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := input.validateClientKeepaliveTimeout(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	siteInput := input.site("", nil)
	if err := domain.NormalizeAndValidateSite(&siteInput); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	zoneID := strings.TrimSpace(input.ZoneID)
	if zoneID == "" {
		if s.ZoneResolver == nil {
			writeError(response, http.StatusServiceUnavailable, errors.New("Cloudflare zone discovery is not configured"))
			return
		}
		var err error
		zoneID, err = s.ZoneResolver.ResolveZoneID(request.Context(), siteInput.Domains)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, integrations.ErrZoneNotFound) || errors.Is(err, integrations.ErrZoneMismatch) {
				status = http.StatusBadRequest
			}
			writeError(response, status, err)
			return
		}
	}
	site, err := s.Store.CreateSite(siteInput, zoneID)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.audit(request, adminID(request.Context()), "create", "site", site.ID, site.Name)
	if domain.SiteNeedsCertificate(site) && s.CertificateManager != nil {
		task, created, queueErr := s.CertificateManager.QueueIssue(site)
		if queueErr != nil {
			if s.Logger != nil {
				s.Logger.Warn("queue automatic site certificate issuance", "site_id", site.ID, "error", queueErr)
			}
		} else {
			detail := task.ID + " automatic"
			if !created {
				detail += " reused"
			}
			s.audit(request, adminID(request.Context()), "issue_certificate", "site", site.ID, detail)
		}
	}
	writeJSON(response, http.StatusCreated, site)
}

func (s *Server) updateSite(response http.ResponseWriter, request *http.Request) {
	var input siteRequest
	if !readJSON(response, request, &input) {
		return
	}
	current, currentZoneID, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if err := input.validateClientMaxBodySize(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := input.validateReadWriteTimeout(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := input.validateClientKeepaliveTimeout(); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	siteInput := input.site(request.PathValue("id"), &current)
	if input.Enabled == nil {
		siteInput.Enabled = current.Enabled
	}
	if input.Passthrough == nil {
		siteInput.Passthrough = current.Passthrough
	}
	if input.RequestBodyBuffering == nil {
		siteInput.RequestBodyBuffering = current.RequestBodyBuffering
	}
	if input.OriginResponseBuffering == nil {
		siteInput.OriginResponseBuffering = current.OriginResponseBuffering
	}
	if input.HTTP3Enabled == nil {
		siteInput.HTTP3Enabled = current.HTTP3Enabled
	}
	if input.ClientMaxBodySizeMB == nil {
		siteInput.ClientMaxBodySizeMB = current.ClientMaxBodySizeMB
	}
	if input.ClientKeepaliveTimeoutSeconds == nil {
		siteInput.ClientKeepaliveTimeoutSeconds = current.ClientKeepaliveTimeoutSeconds
	}
	if input.ReadWriteTimeoutSeconds == nil {
		siteInput.ReadWriteTimeoutSeconds = current.ReadWriteTimeoutSeconds
	}
	zoneID := strings.TrimSpace(input.ZoneID)
	if zoneID == "" {
		zoneID = currentZoneID
	}
	site, err := s.Store.UpdateSite(siteInput, zoneID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "update", "site", site.ID, site.Name)
	writeJSON(response, http.StatusOK, site)
}

func (s *Server) deleteSite(response http.ResponseWriter, request *http.Request) {
	if s.SiteDeleter == nil {
		writeError(response, http.StatusNotImplemented, errors.New("site deletion is not configured"))
		return
	}
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	var input struct {
		Confirmation string `json:"confirmation"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if input.Confirmation != site.Name {
		writeError(response, http.StatusBadRequest, errors.New("confirmation must exactly match the site name"))
		return
	}
	status, err := s.SiteDeleter.Start(request.Context(), site.ID, adminID(request.Context()), s.requestIP(request))
	if err != nil {
		if errors.Is(err, store.ErrSiteTaskActive) || errors.Is(err, store.ErrSiteDeleting) {
			writeError(response, http.StatusConflict, err)
			return
		}
		writeJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error(), "deletion": status})
		return
	}
	writeJSON(response, http.StatusAccepted, status)
}

func (s *Server) deleteSiteStatus(response http.ResponseWriter, request *http.Request) {
	if s.SiteDeleter == nil {
		writeError(response, http.StatusNotImplemented, errors.New("site deletion is not configured"))
		return
	}
	status, err := s.SiteDeleter.Status(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

func (s *Server) publishSite(response http.ResponseWriter, request *http.Request) {
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if site.Enabled && domain.SiteNeedsCertificate(site) {
		certificateTask, ready, err := s.certificateReadyForPublish(site)
		if err != nil {
			writeError(response, http.StatusServiceUnavailable, err)
			return
		}
		if !ready {
			s.audit(request, adminID(request.Context()), "issue_certificate", "site", site.ID, certificateTask.ID+" publish preflight")
			writeJSON(response, http.StatusConflict, map[string]any{
				"error":            "TLS certificate issuance is in progress; publish after it succeeds",
				"certificate_task": certificateTask,
			})
			return
		}
	}
	task, err := s.Publisher.PublishSite(site.ID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "publish", "site", site.ID, task.ID)
	writeJSON(response, http.StatusAccepted, task)
}

func (s *Server) certificateReadyForPublish(site domain.Site) (domain.DeploymentTask, bool, error) {
	task, err := s.Store.LatestCertificateTask(site.ID)
	if err == nil && task.Status == domain.TaskSucceeded {
		return task, true, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return domain.DeploymentTask{}, false, err
	}
	if s.CertificateManager == nil {
		return task, false, errors.New("certificate issuer is not configured")
	}
	task, _, err = s.CertificateManager.QueueIssue(site)
	return task, false, err
}

func (s *Server) publishStatus(response http.ResponseWriter, request *http.Request) {
	if _, _, err := s.Store.GetSite(request.PathValue("id")); err != nil {
		writeStoreError(response, err)
		return
	}
	status, err := s.Store.PublishStatus(request.PathValue("id"))
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, status)
}

type cacheInvalidationRequest struct {
	Scope        domain.CacheInvalidationScope `json:"scope"`
	Value        string                        `json:"value"`
	Prewarm      bool                          `json:"prewarm"`
	PrewarmPaths []string                      `json:"prewarm_paths"`
}

func (s *Server) invalidateCache(response http.ResponseWriter, request *http.Request) {
	input := cacheInvalidationRequest{Scope: domain.CacheInvalidationFull}
	if request.ContentLength != 0 {
		decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
	}
	if input.Scope == "" {
		input.Scope = domain.CacheInvalidationFull
	}
	value := ""
	var err error
	if input.Scope != domain.CacheInvalidationFull {
		value, err = domain.NormalizeCacheInvalidationTarget(input.Scope, input.Value)
		if err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
	} else if strings.TrimSpace(input.Value) != "" {
		writeError(response, http.StatusBadRequest, errors.New("full cache invalidation does not accept a target"))
		return
	}
	prewarmPaths, err := s.cachePrewarmPaths(request.Context(), request.PathValue("id"), input, value)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if cacheable, _ := cacheSiteEligibility(site); !cacheable {
		writeError(response, http.StatusConflict, store.ErrCacheDisabled)
		return
	}
	operation, task, err := s.Publisher.RunCacheInvalidation(store.CacheOperationInput{
		SiteID: request.PathValue("id"), Scope: input.Scope, Target: value, PrewarmPaths: prewarmPaths,
		Actor: adminID(request.Context()), RemoteAddr: s.requestIP(request),
	})
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "invalidate_cache", "cache_operation", operation.ID,
		fmt.Sprintf("site=%s scope=%s target=%q generation=%d prewarm_urls=%d task=%s", operation.SiteID,
			input.Scope, value, operation.CacheGeneration, len(prewarmPaths), task.ID))
	writeJSON(response, http.StatusAccepted, task)
}

func (s *Server) cachePrewarmPaths(ctx context.Context, siteID string, input cacheInvalidationRequest, target string) ([]string, error) {
	if !input.Prewarm {
		if len(input.PrewarmPaths) != 0 {
			return nil, errors.New("prewarm_paths requires prewarm=true")
		}
		return nil, nil
	}
	paths, err := domain.NormalizeCacheWarmupPaths(input.PrewarmPaths)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(paths)+1)
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	prepend := func(path string) error {
		if _, found := seen[path]; found {
			return nil
		}
		if len(paths) >= domain.MaxCacheWarmupPaths {
			return errors.New("too many cache prewarm URLs")
		}
		paths = append([]string{path}, paths...)
		seen[path] = struct{}{}
		return nil
	}
	switch input.Scope {
	case domain.CacheInvalidationURL:
		if err := prepend(target); err != nil {
			return nil, err
		}
	case domain.CacheInvalidationPrefix:
		if s.Logs != nil {
			if events, err := s.Logs.Recent(ctx, siteID, 500); err == nil {
				for _, event := range events {
					if (event.Method == http.MethodGet || event.Method == http.MethodHead) && event.Status >= 200 && event.Status < 400 && strings.HasPrefix(event.Path, target) {
						path, pathErr := domain.NormalizeCacheInvalidationTarget(domain.CacheInvalidationURL, event.Path)
						if pathErr != nil {
							continue
						}
						if _, found := seen[path]; found {
							continue
						}
						if len(paths) >= domain.MaxCacheWarmupPaths {
							break
						}
						paths = append(paths, path)
						seen[path] = struct{}{}
					}
				}
			}
		}
		if len(paths) == 0 {
			paths = append(paths, target)
		}
	case domain.CacheInvalidationFull:
		if len(paths) == 0 {
			return nil, errors.New("full-site prewarm requires at least one prewarm URL")
		}
	default:
		return nil, errors.New("cache invalidation scope must be full, url, or prefix")
	}
	if input.Scope == domain.CacheInvalidationPrefix {
		for _, path := range paths {
			parsed, _ := url.ParseRequestURI(path)
			if !strings.HasPrefix(parsed.Path, target) {
				return nil, errors.New("prefix prewarm URLs must be under the invalidated prefix")
			}
		}
	}
	return paths, nil
}

func (s *Server) issueCertificate(response http.ResponseWriter, request *http.Request) {
	if s.CertificateManager == nil {
		writeError(response, http.StatusNotImplemented, errors.New("certificate issuer is not configured"))
		return
	}
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if !domain.SiteNeedsCertificate(site) {
		writeError(response, http.StatusConflict, errors.New("site has no TLS listeners"))
		return
	}
	task, created, err := s.CertificateManager.QueueIssue(site)
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, err)
		return
	}
	detail := task.ID
	if !created {
		detail += " reused"
	}
	s.audit(request, adminID(request.Context()), "issue_certificate", "site", site.ID, detail)
	writeJSON(response, http.StatusAccepted, task)
}

func (s *Server) latestCertificateTask(response http.ResponseWriter, request *http.Request) {
	if _, _, err := s.Store.GetSite(request.PathValue("id")); err != nil {
		writeStoreError(response, err)
		return
	}
	task, err := s.Store.LatestCertificateTask(request.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(response, http.StatusOK, nil)
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, task)
}

type tlsStatusResponse struct {
	CertificateTask           *domain.DeploymentTask `json:"certificate_task"`
	PublishedAfterCertificate bool                   `json:"published_after_certificate"`
}

func (s *Server) tlsStatus(response http.ResponseWriter, request *http.Request) {
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	task, err := s.Store.LatestCertificateTask(site.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(response, http.StatusOK, tlsStatusResponse{})
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	status := tlsStatusResponse{CertificateTask: &task}
	if task.Status == domain.TaskSucceeded {
		published, err := s.Store.HasSuccessfulPublishAfter(site.ID, task.UpdatedAt)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		status.PublishedAfterCertificate = published
	}
	writeJSON(response, http.StatusOK, status)
}

type originAllowlistNode struct {
	NodeID     string `json:"node_id"`
	NodeName   string `json:"node_name"`
	IPv4CIDR   string `json:"ipv4_cidr"`
	Assignment string `json:"assignment"`
}

type originAllowlistResponse struct {
	SiteID    string                `json:"site_id"`
	IPv4CIDRs []string              `json:"ipv4_cidrs"`
	Nodes     []originAllowlistNode `json:"nodes"`
	Note      string                `json:"note"`
}

func (s *Server) originAllowlist(response http.ResponseWriter, request *http.Request) {
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	currentNodes := make(map[string]bool, len(site.Nodes))
	publishedNodes := make(map[string]bool)
	nodeIDs := make([]string, 0, len(site.Nodes))
	for _, nodeID := range site.Nodes {
		currentNodes[nodeID] = true
		nodeIDs = append(nodeIDs, nodeID)
	}
	publication, publicationErr := s.Store.SitePublication(site.ID)
	if publicationErr == nil {
		publishedNodes = make(map[string]bool, len(publication.Site.Nodes))
		for _, nodeID := range publication.Site.Nodes {
			publishedNodes[nodeID] = true
			if !currentNodes[nodeID] {
				nodeIDs = append(nodeIDs, nodeID)
			}
		}
	} else if !errors.Is(publicationErr, store.ErrNotFound) {
		writeStoreError(response, publicationErr)
		return
	}
	result := originAllowlistResponse{
		SiteID:    site.ID,
		IPv4CIDRs: make([]string, 0, len(nodeIDs)),
		Nodes:     make([]originAllowlistNode, 0, len(nodeIDs)),
		Note:      "源站防火墙或安全组需允许当前配置节点的 IPv4 CIDR。",
	}
	seenNodes := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if seenNodes[nodeID] {
			continue
		}
		seenNodes[nodeID] = true
		node, err := s.Store.GetNode(nodeID)
		if err != nil || node.Status == domain.NodeRevoked || node.Status == domain.NodeUninstalling || node.Status == domain.NodeUninstalled {
			continue
		}
		assignment := "current"
		if currentNodes[nodeID] && publishedNodes[nodeID] {
			assignment = "current_and_published"
		} else if publishedNodes[nodeID] {
			assignment = "published_only"
			result.Note = "发布完成前，源站防火墙需同时允许当前配置节点和待移除的已发布节点。"
		}
		cidr := node.PublicIPv4 + "/32"
		result.IPv4CIDRs = append(result.IPv4CIDRs, cidr)
		result.Nodes = append(result.Nodes, originAllowlistNode{
			NodeID: node.ID, NodeName: node.Name, IPv4CIDR: cidr, Assignment: assignment,
		})
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) getTask(response http.ResponseWriter, request *http.Request) {
	task, err := s.Store.GetTask(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, task)
}

func (s *Server) siteLogs(response http.ResponseWriter, request *http.Request) {
	if s.Logs == nil {
		writeJSON(response, http.StatusOK, []domain.AccessLogEvent{})
		return
	}
	events, err := s.Logs.Recent(request.Context(), request.PathValue("id"), 100)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	writeJSON(response, http.StatusOK, events)
}

const logSearchPageSize = 20

type logSearchResponse struct {
	Logs     []domain.AccessLogEvent `json:"logs"`
	From     time.Time               `json:"from"`
	To       time.Time               `json:"to"`
	Offset   int                     `json:"offset"`
	PageSize int                     `json:"page_size"`
	HasMore  bool                    `json:"has_more"`
}

func (s *Server) searchLogs(response http.ResponseWriter, request *http.Request) {
	query, err := parseLogSearchQuery(request, time.Now().UTC())
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	result := logstore.LogPage{Events: []domain.AccessLogEvent{}}
	if s.Logs != nil {
		result, err = s.Logs.Search(request.Context(), query)
		if err != nil {
			writeError(response, http.StatusBadGateway, err)
			return
		}
	}
	if result.Events == nil {
		result.Events = []domain.AccessLogEvent{}
	}
	writeJSON(response, http.StatusOK, logSearchResponse{
		Logs: result.Events, From: query.From, To: query.To, Offset: query.Offset,
		PageSize: logSearchPageSize, HasMore: result.HasMore,
	})
}

func (s *Server) getLog(response http.ResponseWriter, request *http.Request) {
	id := strings.TrimSpace(request.PathValue("id"))
	if !validLogEntryID(id) {
		writeError(response, http.StatusBadRequest, errors.New("log entry ID is invalid"))
		return
	}
	if s.Logs == nil {
		writeError(response, http.StatusNotFound, logstore.ErrNotFound)
		return
	}
	event, err := s.Logs.Get(request.Context(), id)
	if errors.Is(err, logstore.ErrNotFound) {
		writeError(response, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	writeJSON(response, http.StatusOK, event)
}

func validLogEntryID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func parseLogSearchQuery(request *http.Request, now time.Time) (logstore.LogQuery, error) {
	values := request.URL.Query()
	to := now.UTC()
	var err error
	if raw := strings.TrimSpace(values.Get("to")); raw != "" {
		to, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return logstore.LogQuery{}, errors.New("to must be an RFC3339 timestamp")
		}
		to = to.UTC()
	}
	from := to.Add(-time.Hour)
	if raw := strings.TrimSpace(values.Get("from")); raw != "" {
		from, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return logstore.LogQuery{}, errors.New("from must be an RFC3339 timestamp")
		}
		from = from.UTC()
	}
	if !from.Before(to) {
		return logstore.LogQuery{}, errors.New("from must be earlier than to")
	}
	if to.Sub(from) > 7*24*time.Hour {
		return logstore.LogQuery{}, errors.New("log search range cannot exceed 7 days")
	}

	offset := 0
	if raw := strings.TrimSpace(values.Get("offset")); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return logstore.LogQuery{}, errors.New("offset must be a non-negative integer")
		}
	}
	statusMin, statusMax, err := parseLogStatusFilter(values.Get("status"))
	if err != nil {
		return logstore.LogQuery{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(values.Get("method")))
	if method != "" && !validLogMethod(method) {
		return logstore.LogQuery{}, errors.New("method must be a valid HTTP method token")
	}
	clientIP := strings.TrimSpace(values.Get("client_ip"))
	if clientIP != "" {
		ip := net.ParseIP(clientIP)
		if ip == nil {
			return logstore.LogQuery{}, errors.New("client_ip must be a valid IP address")
		}
		clientIP = ip.String()
	}
	cacheStatus := strings.ToUpper(strings.TrimSpace(values.Get("cache_status")))
	if cacheStatus != "" && !validCacheStatus(cacheStatus) {
		return logstore.LogQuery{}, errors.New("cache_status is not supported")
	}
	requestID := strings.TrimSpace(values.Get("request_id"))
	if requestID != "" && !validLogTraceID(requestID) {
		return logstore.LogQuery{}, errors.New("request_id must be a visible ASCII value no longer than 256 characters")
	}
	siteID := strings.TrimSpace(values.Get("site_id"))
	nodeID := strings.TrimSpace(values.Get("node_id"))
	path := strings.TrimSpace(values.Get("path"))
	if len(siteID) > 128 || len(nodeID) > 128 {
		return logstore.LogQuery{}, errors.New("site_id and node_id must not exceed 128 characters")
	}
	if len(path) > 512 {
		return logstore.LogQuery{}, errors.New("path search must not exceed 512 characters")
	}
	return logstore.LogQuery{
		From: from, To: to, RequestID: requestID, SiteID: siteID, NodeID: nodeID, Method: method,
		StatusMin: statusMin, StatusMax: statusMax, Path: path, ClientIP: clientIP,
		CacheStatus: cacheStatus, Offset: offset, Limit: logSearchPageSize,
	}, nil
}

func validLogTraceID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

func parseLogStatusFilter(value string) (uint16, uint16, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, 0, nil
	}
	if len(value) == 3 && value[1:] == "xx" && value[0] >= '1' && value[0] <= '5' {
		minimum := uint16(value[0]-'0') * 100
		return minimum, minimum + 99, nil
	}
	status, err := strconv.Atoi(value)
	if err != nil || status < 100 || status > 599 {
		return 0, 0, errors.New("status must be an HTTP status code or a class such as 4xx")
	}
	return uint16(status), uint16(status), nil
}

func validLogMethod(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

func validCacheStatus(value string) bool {
	switch value {
	case "HIT", "MISS", "BYPASS", "EXPIRED", "STALE", "UPDATING", "REVALIDATED":
		return true
	default:
		return false
	}
}

func (s *Server) siteMetrics(response http.ResponseWriter, request *http.Request) {
	if s.Logs == nil {
		writeJSON(response, http.StatusOK, []logstore.MinuteMetric{})
		return
	}
	metrics, err := s.Logs.Metrics(request.Context(), request.PathValue("id"), time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	writeJSON(response, http.StatusOK, metrics)
}

type enrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	CSR             string `json:"csr"`
}

func (s *Server) enroll(response http.ResponseWriter, request *http.Request) {
	if s.CA == nil {
		writeError(response, http.StatusServiceUnavailable, errors.New("internal CA is not configured"))
		return
	}
	var input enrollRequest
	if !readJSON(response, request, &input) {
		return
	}
	if _, err := ParseAndVerifyCSR([]byte(input.CSR)); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	nodeID, err := s.Store.ConsumeEnrollmentToken(input.EnrollmentToken)
	if err != nil {
		writeError(response, http.StatusUnauthorized, store.ErrTokenInvalid)
		return
	}
	certificate, err := s.CA.SignCSR([]byte(input.CSR), nodeID)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	fingerprint, err := CertificateFingerprintPEM(certificate)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.SetNodeCertificate(nodeID, fingerprint); err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, "edge:"+nodeID, "enroll", "node", nodeID, fingerprint)
	writeJSON(response, http.StatusCreated, map[string]string{"node_id": nodeID, "client_certificate": string(certificate), "ca_certificate": string(s.CA.CertificatePEM)})
}

func (s *Server) renew(response http.ResponseWriter, request *http.Request) {
	if s.CA == nil {
		writeError(response, http.StatusServiceUnavailable, errors.New("internal CA is not configured"))
		return
	}
	var input struct {
		CSR string `json:"csr"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if _, err := ParseAndVerifyCSR([]byte(input.CSR)); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	certificate, err := s.CA.SignRenewal(request.TLS.PeerCertificates[0].Raw, []byte(input.CSR), edgeNodeID(request.Context()))
	if err != nil {
		writeError(response, http.StatusUnauthorized, err)
		return
	}
	fingerprint, err := CertificateFingerprintPEM(certificate)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.SetNodeCertificate(edgeNodeID(request.Context()), fingerprint); err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, "edge:"+edgeNodeID(request.Context()), "renew", "node", edgeNodeID(request.Context()), fingerprint)
	writeJSON(response, http.StatusOK, map[string]string{"client_certificate": string(certificate), "ca_certificate": string(s.CA.CertificatePEM)})
}

func (s *Server) desiredState(response http.ResponseWriter, request *http.Request) {
	nodeID := edgeNodeID(request.Context())
	desiredVersion, err := s.Store.DesiredVersion(nodeID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	revision := "desired-" + strconv.FormatInt(desiredVersion, 10)
	if requestHasRevision(request, revision) {
		writeRevisionNotModified(response, revision)
		return
	}
	response.Header().Set("ETag", revisionETag(revision))
	state, encryptedCertificates, err := s.Store.NodeState(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(response, http.StatusOK, domain.DesiredState{Version: 0, NginxConfig: ""})
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if len(encryptedCertificates) > 0 {
		plaintext, err := s.Cipher.Decrypt(encryptedCertificates)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		if err := json.Unmarshal(plaintext, &state.Certificates); err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(response, http.StatusOK, state)
}

type heartbeatRequest struct {
	LastError          string                     `json:"last_error"`
	AppliedVersion     int64                      `json:"applied_version"`
	ApplyReport        *domain.ApplyReport        `json:"apply_report,omitempty"`
	CacheWarmupResults []domain.CacheWarmupResult `json:"cache_warmup_results,omitempty"`
	Capabilities       []string                   `json:"capabilities,omitempty"`
	AgentVersion       string                     `json:"agent_version,omitempty"`
	AgentSHA256        string                     `json:"agent_sha256,omitempty"`
	NginxVersion       string                     `json:"nginx_version,omitempty"`
	NginxSHA256        string                     `json:"nginx_sha256,omitempty"`
	ActiveUpgradeID    string                     `json:"active_upgrade_task_id,omitempty"`
	CacheStorage       *domain.CacheStorageUsage  `json:"cache_storage,omitempty"`
	MachineStatus      *domain.MachineStatus      `json:"machine_status,omitempty"`
}

const maxEdgeReportClockSkew = 5 * time.Minute

func (s *Server) heartbeat(response http.ResponseWriter, request *http.Request) {
	var input heartbeatRequest
	if !readJSON(response, request, &input) {
		return
	}
	nodeID := edgeNodeID(request.Context())
	input.AgentVersion = strings.TrimSpace(input.AgentVersion)
	input.AgentSHA256 = strings.ToLower(strings.TrimSpace(input.AgentSHA256))
	input.NginxVersion = strings.TrimSpace(input.NginxVersion)
	input.NginxSHA256 = strings.ToLower(strings.TrimSpace(input.NginxSHA256))
	input.ActiveUpgradeID = strings.TrimSpace(input.ActiveUpgradeID)
	if len(input.AgentVersion) > 64 || strings.ContainsAny(input.AgentVersion, "\x00\r\n") {
		writeError(response, http.StatusBadRequest, errors.New("agent_version is invalid"))
		return
	}
	if input.AgentSHA256 != "" && !validSHA256Digest(input.AgentSHA256) {
		writeError(response, http.StatusBadRequest, errors.New("agent_sha256 must be a 64-character hexadecimal digest"))
		return
	}
	if (input.NginxVersion != "" && !nginxVersionPattern.MatchString(input.NginxVersion)) ||
		len(input.NginxVersion) > 64 || strings.ContainsAny(input.NginxVersion, "\x00\r\n") {
		writeError(response, http.StatusBadRequest, errors.New("nginx_version is invalid"))
		return
	}
	if input.NginxSHA256 != "" && !validSHA256Digest(input.NginxSHA256) {
		writeError(response, http.StatusBadRequest, errors.New("nginx_sha256 must be a 64-character hexadecimal digest"))
		return
	}
	if (input.NginxVersion == "") != (input.NginxSHA256 == "") {
		writeError(response, http.StatusBadRequest, errors.New("nginx_version and nginx_sha256 must be reported together"))
		return
	}
	if input.ActiveUpgradeID != "" && !validNodeUpgradeTaskID(input.ActiveUpgradeID) {
		writeError(response, http.StatusBadRequest, errors.New("active_upgrade_task_id is invalid"))
		return
	}
	if input.CacheStorage != nil && !domain.ValidCacheStorageUsage(*input.CacheStorage) {
		writeError(response, http.StatusBadRequest, errors.New("cache_storage is invalid"))
		return
	}
	if input.CacheStorage != nil {
		input.CacheStorage.CollectedAt = input.CacheStorage.CollectedAt.UTC()
		if input.CacheStorage.CollectedAt.After(time.Now().UTC().Add(maxEdgeReportClockSkew)) {
			input.CacheStorage = nil
		}
	}
	if input.MachineStatus != nil && !domain.ValidMachineStatus(*input.MachineStatus) {
		writeError(response, http.StatusBadRequest, errors.New("machine_status is invalid"))
		return
	}
	if input.MachineStatus != nil {
		input.MachineStatus.CollectedAt = input.MachineStatus.CollectedAt.UTC()
		if input.MachineStatus.CollectedAt.After(time.Now().UTC().Add(maxEdgeReportClockSkew)) {
			input.MachineStatus = nil
		}
	}
	cacheWarmupResults, err := domain.NormalizeCacheWarmupResults(input.CacheWarmupResults)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	for _, result := range cacheWarmupResults {
		if result.CompletedAt.After(time.Now().UTC().Add(maxEdgeReportClockSkew)) {
			writeError(response, http.StatusBadRequest, errors.New("cache prewarm result timestamp is in the future"))
			return
		}
	}
	if err := s.Store.SetNodeCapabilities(nodeID, input.Capabilities); err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.Store.HeartbeatWithArtifacts(nodeID, input.AppliedVersion, input.LastError, input.ApplyReport,
		input.AgentVersion, input.AgentSHA256, input.NginxVersion, input.NginxSHA256, input.ActiveUpgradeID); err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.Store.RecordCacheWarmupResults(nodeID, cacheWarmupResults); err != nil {
		writeStoreError(response, err)
		return
	}
	if err := s.reconcileEdgeRuntimeCapabilities(nodeID, input.Capabilities); err != nil && s.Logger != nil {
		s.Logger.Warn("reconcile edge runtime capabilities", "node_id", nodeID, "error", err)
	}
	if input.CacheStorage != nil {
		if err := s.Store.RecordNodeCacheStorage(nodeID, *input.CacheStorage); err != nil {
			writeStoreError(response, err)
			return
		}
	}
	if input.MachineStatus != nil {
		s.recordNodeMachineStatus(nodeID, *input.MachineStatus)
	}
	result := domain.EdgeHeartbeatResponse{OK: true}
	if slices.Contains(input.Capabilities, domain.EdgeCapabilityControlManifest) {
		manifest, err := s.edgeControlManifest(nodeID)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		result.Control = &manifest
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) writeLogs(response http.ResponseWriter, request *http.Request) {
	if s.Logs == nil {
		writeJSON(response, http.StatusAccepted, map[string]bool{"ok": true})
		return
	}
	const maximumLogPayload = 8 << 20
	wirePayload, err := io.ReadAll(io.LimitReader(request.Body, maximumLogPayload+1))
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if len(wirePayload) > maximumLogPayload {
		writeError(response, http.StatusRequestEntityTooLarge, errors.New("compressed log payload exceeds 8 MiB"))
		return
	}
	payload := wirePayload
	switch strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(wirePayload))
		if err != nil {
			writeError(response, http.StatusBadRequest, errors.New("invalid gzip log payload"))
			return
		}
		payload, err = io.ReadAll(io.LimitReader(reader, maximumLogPayload+1))
		closeErr := reader.Close()
		if err != nil || closeErr != nil {
			writeError(response, http.StatusBadRequest, errors.New("invalid gzip log payload"))
			return
		}
		if len(payload) > maximumLogPayload {
			writeError(response, http.StatusRequestEntityTooLarge, errors.New("decompressed log payload exceeds 8 MiB"))
			return
		}
	default:
		writeError(response, http.StatusUnsupportedMediaType, errors.New("unsupported log content encoding"))
		return
	}
	var events []domain.AccessLogEvent
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&events); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if len(events) > 500 {
		writeError(response, http.StatusRequestEntityTooLarge, errors.New("a log batch may contain at most 500 events"))
		return
	}
	nodeID := edgeNodeID(request.Context())
	sites, err := s.Store.ListSites()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	allowedSites := make(map[string]struct{})
	allowAssigned := func(site domain.Site) {
		for _, assignedNodeID := range site.Nodes {
			if assignedNodeID == nodeID {
				allowedSites[site.ID] = struct{}{}
				break
			}
		}
	}
	for _, site := range sites {
		allowAssigned(site)
	}
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	for _, publication := range publications {
		allowAssigned(publication.Site)
	}
	accepted := events[:0]
	for index := range events {
		if _, allowed := allowedSites[events[index].SiteID]; !allowed {
			continue
		}
		events[index].NodeID = nodeID
		events[index].ID = strings.TrimSpace(events[index].ID)
		if !validLogEntryID(events[index].ID) {
			events[index].ID = uuid.NewString()
		}
		events[index].Path = strings.SplitN(events[index].Path, "?", 2)[0]
		events[index].Path = truncateLogValue(events[index].Path, 4096)
		events[index].Host = truncateLogValue(events[index].Host, 255)
		events[index].Scheme = truncateLogValue(events[index].Scheme, 16)
		events[index].Protocol = truncateLogValue(events[index].Protocol, 32)
		events[index].Upstream = truncateLogValue(events[index].Upstream, 1024)
		events[index].UpstreamStatus = truncateLogValue(events[index].UpstreamStatus, 256)
		events[index].UpstreamResponseTime = truncateLogValue(events[index].UpstreamResponseTime, 256)
		events[index].UserAgent = truncateLogValue(events[index].UserAgent, 4096)
		events[index].Referer = truncateLogValue(events[index].Referer, 4096)
		events[index].ContentType = truncateLogValue(events[index].ContentType, 1024)
		events[index].ResponseContentType = truncateLogValue(events[index].ResponseContentType, 1024)
		events[index].Accept = truncateLogValue(events[index].Accept, 2048)
		events[index].Range = truncateLogValue(events[index].Range, 1024)
		if events[index].Timestamp.IsZero() {
			events[index].Timestamp = time.Now().UTC()
		}
		accepted = append(accepted, events[index])
	}
	if err := s.Logs.Append(request.Context(), accepted); err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]int{"accepted": len(accepted)})
}

func truncateLogValue(value string, maximum int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maximum {
		return string(runes[:maximum])
	}
	return value
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		cookie, err := request.Cookie("cdn_session")
		if err != nil {
			writeError(response, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		session, err := s.Store.Session(cookie.Value)
		if err != nil {
			writeError(response, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions {
			if request.Header.Get("X-CSRF-Token") == "" || request.Header.Get("X-CSRF-Token") != session.CSRFToken {
				writeError(response, http.StatusForbidden, errors.New("invalid CSRF token"))
				return
			}
		}
		ctx := context.WithValue(request.Context(), adminContextKey{}, session.UserID)
		ctx = context.WithValue(ctx, adminSessionContextKey{}, session)
		next(response, request.WithContext(ctx))
	}
}

func (s *Server) requireRecentAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAdmin(func(response http.ResponseWriter, request *http.Request) {
		if !sessionRecentlyAuthenticated(currentAdminSession(request.Context()), time.Now()) {
			writeJSON(response, http.StatusPreconditionRequired, map[string]string{
				"error": "recent authentication is required", "code": "reauthentication_required",
			})
			return
		}
		next(response, request)
	})
}

func (s *Server) requireEdge(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			writeError(response, http.StatusUnauthorized, errors.New("mTLS client certificate required"))
			return
		}
		fingerprint := CertificateFingerprintDER(request.TLS.PeerCertificates[0].Raw)
		nodeID, err := s.Store.NodeIDByFingerprint(fingerprint)
		if err != nil {
			writeError(response, http.StatusUnauthorized, errors.New("edge certificate is not authorized"))
			return
		}
		next(response, request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, nodeID)))
	}
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Referrer-Policy", "same-origin")
		response.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(response, request)
	})
}

func (s *Server) withAPICacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			response.Header().Set("Cache-Control", "no-store, private")
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		loggedResponse := &requestLogResponseWriter{ResponseWriter: response}
		next.ServeHTTP(loggedResponse, request)
		if s.Logger != nil {
			status := loggedResponse.statusCode()
			level := slog.LevelInfo
			message := "request"
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
				message = "request failed"
			} else if status >= http.StatusBadRequest {
				level = slog.LevelWarn
				message = "request rejected"
			}
			s.Logger.Log(request.Context(), level, message,
				"method", request.Method,
				"path", request.URL.Path,
				"remote", s.requestIP(request),
				"status", status,
				"response_bytes", loggedResponse.bytes,
				"duration", time.Since(started).String(),
			)
		}
	})
}

type requestLogResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *requestLogResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestLogResponseWriter) Write(contents []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(contents)
	w.bytes += written
	return written, err
}

func (w *requestLogResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *requestLogResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (s *Server) audit(request *http.Request, actor, action, resourceType, resourceID, detail string) {
	_ = s.Store.Audit(actor, action, resourceType, resourceID, s.requestIP(request), detail)
}

type adminContextKey struct{}
type adminSessionContextKey struct{}
type edgeContextKey struct{}

func adminID(context context.Context) string {
	value, _ := context.Value(adminContextKey{}).(string)
	return value
}
func currentAdminSession(context context.Context) store.Session {
	value, _ := context.Value(adminSessionContextKey{}).(store.Session)
	return value
}
func edgeNodeID(context context.Context) string {
	value, _ := context.Value(edgeContextKey{}).(string)
	return value
}
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func (s *Server) edgeControlURL() string {
	if value := strings.TrimRight(strings.TrimSpace(s.EdgeControlURL), "/"); value != "" {
		return value
	}
	return strings.TrimRight(strings.TrimSpace(s.ControlURL), "/")
}

func (s *Server) requestIP(request *http.Request) string {
	peer := remoteIP(request.RemoteAddr)
	parsedPeer := net.ParseIP(peer)
	if parsedPeer == nil || !s.isTrustedProxy(parsedPeer) {
		return peer
	}
	if forwarded := net.ParseIP(strings.TrimSpace(request.Header.Get("X-Real-IP"))); forwarded != nil {
		return forwarded.String()
	}
	return peer
}

func (s *Server) isTrustedProxy(address net.IP) bool {
	for _, cidr := range s.TrustedProxyCIDRs {
		if cidr.Contains(address) {
			return true
		}
	}
	return false
}
func newRecoveryCodes(count int) ([]string, error) {
	codes := make([]string, 0, count)
	for range count {
		code, err := auth.NewRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

func readJSON(response http.ResponseWriter, request *http.Request, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return false
	}
	return true
}
func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
func writeError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]string{"error": err.Error()})
}
func writeStoreError(response http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(response, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, store.ErrCacheDisabled) || errors.Is(err, store.ErrCacheOperationNotRetryable) || errors.Is(err, store.ErrUninstallActive) || errors.Is(err, store.ErrUninstallNotActive) || errors.Is(err, store.ErrNodeAssigned) || errors.Is(err, store.ErrSiteDeleting) || errors.Is(err, store.ErrSiteTaskActive) || errors.Is(err, store.ErrSiteChanged) || errors.Is(err, store.ErrNodeUpgradeActive) || errors.Is(err, store.ErrNodeOperationActive) || errors.Is(err, store.ErrUpgradeRetryNotReady) || errors.Is(err, store.ErrMonitoringTargetExists) || errors.Is(err, store.ErrMonitoringTargetNameExists) || errors.Is(err, store.ErrMonitoringTargetLimit) || errors.Is(err, store.ErrMonitoringTargetsChanged) || errors.Is(err, store.ErrWireGuardTunnelInUse) || errors.Is(err, store.ErrWireGuardPerformanceActive) {
		writeError(response, http.StatusConflict, err)
		return
	}
	writeError(response, http.StatusBadRequest, err)
}
