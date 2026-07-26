package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"simple_cdn/internal/control"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/project"
	"simple_cdn/internal/store"
	"simple_cdn/internal/version"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version.Version)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "keygen" {
		key, err := control.NewEncryptionKey()
		if err != nil {
			fatal(err.Error())
		}
		fmt.Println(key)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "publish-all" {
		publishAll()
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "publish-site" {
		publishSite(os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "cloudflare-credentials" {
		writeCloudflareCredentials(os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "backup-runtime" {
		writeBackupRuntime(os.Args[2])
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "backup-status" {
		if len(os.Args) != 7 && len(os.Args) != 8 {
			fatal("usage: cdn-control backup-status <path> <state> <attempt> <max-attempts> <started-at> [detail]")
		}
		writeBackupStatus(os.Args[2:])
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dataDir := env("CONTROL_DATA_DIR", "/var/lib/cdn-platform")
	key := os.Getenv("CONTROL_ENCRYPTION_KEY")
	if key == "" {
		fatal("CONTROL_ENCRYPTION_KEY is required; generate it with: cdn-control keygen")
	}
	cipher, err := control.NewCipher(key)
	if err != nil {
		fatal(err.Error())
	}
	restoreRoot := env("ONLINE_RESTORE_ROOT", "/var/lib/cdn-platform-restore")
	clickHouseDatabase := strings.TrimSpace(os.Getenv("CLICKHOUSE_DATABASE"))
	if clickHouseDatabase == "" || clickHouseDatabase == project.LegacyClickHouseDatabase {
		clickHouseDatabase = project.ClickHouseDatabase
	}
	clickHouseAdmin := control.HTTPClickHouseRestoreAdmin{
		Endpoint: env("CLICKHOUSE_URL", "http://127.0.0.1:8123"),
		Username: os.Getenv("CLICKHOUSE_USER"),
		Password: os.Getenv("CLICKHOUSE_PASSWORD"),
	}
	restoreReadyTimeout := durationEnvironment("ONLINE_RESTORE_READY_TIMEOUT", 2*time.Minute)
	restoreApplyTimeout := durationEnvironment("ONLINE_RESTORE_APPLY_TIMEOUT", 30*time.Minute)
	appliedRestore, err := control.ApplyPendingOnlineRestore(context.Background(), control.OnlineRestoreApplyConfig{
		Root:             restoreRoot,
		DataDir:          dataDir,
		TLSDir:           env("CONTROL_TLS_DIR", "/var/lib/cdn-control-tls"),
		ControlTLSDomain: os.Getenv("CONTROL_TLS_DOMAIN"),
		Cipher:           cipher,
		ClickHouse:       clickHouseAdmin,
		ReadyTimeout:     restoreReadyTimeout,
		ApplyTimeout:     restoreApplyTimeout,
	})
	if err != nil {
		fatal("apply pending online restore: " + err.Error())
	}
	if appliedRestore {
		logger.Info("online restore cutover completed")
	}
	if os.Getenv("CLICKHOUSE_DISABLED") != "1" && clickHouseDatabase == project.ClickHouseDatabase {
		migrated, err := control.MigrateLegacyClickHouseDatabase(context.Background(), clickHouseAdmin)
		if err != nil {
			fatal("migrate legacy ClickHouse database: " + err.Error())
		}
		if migrated {
			logger.Info("migrated legacy ClickHouse database", "from", project.LegacyClickHouseDatabase, "to", project.ClickHouseDatabase)
		}
	}
	database, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		fatal("open database: " + err.Error())
	}
	defer database.Close()
	ca, err := control.LoadOrCreateInternalCA(filepath.Join(dataDir, "pki"))
	if err != nil {
		fatal("load internal CA: " + err.Error())
	}
	environment, err := settingsFromEnv()
	if err != nil {
		fatal(err.Error())
	}
	settings, err := control.NewSettingsManager(database, cipher, environment)
	if err != nil {
		fatal("load settings: " + err.Error())
	}
	var onlineRestore *control.OnlineRestoreManager
	if os.Getenv("CLICKHOUSE_DISABLED") != "1" {
		clickHouseGroupID := 101
		if value := strings.TrimSpace(os.Getenv("CLICKHOUSE_HOST_GID")); value != "" {
			clickHouseGroupID, err = strconv.Atoi(value)
			if err != nil || clickHouseGroupID < 0 {
				fatal("CLICKHOUSE_HOST_GID must be a non-negative integer")
			}
		}
		onlineRestore, err = control.NewOnlineRestoreManager(control.OnlineRestoreManagerConfig{
			Root:                restoreRoot,
			Settings:            settings,
			Cipher:              cipher,
			ClickHouse:          clickHouseAdmin,
			Database:            clickHouseDatabase,
			ControlTLSDomain:    os.Getenv("CONTROL_TLS_DOMAIN"),
			ClickHouseGroupID:   clickHouseGroupID,
			RestoreTimeout:      durationEnvironment("ONLINE_RESTORE_PREPARE_TIMEOUT", 2*time.Hour),
			SnapshotListTimeout: durationEnvironment("ONLINE_RESTORE_LIST_TIMEOUT", time.Minute),
			QuiesceTimeout:      durationEnvironment("ONLINE_RESTORE_QUIESCE_TIMEOUT", 2*time.Minute),
		})
		if err != nil {
			fatal("initialize online restore: " + err.Error())
		}
		defer onlineRestore.Stop()
	}
	dns := &integrations.CloudflareDNS{Token: settings.CloudflareToken}
	issuer := integrations.CertbotDNSIssuer{Email: os.Getenv("ACME_EMAIL"), ConfigDir: filepath.Join(dataDir, "letsencrypt"), Token: settings.CloudflareToken}
	var logs logstore.Store = logstore.Noop{}
	var monitoringHistory logstore.MonitoringHistoryStore
	if os.Getenv("CLICKHOUSE_DISABLED") != "1" {
		clickhouse := logstore.ClickHouse{Endpoint: env("CLICKHOUSE_URL", "http://127.0.0.1:8123"), Database: clickHouseDatabase, Username: os.Getenv("CLICKHOUSE_USER"), Password: os.Getenv("CLICKHOUSE_PASSWORD")}
		if err := clickhouse.EnsureSchema(context.Background()); err != nil {
			logger.Warn("ClickHouse schema setup failed; log writes will retry", "error", err)
		}
		logs = clickhouse
		monitoringHistory = clickhouse
	}
	publisher := control.Publisher{Store: database, Cipher: cipher}
	var notifier integrations.Notifier = settings
	issueTimeout := 10 * time.Minute
	if value := strings.TrimSpace(os.Getenv("CERTIFICATE_ISSUE_TIMEOUT")); value != "" {
		issueTimeout, err = time.ParseDuration(value)
		if err != nil || issueTimeout <= 0 {
			fatal("CERTIFICATE_ISSUE_TIMEOUT must be a positive Go duration")
		}
	}
	certificateManager := &control.CertificateManager{Store: database, Publisher: publisher, Issuer: issuer, Notifier: notifier, IssueTimeout: issueTimeout}
	siteDeleter := &control.SiteDeletionManager{Store: database, Publisher: publisher, DNS: dns, Certificates: issuer}
	setupCIDRs, err := parseCIDRs(os.Getenv("SETUP_ALLOW_CIDRS"))
	if err != nil {
		fatal("SETUP_ALLOW_CIDRS: " + err.Error())
	}
	trustedProxyCIDRs, err := parseCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		fatal("TRUSTED_PROXY_CIDRS: " + err.Error())
	}
	controlURL := env("CONTROL_PUBLIC_URL", "https://control.example.invalid")
	initializationTokenPath := env("CONTROL_INITIALIZATION_TOKEN_FILE", filepath.Join(dataDir, "initialization-token"))
	edgeBinaryPath := os.Getenv("EDGE_BINARY_PATH")
	edgeBinarySHA256, err := control.ResolveEdgeBinarySHA256(edgeBinaryPath)
	if err != nil {
		fatal(err.Error())
	}
	server := &control.Server{Store: database, Cipher: cipher, CA: ca, Publisher: publisher, DNS: dns, ZoneResolver: dns, Cloudflare: dns, Issuer: issuer, CertificateManager: certificateManager, SiteDeleter: siteDeleter, Settings: settings, BackupValidator: control.ResticBackupRepositoryValidator{}, BackupStatusPath: env("BACKUP_STATUS_FILE", "/var/lib/cdn-platform-operations/backup.json"), OnlineRestore: onlineRestore, Notifier: notifier, Logs: logs, MonitoringHistory: monitoringHistory, ControlURL: controlURL, EdgeControlURL: env("EDGE_CONTROL_URL", controlURL), EdgeBinaryURL: os.Getenv("EDGE_BINARY_URL"), EdgeBinarySHA256: edgeBinarySHA256, EdgeBinaryPath: edgeBinaryPath, InitializationTokenPath: initializationTokenPath, SetupAllowCIDRs: setupCIDRs, TrustedProxyCIDRs: trustedProxyCIDRs, Logger: logger}
	if err := server.MigrateAdminTOTPSecret(); err != nil {
		fatal(err.Error())
	}
	hasAdmin, err := database.HasAdmin()
	if err != nil {
		fatal("check administrator setup: " + err.Error())
	}
	if hasAdmin {
		if err := control.RemoveInitializationToken(initializationTokenPath); err != nil {
			fatal("remove stale initialization token: " + err.Error())
		}
	} else {
		created, err := control.EnsureInitializationToken(initializationTokenPath)
		if err != nil {
			fatal("prepare initialization token: " + err.Error())
		}
		if created {
			logger.Info("initialization token created", "path", initializationTokenPath)
		} else {
			logger.Info("initialization token is ready", "path", initializationTokenPath)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var monitoringWriter *logstore.AsyncMonitoringWriter
	var stopMonitoringWriter context.CancelFunc
	if monitoringHistory != nil {
		monitoringWriterContext, cancelMonitoringWriter := context.WithCancel(context.Background())
		stopMonitoringWriter = cancelMonitoringWriter
		monitoringWriter = logstore.NewAsyncMonitoringWriter(monitoringHistory, logger)
		monitoringWriter.Start(monitoringWriterContext)
		server.MonitoringWriter = monitoringWriter
	}
	defer func() {
		stop()
		if monitoringWriter != nil {
			stopMonitoringWriter()
			monitoringWriter.Wait()
		}
	}()
	server.RestartControl = stop
	healthManager := &control.HealthManager{Server: server}
	server.HealthManager = healthManager
	certificateManager.Start(ctx)
	defer certificateManager.Stop()
	go healthManager.Run(ctx)
	go certificateManager.Run(ctx)
	certificatePath, privateKeyPath := os.Getenv("CONTROL_TLS_CERT"), os.Getenv("CONTROL_TLS_KEY")
	if certificatePath == "" || privateKeyPath == "" {
		fatal("CONTROL_TLS_CERT and CONTROL_TLS_KEY are required")
	}
	certificate, err := control.NewReloadingCertificate(certificatePath, privateKeyPath)
	if err != nil {
		fatal(err.Error())
	}
	listen := env("CONTROL_LISTEN", ":443")
	tlsConfig := server.TLSConfig()
	tlsConfig.GetCertificate = certificate.GetCertificate
	httpServer := &http.Server{Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: tlsConfig}
	go func() {
		<-ctx.Done()
		certificateManager.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	logger.Info("control plane listening", "address", listen)
	if err := httpServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		fatal(err.Error())
	}
}

// publishAll rebuilds edge configurations after a renderer upgrade without exposing an HTTP endpoint.
func publishAll() {
	database, publisher := openPublisher()
	defer database.Close()
	if err := publisher.PublishAll(); err != nil {
		fatal("publish all sites: " + err.Error())
	}
}

func publishSite(siteID string) {
	siteID = strings.TrimSpace(siteID)
	if siteID == "" {
		fatal("site ID is required")
	}
	database, publisher := openPublisher()
	defer database.Close()
	task, err := publisher.PublishSite(siteID)
	if err != nil {
		fatal("publish site: " + err.Error())
	}
	fmt.Printf("publish task %s: %s\n", task.ID, task.Status)
}

func openPublisher() (*store.Store, control.Publisher) {
	dataDir := env("CONTROL_DATA_DIR", "/var/lib/cdn-platform")
	key := os.Getenv("CONTROL_ENCRYPTION_KEY")
	if key == "" {
		fatal("CONTROL_ENCRYPTION_KEY is required")
	}
	cipher, err := control.NewCipher(key)
	if err != nil {
		fatal(err.Error())
	}
	database, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		fatal("open database: " + err.Error())
	}
	return database, control.Publisher{Store: database, Cipher: cipher}
}

func writeCloudflareCredentials(path string) {
	token := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
	dataDir := env("CONTROL_DATA_DIR", "/var/lib/cdn-platform")
	ciphertext, err := store.ReadSecret(filepath.Join(dataDir, "control.db"), store.SecretCloudflareAPIToken)
	if err == nil {
		key := os.Getenv("CONTROL_ENCRYPTION_KEY")
		if key == "" {
			fatal("CONTROL_ENCRYPTION_KEY is required to decrypt the Cloudflare API token")
		}
		cipher, err := control.NewCipher(key)
		if err != nil {
			fatal(err.Error())
		}
		plaintext, err := cipher.Decrypt(ciphertext)
		if err != nil {
			fatal("decrypt Cloudflare API token: " + err.Error())
		}
		token = strings.TrimSpace(string(plaintext))
	} else if !errors.Is(err, store.ErrNotFound) {
		fatal("read Cloudflare API token: " + err.Error())
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		fatal("CLOUDFLARE_API_TOKEN is not configured")
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".cloudflare-credentials-*")
	if err != nil {
		fatal("create Cloudflare credentials: " + err.Error())
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		fatal("secure Cloudflare credentials: " + err.Error())
	}
	if _, err := fmt.Fprintf(temporary, "dns_cloudflare_api_token = %s\n", token); err != nil {
		temporary.Close()
		fatal("write Cloudflare credentials: " + err.Error())
	}
	if err := temporary.Close(); err != nil {
		fatal("close Cloudflare credentials: " + err.Error())
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		fatal("install Cloudflare credentials: " + err.Error())
	}
}

func writeBackupRuntime(directory string) {
	dataDir := env("CONTROL_DATA_DIR", "/var/lib/cdn-platform")
	database, err := store.OpenReadOnly(filepath.Join(dataDir, "control.db"))
	if err != nil {
		fatal("open backup settings database: " + err.Error())
	}
	defer database.Close()
	key := os.Getenv("CONTROL_ENCRYPTION_KEY")
	if key == "" {
		fatal("CONTROL_ENCRYPTION_KEY is required to resolve backup settings")
	}
	cipher, err := control.NewCipher(key)
	if err != nil {
		fatal(err.Error())
	}
	runtimeFromEnvironment, environmentErr := backupRuntimeFromEnv()
	persisted, err := database.BackupSettingsSnapshot()
	if err != nil {
		fatal("read backup settings: " + err.Error())
	}
	if !persisted.Override && environmentErr != nil {
		fatal(environmentErr.Error())
	}
	runtime, databaseOverride, err := control.LoadBackupRuntime(database, cipher, runtimeFromEnvironment)
	if err != nil {
		fatal("resolve backup settings: " + err.Error())
	}
	directory = filepath.Clean(directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		fatal("create backup runtime directory: " + err.Error())
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		fatal("secure backup runtime directory: " + err.Error())
	}
	values := map[string]string{
		"repository":           runtime.Settings.Repository,
		"access-key-id":        runtime.Settings.AccessKeyID,
		"secret-access-key":    runtime.SecretAccessKey,
		"region":               runtime.Settings.Region,
		"restic-password":      runtime.ResticPassword,
		"backup-time":          runtime.Settings.BackupTime,
		"random-delay-seconds": strconv.Itoa(runtime.Settings.RandomDelaySeconds),
		"source":               control.SettingsSourceEnvironment,
	}
	if databaseOverride {
		values["source"] = control.SettingsSourceDatabase
	}
	for name, value := range values {
		if err := writePrivateRuntimeFile(directory, name, value); err != nil {
			fatal("write backup runtime settings: " + err.Error())
		}
	}
}

func writeBackupStatus(arguments []string) {
	attempt, err := strconv.Atoi(arguments[2])
	if err != nil {
		fatal("backup attempt must be an integer")
	}
	maxAttempts, err := strconv.Atoi(arguments[3])
	if err != nil {
		fatal("backup max attempts must be an integer")
	}
	startedAt, err := time.Parse(time.RFC3339, arguments[4])
	if err != nil {
		fatal("backup start time must use RFC3339: " + err.Error())
	}
	detail := ""
	if len(arguments) == 6 {
		detail = arguments[5]
	}
	host, _ := os.Hostname()
	status, err := control.NewBackupRunStatus(arguments[1], attempt, maxAttempts, host, startedAt, time.Now(), detail)
	if err != nil {
		fatal("build backup status: " + err.Error())
	}
	if err := control.WriteBackupRunStatus(arguments[0], status); err != nil {
		fatal(err.Error())
	}
	if status.State == control.BackupRunFailed {
		if err := notifyBackupFailure(status); err != nil {
			fatal("send backup failure alert: " + err.Error())
		}
	}
}

func notifyBackupFailure(status control.BackupRunStatus) error {
	dataDir := env("CONTROL_DATA_DIR", "/var/lib/cdn-platform")
	database, err := store.OpenReadOnly(filepath.Join(dataDir, "control.db"))
	if err != nil {
		return fmt.Errorf("open settings database: %w", err)
	}
	defer database.Close()
	key := os.Getenv("CONTROL_ENCRYPTION_KEY")
	if key == "" {
		return errors.New("CONTROL_ENCRYPTION_KEY is required")
	}
	cipher, err := control.NewCipher(key)
	if err != nil {
		return err
	}
	environment, err := settingsFromEnv()
	if err != nil {
		return err
	}
	settings, err := control.NewSettingsManager(database, cipher, environment)
	if err != nil {
		return fmt.Errorf("load SMTP settings: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return settings.NotifyNotification(ctx, integrations.Notification{
		Category: integrations.NotificationCategoryBackup,
		Severity: integrations.NotificationSeverityError,
		Subject:  "[CDN] 备份任务失败",
		Message:  "自动备份任务未能完成。",
		Details: []integrations.NotificationDetail{
			{Label: "主机", Value: status.Host},
			{Label: "开始时间", Value: status.StartedAt.Format(time.RFC3339)},
			{Label: "失败时间", Value: status.UpdatedAt.Format(time.RFC3339)},
			{Label: "重试次数", Value: fmt.Sprintf("%d/%d", status.Attempt, status.MaxAttempts)},
			{Label: "错误", Value: status.Error},
		},
		OccurredAt: status.UpdatedAt,
		Key:        "backup:failure",
		Cooldown:   5 * time.Minute,
	})
}

func writePrivateRuntimeFile(directory, name, value string) error {
	temporary, err := os.CreateTemp(directory, "."+name+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, name))
}

func backupRuntimeFromEnv() (control.BackupRuntime, error) {
	randomDelay := domain.DefaultBackupRandomDelaySeconds
	var environmentErr error
	if value := strings.TrimSpace(os.Getenv("BACKUP_RANDOM_DELAY_SECONDS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			randomDelay = -1
			environmentErr = errors.New("BACKUP_RANDOM_DELAY_SECONDS must be an integer")
		} else {
			randomDelay = parsed
		}
	}
	password := os.Getenv("RESTIC_PASSWORD")
	if password == "" {
		passwordFile := strings.TrimSpace(os.Getenv("RESTIC_PASSWORD_FILE"))
		if passwordFile != "" {
			contents, err := os.ReadFile(passwordFile)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) && environmentErr == nil {
					environmentErr = fmt.Errorf("read RESTIC_PASSWORD_FILE: %w", err)
				}
			} else {
				password = strings.TrimRight(string(contents), "\r\n")
			}
		}
	}
	return control.BackupRuntime{
		Settings: domain.NormalizeBackupSettings(domain.BackupSettings{
			Repository:         os.Getenv("RESTIC_REPOSITORY"),
			AccessKeyID:        os.Getenv("AWS_ACCESS_KEY_ID"),
			Region:             env("AWS_DEFAULT_REGION", domain.DefaultBackupRegion),
			BackupTime:         env("BACKUP_TIME", domain.DefaultBackupTime),
			RandomDelaySeconds: randomDelay,
		}),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		ResticPassword:  password,
	}, environmentErr
}

func settingsFromEnv() (control.EnvironmentSettings, error) {
	recipients := split(os.Getenv("SMTP_TO"))
	security := strings.ToLower(env("SMTP_SECURITY", integrations.SMTPSecurityStartTLS))
	if security != integrations.SMTPSecurityStartTLS && security != integrations.SMTPSecurityTLS {
		if len(recipients) != 0 {
			return control.EnvironmentSettings{}, fmt.Errorf("SMTP_SECURITY must be starttls or tls")
		}
		security = integrations.SMTPSecurityStartTLS
	}
	port := 587
	if security == integrations.SMTPSecurityTLS {
		port = 465
	}
	if value := strings.TrimSpace(os.Getenv("SMTP_PORT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 65535 {
			if len(recipients) != 0 {
				return control.EnvironmentSettings{}, fmt.Errorf("SMTP_PORT must be between 1 and 65535")
			}
		} else {
			port = parsed
		}
	}
	// Backup is optional, and a database override must remain usable even when
	// the environment fallback is incomplete or stale. The backup command
	// reports fallback errors when it actually needs the environment values.
	backup, _ := backupRuntimeFromEnv()
	return control.EnvironmentSettings{
		CloudflareAPIToken: os.Getenv("CLOUDFLARE_API_TOKEN"),
		SMTP:               control.SMTPProfile{Enabled: len(recipients) != 0, Host: os.Getenv("SMTP_HOST"), Port: port, Username: os.Getenv("SMTP_USER"), FromAddress: os.Getenv("SMTP_FROM"), Recipients: recipients, Security: security},
		SMTPPassword:       os.Getenv("SMTP_PASSWORD"),
		Backup:             backup.Settings,
		BackupAccessKey:    backup.SecretAccessKey,
		BackupPassword:     backup.ResticPassword,
	}, nil
}
func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnvironment(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		fatal(name + " must be a positive Go duration")
	}
	return duration
}
func split(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
func parseCIDRs(value string) ([]*net.IPNet, error) {
	var result []*net.IPNet
	for _, item := range split(value) {
		_, cidr, err := net.ParseCIDR(item)
		if err != nil {
			return nil, err
		}
		result = append(result, cidr)
	}
	return result, nil
}
func fatal(message string) { fmt.Fprintln(os.Stderr, "cdn-control:", message); os.Exit(1) }
