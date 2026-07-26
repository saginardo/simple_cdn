package edge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/version"
)

type Config struct {
	ControlURL            string
	EnrollmentToken       string
	StateDir              string
	NginxConfigPath       string
	NginxStreamConfigPath string
	NginxMainConfigPath   string
	NginxEventsConfigPath string
	CertificateDir        string
	ClientKeyPath         string
	ClientCertPath        string
	CAPath                string
	AccessLogPath         string
	SecurityLogPath       string
	Capabilities          []string
	AgentVersion          string
	AgentSHA256           string
	PollInterval          time.Duration
	ListenerSettleTimeout time.Duration
	ListenerPollInterval  time.Duration
	HTTPClient            *http.Client
	ArtifactHTTPClient    *http.Client
	Runner                Runner
	UpgradeRunner         UpgradeRunner
	SecurityFirewall      SecurityFirewall
	SecurityPollInterval  time.Duration
	MonitoringDialer      MonitoringDialer
	MonitoringTimeout     time.Duration
	MonitoringAttempts    int
	MonitoringWorkers     int
}

type Agent struct {
	Config        Config
	logs          *LogForwarder
	security      *SecurityManager
	cacheUsage    *cacheUsageCollector
	machineStatus machineStatusReporter

	statusMu          sync.Mutex
	lastApplyReport   *domain.ApplyReport
	applyReportSeq    uint64
	componentFailures map[string]string

	manifestMu sync.RWMutex
	manifest   *domain.EdgeControlManifest

	monitoringMu       sync.RWMutex
	monitoringTargets  []domain.MonitoringTarget
	monitoringRevision string
	monitoringLoaded   bool

	clientMu          sync.Mutex
	controlClient     *http.Client
	controlTransport  *http.Transport
	artifactMu        sync.Mutex
	artifactClient    *http.Client
	artifactTransport *http.Transport
}

func New(config Config) (*Agent, error) {
	parsedControlURL, err := url.Parse(strings.TrimSpace(config.ControlURL))
	if err != nil || parsedControlURL.Scheme != "https" || parsedControlURL.Host == "" || parsedControlURL.User != nil || parsedControlURL.Fragment != "" {
		return nil, errors.New("CONTROL_URL must be an absolute HTTPS URL")
	}
	config.ControlURL = strings.TrimRight(config.ControlURL, "/")
	if config.StateDir == "" {
		config.StateDir = "/opt/cdn-edge/data"
	}
	if config.NginxConfigPath == "" {
		config.NginxConfigPath = "/opt/cdn-edge/config/nginx/cdn-platform.conf"
	}
	if config.NginxStreamConfigPath == "" {
		config.NginxStreamConfigPath = filepath.Join(filepath.Dir(config.NginxConfigPath), "cdn-platform-stream.conf")
	}
	if config.NginxMainConfigPath == "" {
		config.NginxMainConfigPath = filepath.Join(filepath.Dir(config.NginxConfigPath), "cdn-platform-main.conf")
	}
	if config.NginxEventsConfigPath == "" {
		config.NginxEventsConfigPath = filepath.Join(filepath.Dir(config.NginxConfigPath), "cdn-platform-events.conf")
	}
	if config.CertificateDir == "" {
		config.CertificateDir = "/opt/cdn-edge/config/certs"
	}
	if config.ClientKeyPath == "" {
		config.ClientKeyPath = filepath.Join(config.StateDir, "edge-client.key")
	}
	if config.ClientCertPath == "" {
		config.ClientCertPath = filepath.Join(config.StateDir, "edge-client.crt")
	}
	if config.CAPath == "" {
		config.CAPath = filepath.Join(config.StateDir, "edge-ca.crt")
	}
	if config.AccessLogPath == "" {
		config.AccessLogPath = "/opt/cdn-edge/logs/access.json"
	}
	if config.SecurityLogPath == "" {
		config.SecurityLogPath = "/opt/cdn-edge/logs/security.json"
	}
	if config.PollInterval == 0 {
		config.PollInterval = 30 * time.Second
	}
	if config.ListenerSettleTimeout <= 0 {
		config.ListenerSettleTimeout = 5 * time.Second
	}
	if config.ListenerPollInterval <= 0 {
		config.ListenerPollInterval = 100 * time.Millisecond
	}
	if config.Runner == nil {
		config.Runner = NginxRunner{}
	}
	if config.UpgradeRunner == nil {
		config.UpgradeRunner = SystemdUpgradeRunner{}
	}
	if strings.TrimSpace(config.AgentSHA256) == "" {
		config.AgentSHA256, err = executableSHA256()
		if err != nil {
			return nil, fmt.Errorf("calculate edge agent digest: %w", err)
		}
	}
	if strings.TrimSpace(config.AgentVersion) == "" {
		config.AgentVersion = version.Version
	}
	config.AgentVersion = strings.TrimSpace(config.AgentVersion)
	config.AgentSHA256 = strings.ToLower(strings.TrimSpace(config.AgentSHA256))
	config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilityOnlineUpgrade)
	config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilityCacheUsage)
	config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilityMachineStatus)
	config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilityNginxFragments)
	config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilityControlManifest)
	configureMonitoring(&config)
	config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilityTCPMonitoring)
	config.SecurityFirewall = defaultSecurityFirewall(config.SecurityFirewall)
	if config.SecurityFirewall != nil {
		config.Capabilities = appendCapability(config.Capabilities, domain.EdgeCapabilitySecurity)
	}
	if err := os.MkdirAll(config.StateDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config.CertificateDir, 0o700); err != nil {
		return nil, err
	}
	agent := &Agent{
		Config: config, logs: NewLogForwarder(config.StateDir, config.AccessLogPath),
		cacheUsage:        newCacheUsageCollector(nginx.DefaultCachePath, nginx.DefaultCacheMaxBytes, defaultCacheUsageInterval),
		machineStatus:     newMachineStatusCollector(),
		componentFailures: make(map[string]string),
	}
	if config.SecurityFirewall != nil {
		agent.security = NewSecurityManager(config.StateDir, config.SecurityLogPath, config.SecurityPollInterval, config.SecurityFirewall)
	}
	return agent, nil
}

func (a *Agent) Run(ctx context.Context) error {
	defer a.resetControlClient()
	defer a.resetArtifactClient()
	if err := a.EnsureEnrollment(ctx); err != nil {
		return err
	}
	var group sync.WaitGroup
	start := func(run func()) {
		group.Add(1)
		go func() {
			defer group.Done()
			run()
		}()
	}
	start(func() { a.cacheUsage.Run(ctx) })
	start(func() { a.logs.Run(ctx, a.Config.ControlURL, a.client) })
	if a.security != nil {
		start(func() { a.security.Run(ctx, a.Config.ControlURL, a.client) })
	}
	start(func() { a.runPeriodic(ctx, "configuration", a.runConfigurationRound) })
	start(func() { a.runPeriodic(ctx, "monitoring", a.runMonitoringRound) })
	start(func() { a.runPeriodic(ctx, "upgrade", a.runUpgradeRound) })
	err := a.runHeartbeatLoop(ctx)
	group.Wait()
	return err
}

func (a *Agent) runHeartbeatLoop(ctx context.Context) error {
	first := true
	for {
		report, reportSeq := a.heartbeatReport()
		if err := a.Heartbeat(ctx, a.appliedVersion(), a.heartbeatError(), report); err == nil {
			a.acknowledgeApplyReport(reportSeq, report != nil)
			_ = a.markUpgradeReady()
		}
		delay := a.Config.PollInterval
		if first {
			delay += a.stableJitter("heartbeat")
			first = false
		}
		if !waitContext(ctx, delay) {
			return ctx.Err()
		}
	}
}

func (a *Agent) runPeriodic(ctx context.Context, name string, run func(context.Context)) {
	if !waitContext(ctx, a.stableJitter(name)) {
		return
	}
	for {
		run(ctx)
		if !waitContext(ctx, a.Config.PollInterval) {
			return
		}
	}
}

func (a *Agent) runConfigurationRound(ctx context.Context) {
	a.setComponentFailure("certificate", "renew edge certificate", a.renewIfNeeded(ctx))
	manifest, supported := a.controlManifest()
	if supported && manifest.DesiredStateVersion <= a.appliedVersion() {
		a.setComponentFailure("configuration", "sync desired state", nil)
		return
	}
	a.setComponentFailure("configuration", "sync desired state", a.Sync(ctx))
}

func (a *Agent) runMonitoringRound(ctx context.Context) {
	manifest, supported := a.controlManifest()
	revision := ""
	if supported {
		revision = manifest.MonitoringRevision
	}
	a.setComponentFailure("monitoring", "TCP monitoring", a.monitor(ctx, revision, supported))
}

func (a *Agent) runUpgradeRound(ctx context.Context) {
	manifest, supported := a.controlManifest()
	if supported && manifest.UpgradeTaskID == "" && a.activeUpgradeID() == "" {
		a.setComponentFailure("upgrade", "process online upgrade", nil)
		return
	}
	a.setComponentFailure("upgrade", "process online upgrade", a.ProcessUpgrade(ctx))
}

func (a *Agent) stableJitter(purpose string) time.Duration {
	maximum := a.Config.PollInterval / 5
	if maximum > 5*time.Second {
		maximum = 5 * time.Second
	}
	if maximum <= 0 {
		return 0
	}
	identity, _ := os.ReadFile(a.Config.ClientCertPath)
	digest := sha256.Sum256(append(append(identity, a.Config.ControlURL...), purpose...))
	value := uint64(0)
	for _, item := range digest[:8] {
		value = value<<8 | uint64(item)
	}
	return time.Duration(value % uint64(maximum))
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *Agent) setComponentFailure(component, action string, err error) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if err == nil || errors.Is(err, context.Canceled) {
		delete(a.componentFailures, component)
		return
	}
	a.componentFailures[component] = action + ": " + err.Error()
}

func (a *Agent) heartbeatError() string {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	for _, component := range []string{"certificate", "configuration", "monitoring", "upgrade"} {
		if detail := a.componentFailures[component]; detail != "" {
			return detail
		}
	}
	if detail := a.logs.LastError(); detail != "" {
		return detail
	}
	if a.security != nil {
		if detail := a.security.LastError(); detail != "" {
			return "edge security: " + detail
		}
	}
	return ""
}

func (a *Agent) setApplyReport(report *domain.ApplyReport) {
	a.statusMu.Lock()
	a.lastApplyReport = report
	a.applyReportSeq++
	a.statusMu.Unlock()
}

func (a *Agent) heartbeatReport() (*domain.ApplyReport, uint64) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.lastApplyReport == nil {
		return nil, a.applyReportSeq
	}
	report := *a.lastApplyReport
	report.PortConflicts = append([]domain.PortConflict(nil), report.PortConflicts...)
	return &report, a.applyReportSeq
}

func (a *Agent) acknowledgeApplyReport(sequence uint64, sent bool) {
	if !sent {
		return
	}
	a.statusMu.Lock()
	if a.applyReportSeq == sequence {
		a.lastApplyReport = nil
	}
	a.statusMu.Unlock()
}

func (a *Agent) controlManifest() (domain.EdgeControlManifest, bool) {
	a.manifestMu.RLock()
	defer a.manifestMu.RUnlock()
	if a.manifest == nil {
		return domain.EdgeControlManifest{}, false
	}
	return *a.manifest, true
}

func (a *Agent) setControlManifest(manifest *domain.EdgeControlManifest) {
	a.manifestMu.Lock()
	if manifest == nil {
		a.manifest = nil
	} else {
		copyOfManifest := *manifest
		a.manifest = &copyOfManifest
	}
	a.manifestMu.Unlock()
	a.logs.SetCompressionEnabled(manifest != nil && manifest.AccessLogGzip)
	if manifest != nil && a.security != nil {
		a.security.NotifyRevision(manifest.SecurityRevision)
	}
}

func (a *Agent) EnsureEnrollment(ctx context.Context) error {
	if pathExists(a.Config.ClientCertPath) && pathExists(a.Config.ClientKeyPath) && pathExists(a.Config.CAPath) {
		return a.renewIfNeeded(ctx)
	}
	if strings.TrimSpace(a.Config.EnrollmentToken) == "" {
		return errors.New("edge is not enrolled and ENROLLMENT_TOKEN is empty")
	}
	_, csr, err := a.loadOrCreateKeyAndCSR()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"enrollment_token": a.Config.EnrollmentToken, "csr": string(csr)})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/enroll", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.bootstrapClient().Do(request)
	if err != nil {
		return fmt.Errorf("enroll edge: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("enroll edge: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var result struct {
		ClientCertificate string `json:"client_certificate"`
		CACertificate     string `json:"ca_certificate"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return err
	}
	if result.ClientCertificate == "" || result.CACertificate == "" {
		return errors.New("enrollment response is missing certificates")
	}
	if err := atomicWriteFile(a.Config.ClientCertPath, []byte(result.ClientCertificate), 0o600); err != nil {
		return err
	}
	if err := atomicWriteFile(a.Config.CAPath, []byte(result.CACertificate), 0o644); err != nil {
		return err
	}
	a.resetControlClient()
	return nil
}

func (a *Agent) renewIfNeeded(ctx context.Context) error {
	certificatePEM, err := os.ReadFile(a.Config.ClientCertPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		return errors.New("invalid edge client certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if certificate.NotAfter.After(time.Now().UTC().Add(30 * 24 * time.Hour)) {
		return nil
	}
	_, csr, err := a.loadOrCreateKeyAndCSR()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"csr": string(csr)})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/renew", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("renew edge certificate: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("renew edge certificate: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var result struct {
		ClientCertificate string `json:"client_certificate"`
		CACertificate     string `json:"ca_certificate"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return err
	}
	if result.ClientCertificate == "" || result.CACertificate == "" {
		return errors.New("renewal response is missing certificates")
	}
	if err := atomicWriteFile(a.Config.ClientCertPath, []byte(result.ClientCertificate), 0o600); err != nil {
		return err
	}
	if err := atomicWriteFile(a.Config.CAPath, []byte(result.CACertificate), 0o644); err != nil {
		return err
	}
	a.resetControlClient()
	return nil
}

func (a *Agent) Sync(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.ControlURL+"/api/edge/v1/desired-state", nil)
	if err != nil {
		return err
	}
	request.Header.Set("If-None-Match", fmt.Sprintf("\"desired-%d\"", a.appliedVersion()))
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("pull desired state: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode == http.StatusNotModified {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("pull desired state: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var state domain.DesiredState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		return err
	}
	if state.Version == 0 || state.NginxConfig == "" || state.Version <= a.appliedVersion() {
		return nil
	}
	return a.apply(state)
}

func (a *Agent) Heartbeat(ctx context.Context, appliedVersion int64, lastError string, report *domain.ApplyReport) error {
	var machineStatus *domain.MachineStatus
	if a.machineStatus != nil {
		machineStatus, _ = a.machineStatus.Collect()
	}
	payload, err := json.Marshal(map[string]any{
		"last_error": lastError, "applied_version": appliedVersion, "apply_report": report,
		"capabilities":  append([]string(nil), a.Config.Capabilities...),
		"agent_version": a.Config.AgentVersion, "agent_sha256": a.Config.AgentSHA256, "active_upgrade_task_id": a.activeUpgradeID(),
		"cache_storage":  a.cacheUsage.Snapshot(),
		"machine_status": machineStatus,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/heartbeat", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return err
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat: %s", response.Status)
	}
	var result domain.EdgeHeartbeatResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		if errors.Is(err, io.EOF) {
			a.setControlManifest(nil)
			return nil
		}
		return fmt.Errorf("decode heartbeat response: %w", err)
	}
	if result.Control != nil {
		if result.Control.DesiredStateVersion < 0 ||
			(result.Control.MonitoringRevision != "" && !validDigest(result.Control.MonitoringRevision)) ||
			(result.Control.SecurityRevision != "" && !validDigest(result.Control.SecurityRevision)) {
			return errors.New("heartbeat returned an invalid control manifest")
		}
		if result.Control.UpgradeTaskID != "" {
			if _, err := uuid.Parse(result.Control.UpgradeTaskID); err != nil {
				return errors.New("heartbeat returned an invalid upgrade task ID")
			}
		}
	}
	a.setControlManifest(result.Control)
	return nil
}

func (a *Agent) apply(state domain.DesiredState) error {
	ports, err := desiredPublicPorts(state)
	if err != nil {
		return a.applyFailed(state.Version, "invalid_desired_state", err, nil)
	}
	configurationBackups := make(map[string]fileBackup)
	for _, path := range []string{a.Config.NginxConfigPath, a.Config.NginxStreamConfigPath, a.Config.NginxMainConfigPath, a.Config.NginxEventsConfigPath} {
		backup, backupErr := readBackup(path)
		if backupErr != nil {
			return a.applyFailed(state.Version, "config_backup_failed", backupErr, nil)
		}
		configurationBackups[path] = backup
	}
	if err := a.addFragmentBackups(configurationBackups); err != nil {
		return a.applyFailed(state.Version, "config_backup_failed", err, nil)
	}
	listeners, err := a.Config.Runner.PortListeners(ports)
	if err != nil {
		return a.applyFailed(state.Version, "port_check_failed", err, nil)
	}
	if conflicts := unmanagedListeners(listeners, managedListenerPorts(configurationBackups, ports)); len(conflicts) > 0 {
		return a.applyFailed(state.Version, "port_conflict", fmt.Errorf("public port is already held by another service: %s", formatPortConflicts(conflicts)), conflicts)
	}
	backups, code, err := a.stageCertificates(state.Certificates)
	if err != nil {
		return a.applyFailed(state.Version, code, err, nil)
	}
	httpConfig := state.NginxConfig
	streamConfig := state.NginxStreamConfig
	createdFragmentDirectories := make([]string, 0, 2)
	activeFragmentDirectories := make([]string, 0, 2)
	fragmentsActivated := false
	defer func() {
		if fragmentsActivated {
			return
		}
		for _, directory := range createdFragmentDirectories {
			_ = os.RemoveAll(directory)
		}
	}()
	if state.NginxFragments != nil {
		httpConfig, streamConfig, activeFragmentDirectories, createdFragmentDirectories, err = a.stageNginxFragments(state.Version, *state.NginxFragments)
		if err != nil {
			restoreBackups(backups)
			return a.applyFailed(state.Version, "config_write_failed", err, nil)
		}
	}
	if streamConfig == "" {
		streamConfig = "# Generated by cdn-edge-agent. Do not edit.\n"
	}
	if state.NginxMainConfig != "" {
		if err := atomicWriteFile(a.Config.NginxMainConfigPath, []byte(state.NginxMainConfig), 0o640); err != nil {
			restoreBackups(backups)
			restoreConfigurationFiles(configurationBackups)
			return a.applyFailed(state.Version, "config_write_failed", fmt.Errorf("write Nginx main configuration: %w", err), nil)
		}
	}
	if state.NginxEventsConfig != "" {
		if err := atomicWriteFile(a.Config.NginxEventsConfigPath, []byte(state.NginxEventsConfig), 0o640); err != nil {
			restoreBackups(backups)
			restoreConfigurationFiles(configurationBackups)
			return a.applyFailed(state.Version, "config_write_failed", fmt.Errorf("write Nginx events configuration: %w", err), nil)
		}
	}
	if err := atomicWriteFile(a.Config.NginxConfigPath, []byte(httpConfig), 0o640); err != nil {
		restoreBackups(backups)
		restoreConfigurationFiles(configurationBackups)
		return a.applyFailed(state.Version, "config_write_failed", fmt.Errorf("write Nginx HTTP configuration: %w", err), nil)
	}
	if err := atomicWriteFile(a.Config.NginxStreamConfigPath, []byte(streamConfig), 0o640); err != nil {
		restoreBackups(backups)
		restoreConfigurationFiles(configurationBackups)
		return a.applyFailed(state.Version, "config_write_failed", fmt.Errorf("write Nginx stream configuration: %w", err), nil)
	}
	if err := prepareManagedCacheLayout(nginx.DefaultCachePath, state.NginxConfig); err != nil {
		restoreBackups(backups)
		a.restorePreviousConfigs(configurationBackups)
		return a.applyFailed(state.Version, "cache_layout_failed", err, nil)
	}
	if err := a.Config.Runner.Test(); err != nil {
		restoreBackups(backups)
		a.restorePreviousConfigs(configurationBackups)
		return a.applyFailed(state.Version, "nginx_config_invalid", err, nil)
	}
	if err := a.Config.Runner.Apply(); err != nil {
		restoreBackups(backups)
		a.restorePreviousConfigs(configurationBackups)
		if listeners, inspectErr := a.Config.Runner.PortListeners(ports); inspectErr == nil {
			if conflicts := foreignListeners(listeners); len(conflicts) > 0 {
				return a.applyFailed(state.Version, "port_conflict", fmt.Errorf("public port is already held by another service: %s", formatPortConflicts(conflicts)), conflicts)
			}
		}
		return a.applyFailed(state.Version, "nginx_apply_failed", err, nil)
	}
	listeners, err = a.waitForNginxListeners(ports)
	if err != nil {
		restoreBackups(backups)
		a.restorePreviousConfigs(configurationBackups)
		return a.applyFailed(state.Version, "port_check_failed", err, nil)
	}
	if conflicts := foreignListeners(listeners); len(conflicts) > 0 {
		restoreBackups(backups)
		a.restorePreviousConfigs(configurationBackups)
		return a.applyFailed(state.Version, "port_conflict", fmt.Errorf("public port is already held by another service: %s", formatPortConflicts(conflicts)), conflicts)
	}
	if !nginxOwnsPorts(listeners, ports) {
		restoreBackups(backups)
		a.restorePreviousConfigs(configurationBackups)
		return a.applyFailed(state.Version, "nginx_not_listening", fmt.Errorf("Nginx did not retain all required public listeners after applying configuration: %s", formatPorts(ports)), nil)
	}
	fragmentsActivated = true
	if a.cacheUsage != nil {
		a.cacheUsage.SetTotalBytes(state.CacheMaxBytes)
	}
	cacheCleanupErr := reconcileInstalledCacheLayout(nginx.DefaultCachePath, state.NginxConfig)
	if err := atomicWriteFile(filepath.Join(a.Config.StateDir, "applied-version"), []byte(fmt.Sprintf("%d\n", state.Version)), 0o640); err != nil {
		return a.applyFailed(state.Version, "applied_version_write_failed", err, nil)
	}
	a.cleanupOldFragmentDirectories(activeFragmentDirectories)
	detail := "Nginx is listening on " + formatPorts(ports)
	if cacheCleanupErr != nil {
		detail += "; cache cleanup warning: " + cacheCleanupErr.Error()
	}
	a.setApplyReport(&domain.ApplyReport{Version: state.Version, Status: domain.ApplySucceeded, Detail: detail})
	return nil
}

func (a *Agent) stageNginxFragments(version int64, fragments domain.NginxConfigFragments) (string, string, []string, []string, error) {
	root := filepath.Join(filepath.Dir(a.Config.NginxConfigPath), "fragments")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", "", nil, nil, fmt.Errorf("create Nginx fragment root: %w", err)
	}
	httpDirectory, httpCreated, err := stageFragmentDirectory(root, "http", version, fragments.HTTPBase, fragments.HTTPSites)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("stage Nginx HTTP fragments: %w", err)
	}
	created := make([]string, 0, 2)
	if httpCreated {
		created = append(created, httpDirectory)
	}
	streamDirectory, streamCreated, err := stageFragmentDirectory(root, "stream", version, fragments.StreamBase, fragments.StreamSites)
	if err != nil {
		for _, directory := range created {
			_ = os.RemoveAll(directory)
		}
		return "", "", nil, nil, fmt.Errorf("stage Nginx stream fragments: %w", err)
	}
	if streamCreated {
		created = append(created, streamDirectory)
	}
	httpConfig := generatedFragmentIndex(httpDirectory)
	streamConfig := generatedFragmentIndex(streamDirectory)
	return httpConfig, streamConfig, []string{httpDirectory, streamDirectory}, created, nil
}

func stageFragmentDirectory(root, kind string, version int64, base string, fragments []domain.NginxConfigFragment) (string, bool, error) {
	if base == "" {
		return "", false, errors.New("base fragment is empty")
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, base)
	for _, fragment := range fragments {
		_, _ = io.WriteString(digest, "\x00"+fragment.Name+"\x00"+fragment.Content)
	}
	directory := filepath.Join(root, fmt.Sprintf("%s-v%d-%x", kind, version, digest.Sum(nil)[:6]))
	if matches, err := fragmentDirectoryMatches(directory, base, fragments); err != nil {
		return "", false, err
	} else if matches {
		return directory, false, nil
	}
	temporary, err := os.MkdirTemp(root, "."+kind+"-fragments-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o750); err != nil {
		return "", false, err
	}
	if err := atomicWriteFile(filepath.Join(temporary, "00-base.conf"), []byte(base), 0o640); err != nil {
		return "", false, err
	}
	seen := map[string]bool{"00-base.conf": true}
	for _, fragment := range fragments {
		if !validFragmentName(fragment.Name) || fragment.Content == "" || seen[fragment.Name] {
			return "", false, fmt.Errorf("invalid or duplicate Nginx fragment %q", fragment.Name)
		}
		seen[fragment.Name] = true
		if err := atomicWriteFile(filepath.Join(temporary, fragment.Name), []byte(fragment.Content), 0o640); err != nil {
			return "", false, err
		}
	}
	if err := os.Rename(temporary, directory); err != nil {
		if matches, matchErr := fragmentDirectoryMatches(directory, base, fragments); matchErr == nil && matches {
			return directory, false, nil
		}
		return "", false, err
	}
	return directory, true, nil
}

func fragmentDirectoryMatches(directory, base string, fragments []domain.NginxConfigFragment) (bool, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	expected := make(map[string]string, len(fragments)+1)
	expected["00-base.conf"] = base
	for _, fragment := range fragments {
		if !validFragmentName(fragment.Name) || fragment.Content == "" {
			return false, fmt.Errorf("invalid Nginx fragment %q", fragment.Name)
		}
		if _, found := expected[fragment.Name]; found {
			return false, fmt.Errorf("duplicate Nginx fragment %q", fragment.Name)
		}
		expected[fragment.Name] = fragment.Content
	}
	if len(entries) != len(expected) {
		return false, fmt.Errorf("existing Nginx fragment directory %s does not match its content digest", directory)
	}
	for _, entry := range entries {
		want, found := expected[entry.Name()]
		if !found || entry.IsDir() {
			return false, fmt.Errorf("existing Nginx fragment directory %s contains an unexpected entry", directory)
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return false, err
		}
		if string(contents) != want {
			return false, fmt.Errorf("existing Nginx fragment directory %s failed content verification", directory)
		}
	}
	return true, nil
}

func validFragmentName(name string) bool {
	if name == "" || name != filepath.Base(name) || !strings.HasSuffix(name, ".conf") {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func generatedFragmentIndex(directory string) string {
	return "# Generated by cdn-edge-agent. Do not edit.\ninclude " + filepath.ToSlash(directory) + "/*.conf;\n"
}

func (a *Agent) cleanupOldFragmentDirectories(active []string) {
	if len(active) == 0 {
		return
	}
	keep := make(map[string]bool, len(active))
	for _, directory := range active {
		keep[filepath.Clean(directory)] = true
	}
	root := filepath.Dir(active[0])
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || (!strings.HasPrefix(entry.Name(), "http-v") && !strings.HasPrefix(entry.Name(), "stream-v")) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if !keep[path] {
			_ = os.RemoveAll(path)
		}
	}
}

func (a *Agent) waitForNginxListeners(ports []int) ([]domain.PortConflict, error) {
	deadline := time.Now().Add(a.Config.ListenerSettleTimeout)
	for {
		listeners, err := a.Config.Runner.PortListeners(ports)
		if err != nil {
			return nil, err
		}
		if len(foreignListeners(listeners)) > 0 || nginxOwnsPorts(listeners, ports) {
			return listeners, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return listeners, nil
		}
		delay := a.Config.ListenerPollInterval
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
	}
}

func desiredPublicPorts(state domain.DesiredState) ([]int, error) {
	if state.PublicPorts == nil {
		// Desired states emitted before this field was introduced all represented
		// a public CDN configuration and therefore retain the original 80/443
		// expectation during a rolling control-plane upgrade.
		return []int{80, 443}, nil
	}
	ports := make([]int, 0, len(state.PublicPorts))
	seen := make(map[int]bool, len(state.PublicPorts))
	for _, port := range state.PublicPorts {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid public TCP port %d", port)
		}
		if seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports, nil
}

func restoreConfigurationFiles(backups map[string]fileBackup) {
	for path, backup := range backups {
		if backup.exists {
			_ = atomicWriteFile(path, backup.contents, 0o640)
		} else {
			_ = os.Remove(path)
		}
	}
}

func (a *Agent) addFragmentBackups(backups map[string]fileBackup) error {
	root := filepath.Clean(filepath.Join(filepath.Dir(a.Config.NginxConfigPath), "fragments"))
	indexPaths := []string{a.Config.NginxConfigPath, a.Config.NginxStreamConfigPath}
	for _, indexPath := range indexPaths {
		backup := backups[indexPath]
		if !backup.exists || !bytes.HasPrefix(backup.contents, []byte("# Generated by cdn-edge-agent. Do not edit.\ninclude ")) {
			continue
		}
		lines := strings.Split(string(backup.contents), "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[1], "include ") || !strings.HasSuffix(lines[1], "/*.conf;") {
			return fmt.Errorf("generated Nginx fragment index %s is malformed", indexPath)
		}
		pattern := strings.TrimSuffix(strings.TrimPrefix(lines[1], "include "), ";")
		directory := filepath.Clean(strings.TrimSuffix(pattern, "/*.conf"))
		if filepath.Dir(directory) != root {
			return fmt.Errorf("generated Nginx fragment index %s points outside the managed root", indexPath)
		}
		paths, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("read Nginx fragment index %s: %w", indexPath, err)
		}
		if len(paths) == 0 {
			return fmt.Errorf("generated Nginx fragment index %s has no fragments", indexPath)
		}
		for _, path := range paths {
			fragment, err := readBackup(path)
			if err != nil {
				return fmt.Errorf("back up Nginx fragment %s: %w", filepath.Base(path), err)
			}
			backups[path] = fragment
		}
	}
	return nil
}

func (a *Agent) restorePreviousConfigs(backups map[string]fileBackup) {
	restoreConfigurationFiles(backups)
	if a.Config.Runner.Test() == nil {
		_ = a.Config.Runner.Apply()
	}
}

func (a *Agent) applyFailed(version int64, code string, err error, conflicts []domain.PortConflict) error {
	a.setApplyReport(&domain.ApplyReport{Version: version, Status: domain.ApplyFailed, Code: code, Detail: err.Error(), PortConflicts: conflicts})
	return err
}

func foreignListeners(listeners []domain.PortConflict) []domain.PortConflict {
	conflicts := make([]domain.PortConflict, 0)
	for _, listener := range listeners {
		name := strings.ToLower(strings.TrimSpace(listener.Process))
		if name == "nginx" || strings.HasPrefix(name, "nginx:") {
			continue
		}
		conflicts = append(conflicts, listener)
	}
	return conflicts
}

func managedListenerPorts(backups map[string]fileBackup, desiredPorts []int) map[int]bool {
	managed := make(map[int]bool, len(desiredPorts))
	for _, port := range desiredPorts {
		plain := fmt.Sprintf("listen %d;", port)
		withOptions := fmt.Sprintf("listen %d ", port)
		for _, backup := range backups {
			if backup.exists && (bytes.Contains(backup.contents, []byte(plain)) || bytes.Contains(backup.contents, []byte(withOptions))) {
				managed[port] = true
				break
			}
		}
	}
	return managed
}

func unmanagedListeners(listeners []domain.PortConflict, managedPorts map[int]bool) []domain.PortConflict {
	conflicts := make([]domain.PortConflict, 0)
	for _, listener := range listeners {
		name := strings.ToLower(strings.TrimSpace(listener.Process))
		if (name == "nginx" || strings.HasPrefix(name, "nginx:")) && managedPorts[listener.Port] {
			continue
		}
		if name == "nginx" || strings.HasPrefix(name, "nginx:") {
			listener.Process = "nginx (unmanaged configuration)"
		}
		conflicts = append(conflicts, listener)
	}
	return conflicts
}

func nginxOwnsPorts(listeners []domain.PortConflict, ports []int) bool {
	owned := make(map[int]bool, len(ports))
	for _, listener := range listeners {
		name := strings.ToLower(strings.TrimSpace(listener.Process))
		if name == "nginx" || strings.HasPrefix(name, "nginx:") {
			owned[listener.Port] = true
		}
	}
	for _, port := range ports {
		if !owned[port] {
			return false
		}
	}
	return true
}

func formatPortConflicts(conflicts []domain.PortConflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		value := fmt.Sprintf("TCP %d: %s", conflict.Port, conflict.Process)
		if conflict.PID > 0 {
			value += fmt.Sprintf(" (PID %d)", conflict.PID)
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, ", ")
}

func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return "no public TCP ports"
	}
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, fmt.Sprintf("TCP %d", port))
	}
	return strings.Join(values, " and ")
}

type fileBackup struct {
	contents []byte
	exists   bool
}

func readBackup(path string) (fileBackup, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileBackup{}, nil
	}
	if err != nil {
		return fileBackup{}, err
	}
	return fileBackup{contents: contents, exists: true}, nil
}

func restoreBackups(backups map[string]fileBackup) {
	for path, backup := range backups {
		if backup.exists {
			_ = atomicWriteFile(path, backup.contents, 0o600)
		} else {
			_ = os.Remove(path)
		}
	}
}

func (a *Agent) stageCertificates(certificates map[string]domain.TLSBundle) (map[string]fileBackup, string, error) {
	if err := os.MkdirAll(a.Config.CertificateDir, 0o750); err != nil {
		return nil, "certificate_write_failed", err
	}
	desiredPaths := make(map[string]bool, len(certificates)*2)
	for siteID := range certificates {
		desiredPaths[filepath.Join(a.Config.CertificateDir, siteID+".crt")] = true
		desiredPaths[filepath.Join(a.Config.CertificateDir, siteID+".key")] = true
	}
	entries, err := os.ReadDir(a.Config.CertificateDir)
	if err != nil {
		return nil, "certificate_backup_failed", err
	}
	stalePaths := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !managedSiteCertificateName(entry.Name()) {
			continue
		}
		path := filepath.Join(a.Config.CertificateDir, entry.Name())
		if !desiredPaths[path] {
			stalePaths = append(stalePaths, path)
		}
	}
	backups := make(map[string]fileBackup, len(desiredPaths)+len(stalePaths))
	for path := range desiredPaths {
		backup, err := readBackup(path)
		if err != nil {
			return nil, "certificate_backup_failed", err
		}
		backups[path] = backup
	}
	for _, path := range stalePaths {
		backup, err := readBackup(path)
		if err != nil {
			return nil, "certificate_backup_failed", err
		}
		backups[path] = backup
	}
	for siteID, certificate := range certificates {
		certificatePath := filepath.Join(a.Config.CertificateDir, siteID+".crt")
		privateKeyPath := filepath.Join(a.Config.CertificateDir, siteID+".key")
		if err := atomicWriteFile(certificatePath, []byte(certificate.CertificatePEM), 0o600); err != nil {
			restoreBackups(backups)
			return nil, "certificate_write_failed", fmt.Errorf("write certificate for %s: %w", siteID, err)
		}
		if err := atomicWriteFile(privateKeyPath, []byte(certificate.PrivateKeyPEM), 0o600); err != nil {
			restoreBackups(backups)
			return nil, "private_key_write_failed", fmt.Errorf("write private key for %s: %w", siteID, err)
		}
	}
	for _, path := range stalePaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			restoreBackups(backups)
			return nil, "certificate_remove_failed", fmt.Errorf("remove stale certificate file %s: %w", filepath.Base(path), err)
		}
	}
	return backups, "", nil
}

func managedSiteCertificateName(name string) bool {
	extension := filepath.Ext(name)
	if extension != ".crt" && extension != ".key" {
		return false
	}
	_, err := uuid.Parse(strings.TrimSuffix(name, extension))
	return err == nil
}

func (a *Agent) loadOrCreateKeyAndCSR() (*ecdsa.PrivateKey, []byte, error) {
	var key *ecdsa.PrivateKey
	if existing, err := os.ReadFile(a.Config.ClientKeyPath); err == nil {
		block, _ := pem.Decode(existing)
		if block == nil {
			return nil, nil, errors.New("invalid edge client key")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, nil, err
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, errors.New("edge client key is not ECDSA")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		if err := atomicWriteFile(a.Config.ClientKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, nil, err
		}
	} else {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkixName("cdn-edge")}, key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), nil
}

func (a *Agent) bootstrapClient() *http.Client {
	if a.Config.HTTPClient != nil {
		return a.Config.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (a *Agent) client() *http.Client {
	if a.Config.HTTPClient != nil {
		return a.Config.HTTPClient
	}
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if a.controlClient != nil {
		return a.controlClient
	}
	certificate, err := tls.LoadX509KeyPair(a.Config.ClientCertPath, a.Config.ClientKeyPath)
	if err != nil {
		return &http.Client{Timeout: 30 * time.Second, Transport: rejectingTransport{err: err}}
	}
	roots, _ := x509.SystemCertPool()
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if internalCA, err := os.ReadFile(a.Config.CAPath); err == nil {
		roots.AppendCertsFromPEM(internalCA)
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	a.controlTransport = transport
	a.controlClient = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	return a.controlClient
}

func (a *Agent) appliedVersion() int64 {
	contents, err := os.ReadFile(filepath.Join(a.Config.StateDir, "applied-version"))
	if err != nil {
		return 0
	}
	var version int64
	_, _ = fmt.Sscanf(strings.TrimSpace(string(contents)), "%d", &version)
	return version
}

type rejectingTransport struct{ err error }

func (r rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

func (a *Agent) resetControlClient() {
	if a.Config.HTTPClient != nil {
		return
	}
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if a.controlTransport != nil {
		a.controlTransport.CloseIdleConnections()
	}
	a.controlClient = nil
	a.controlTransport = nil
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

func pkixName(commonName string) pkix.Name { return pkix.Name{CommonName: commonName} }

func atomicWriteFile(path string, contents []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".cdn-platform-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func pathExists(path string) bool { _, err := os.Stat(path); return err == nil }
