package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/control"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestWriteCloudflareCredentialsUsesDatabaseOverride(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CONTROL_DATA_DIR", dataDir)
	t.Setenv("CLOUDFLARE_API_TOKEN", "environment-token")
	key, err := control.NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_ENCRYPTION_KEY", key)
	cipher, err := control.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ciphertext, err := cipher.Encrypt([]byte("database-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetSecret(store.SecretCloudflareAPIToken, ciphertext); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cloudflare.ini")
	writeCloudflareCredentials(path)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "dns_cloudflare_api_token = database-token\n" || strings.Contains(string(contents), "environment-token") {
		t.Fatalf("credentials = %q", contents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %o", info.Mode().Perm())
	}
	rotated, err := cipher.Encrypt([]byte("rotated-database-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetSecret(store.SecretCloudflareAPIToken, rotated); err != nil {
		t.Fatal(err)
	}
	writeCloudflareCredentials(path)
	contents, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "dns_cloudflare_api_token = rotated-database-token\n" {
		t.Fatalf("rotated credentials = %q", contents)
	}
}

func TestWriteCloudflareCredentialsFallsBackToEnvironment(t *testing.T) {
	t.Setenv("CONTROL_DATA_DIR", t.TempDir())
	t.Setenv("CLOUDFLARE_API_TOKEN", "environment-token")
	path := filepath.Join(t.TempDir(), "cloudflare.ini")
	writeCloudflareCredentials(path)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "dns_cloudflare_api_token = environment-token\n" {
		t.Fatalf("credentials = %q", contents)
	}
}

func TestWriteBackupRuntimeUsesDatabaseOverrideBeforeEnvironment(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CONTROL_DATA_DIR", dataDir)
	t.Setenv("RESTIC_REPOSITORY", "s3:https://environment.example.test/environment-bucket")
	t.Setenv("AWS_ACCESS_KEY_ID", "environment-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "environment-secret-key")
	t.Setenv("AWS_DEFAULT_REGION", "environment-region")
	t.Setenv("RESTIC_PASSWORD_FILE", filepath.Join(t.TempDir(), "missing-password"))
	t.Setenv("BACKUP_RANDOM_DELAY_SECONDS", "not-an-integer")
	key, err := control.NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_ENCRYPTION_KEY", key)
	cipher, err := control.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := control.NewSettingsManager(database, cipher, control.EnvironmentSettings{})
	if err != nil {
		t.Fatal(err)
	}
	secretAccessKey := "database-secret-key"
	resticPassword := "database-restic-password"
	databaseSettings := domain.BackupSettings{
		Repository:         "s3:https://database.example.test/database-bucket",
		AccessKeyID:        "database-access-key",
		Region:             "auto",
		BackupTime:         "04:20",
		RandomDelaySeconds: 600,
	}
	if err := manager.SaveBackup(databaseSettings, &secretAccessKey, &resticPassword); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	writeBackupRuntime(runtimeDir)
	assertBackupRuntimeFiles(t, runtimeDir, map[string]string{
		"repository":           databaseSettings.Repository,
		"access-key-id":        databaseSettings.AccessKeyID,
		"secret-access-key":    secretAccessKey,
		"region":               databaseSettings.Region,
		"restic-password":      resticPassword,
		"backup-time":          databaseSettings.BackupTime,
		"random-delay-seconds": "600",
		"source":               control.SettingsSourceDatabase,
	})
}

func TestWriteBackupRuntimeFallsBackToEnvironment(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CONTROL_DATA_DIR", dataDir)
	t.Setenv("RESTIC_REPOSITORY", "s3:https://environment.example.test/environment-bucket")
	t.Setenv("AWS_ACCESS_KEY_ID", "environment-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "environment-secret-key")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("BACKUP_TIME", "05:10")
	t.Setenv("BACKUP_RANDOM_DELAY_SECONDS", "300")
	passwordPath := filepath.Join(t.TempDir(), "restic-password")
	if err := os.WriteFile(passwordPath, []byte("environment-restic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTIC_PASSWORD_FILE", passwordPath)
	key, err := control.NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_ENCRYPTION_KEY", key)
	database, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	writeBackupRuntime(runtimeDir)
	assertBackupRuntimeFiles(t, runtimeDir, map[string]string{
		"repository":           "s3:https://environment.example.test/environment-bucket",
		"access-key-id":        "environment-access-key",
		"secret-access-key":    "environment-secret-key",
		"region":               "us-east-1",
		"restic-password":      "environment-restic-password",
		"backup-time":          "05:10",
		"random-delay-seconds": "300",
		"source":               control.SettingsSourceEnvironment,
	})
}

func assertBackupRuntimeFiles(t *testing.T, directory string, expected map[string]string) {
	t.Helper()
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory mode = %o", info.Mode().Perm())
	}
	for name, want := range expected {
		path := filepath.Join(directory, name)
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got := string(contents); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
}

func TestSettingsFromEnvUsesTLSDefaultsAndRejectsUnsafeModes(t *testing.T) {
	t.Setenv("SMTP_TO", "ops@example.test")
	t.Setenv("SMTP_SECURITY", "tls")
	t.Setenv("SMTP_PORT", "")
	settings, err := settingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.SMTP.Enabled || settings.SMTP.Port != 465 || settings.SMTP.Security != "tls" {
		t.Fatalf("implicit TLS environment = %#v", settings.SMTP)
	}
	t.Setenv("SMTP_SECURITY", "none")
	if _, err := settingsFromEnv(); err == nil {
		t.Fatal("accepted plaintext SMTP environment")
	}
}

func TestSettingsFromEnvDoesNotLetInvalidOptionalBackupBlockControl(t *testing.T) {
	t.Setenv("BACKUP_RANDOM_DELAY_SECONDS", "invalid")
	settings, err := settingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Backup.RandomDelaySeconds != -1 {
		t.Fatalf("invalid backup fallback marker = %d", settings.Backup.RandomDelaySeconds)
	}
}

func TestPublishSiteCommandPublishesOnlySelectedSite(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CONTROL_DATA_DIR", dataDir)
	key, err := control.NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_ENCRYPTION_KEY", key)
	database, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge", "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	target, err := database.CreateSite(controlTestSite("target", "target.example.test", node.ID), "zone")
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.CreateSite(controlTestSite("other", "other.example.test", node.ID), "zone")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	publishSite(target.ID)

	database, err = store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.SitePublication(target.ID); err != nil {
		t.Fatalf("target site was not published: %v", err)
	}
	if _, err := database.SitePublication(other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unrelated site publication = %v", err)
	}
}

func controlTestSite(name, domainName, nodeID string) domain.Site {
	return domain.Site{
		Name: name, Domains: []string{domainName}, Nodes: []string{nodeID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       false,
	}
}

func TestComposeCertbotRefreshesCredentialsForEveryCertificateOperation(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "compose-certbot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	if !strings.Contains(script, `cdn-control cloudflare-credentials "$credentials"`) {
		t.Fatal("compose certbot does not load runtime Cloudflare credentials")
	}
	if count := strings.Count(script, "  write_credentials\n"); count != 2 {
		t.Fatalf("credential refresh count = %d, want issue and renew", count)
	}
	if strings.Contains(script, "dns_cloudflare_api_token = %s") {
		t.Fatal("compose certbot still writes the environment token directly")
	}
}

func TestComposeBackupCommandsResolveRuntimeSettings(t *testing.T) {
	repositoryRoot := filepath.Join("..", "..")
	commonContents, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "compose-backup-common.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commonContents), `cdn-control backup-runtime "$runtime_dir"`) {
		t.Fatal("backup runtime loader does not resolve database-first settings")
	}
	for _, name := range []string{"compose-backup.sh", "compose-backup-loop.sh", "compose-backup-restic.sh"} {
		contents, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		script := string(contents)
		if !strings.Contains(script, "compose-backup-common.sh") || !strings.Contains(script, `load_backup_runtime "$runtime_dir"`) {
			t.Fatalf("%s bypasses the effective backup settings", name)
		}
	}
	loopContents, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "compose-backup-loop.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loopContents), "if ((sleep_seconds > 60))") {
		t.Fatal("backup scheduler does not bound settings refresh latency")
	}
	composeContents, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy", "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	compose := string(composeContents)
	if strings.Count(compose, "- ./config/backup.env") < 2 || strings.Count(compose, "- ./config/control.env") < 3 || !strings.Contains(compose, "./config/restic-password:/deployment/config/restic-password:ro") {
		t.Fatal("Compose does not provide backup fallbacks and decryption material to both services")
	}
	if !strings.Contains(compose, "./backup/status:/var/lib/cdn-platform-operations:ro") || !strings.Contains(compose, "./backup/status:/var/lib/cdn-platform-operations\n") {
		t.Fatal("Compose does not share backup status with the controller")
	}
	if !strings.Contains(compose, "\n  backup:\n") || strings.Contains(compose, `profiles: ["backup"]`) {
		t.Fatal("Compose does not start the required backup service by default")
	}
}

func TestComposeDeploymentPullsPublishedImageWithoutHostBuild(t *testing.T) {
	repositoryRoot := filepath.Join("..", "..")
	composeContents, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy", "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	compose := string(composeContents)
	if !strings.Contains(compose, "image: ${CDN_CONTROL_IMAGE:-ghcr.io/saginardo/simple_cdn:main}") {
		t.Fatal("Compose does not default to the GitHub Actions image")
	}
	if strings.Contains(compose, "build:") || strings.Contains(compose, "CDN_SOURCE_DIR") {
		t.Fatal("Compose still permits a control-plane source build")
	}
	if !strings.Contains(compose, "./.env:/deployment/.env:ro") || strings.Contains(compose, "/deployment/Dockerfile") {
		t.Fatal("Compose backup inputs do not record the deployed image reference")
	}

	installerContents, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "install-control-compose.sh"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerContents)
	if !strings.Contains(installer, "CDN_CONTROL_IMAGE=%s") || !strings.Contains(installer, "docker compose pull") || strings.Contains(installer, "docker compose build") {
		t.Fatal("Compose installer does not enforce pull-only delivery")
	}

	backupContents, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "compose-backup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	backup := string(backupContents)
	if !strings.Contains(backup, "/deployment/.env") || strings.Contains(backup, "/deployment/Dockerfile") {
		t.Fatal("backup snapshot does not preserve the immutable image reference")
	}
}

func TestGitHubActionsBuildPublishesImmutableImageWithoutProductionConfig(t *testing.T) {
	repositoryRoot := filepath.Join("..", "..")
	workflowContents, err := os.ReadFile(filepath.Join(repositoryRoot, ".github", "workflows", "ci-cd.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowContents)
	for _, expected := range []string{
		"pull_request:",
		"go test ./...",
		"npm --prefix frontend run check",
		"docker/build-push-action@",
		"packages: write",
		"provenance: mode=max",
		"sbom: true",
		"type=raw,value=sha-${{ github.sha }}",
		"Published \\`${IMAGE_NAME}@${IMAGE_DIGEST}\\`",
	} {
		if !strings.Contains(workflow, expected) {
			t.Fatalf("CI/CD workflow is missing %q", expected)
		}
	}
	for _, forbidden := range []string{"environment:", "known_hosts", "StrictHostKeyChecking", " scp ", " ssh "} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("public CI/CD workflow contains private deployment configuration %q", forbidden)
		}
	}

	deployContents, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "deploy-control-compose.sh"))
	if err != nil {
		t.Fatal(err)
	}
	deploy := string(deployContents)
	for _, expected := range []string{"docker pull \"$image_ref\"", "--no-build", "wait_for_control", "rollback_deployment", "running_image", "prune_obsolete_control_images", "docker image rm", "control, clickhouse, and backup must all be running"} {
		if !strings.Contains(deploy, expected) {
			t.Fatalf("deployment script is missing %q", expected)
		}
	}
	if strings.Contains(deploy, "docker compose build") || strings.Contains(deploy, "docker image prune") {
		t.Fatal("deployment script can still build on the control host or prune unrelated images")
	}
}

func TestComposeRestoreStagesAndValidatesBeforeBoundedCutover(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "restore-control-compose.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	for _, expected := range []string{
		"--verify-only",
		"PRAGMA quick_check;",
		"RESTORE DATABASE $restored_source_database AS $temporary_database",
		"CHECK TABLE $temporary_database.$table",
		"check_query_single_value_result=1",
		"RESTORE_CLICKHOUSE_READY_TIMEOUT_SECONDS",
		"RENAME DATABASE $previous_clickhouse_database TO $rollback_database",
		"RENAME DATABASE $temporary_database TO $clickhouse_database",
		"rollback_cutover",
		"live data was not changed",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("restore script is missing %q", expected)
		}
	}
	if strings.Contains(script, "until docker compose exec -T clickhouse clickhouse-client --query 'SELECT 1' >/dev/null 2>&1; do sleep 2; done") {
		t.Fatal("restore script still waits indefinitely for ClickHouse")
	}
}

func TestComposeBackupRuntimeLoaderCanRefreshRepeatedly(t *testing.T) {
	directory := t.TempDir()
	binDirectory := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeControl := filepath.Join(binDirectory, "cdn-control")
	fakeScript := `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == "backup-runtime" ]]
[[ "$RESTIC_PASSWORD_FILE" == "$EXPECTED_PASSWORD_FILE" ]]
printf '%s\n' "$RESTIC_PASSWORD_FILE" >> "$CALL_LOG"
runtime_dir="$2"
mkdir -p "$runtime_dir"
printf '%s' 's3:https://effective.example.test/bucket' > "$runtime_dir/repository"
printf '%s' 'effective-access' > "$runtime_dir/access-key-id"
printf '%s' 'effective-secret' > "$runtime_dir/secret-access-key"
printf '%s' 'auto' > "$runtime_dir/region"
printf '%s' 'effective-password' > "$runtime_dir/restic-password"
printf '%s' '04:20' > "$runtime_dir/backup-time"
printf '%s' '600' > "$runtime_dir/random-delay-seconds"
printf '%s' 'environment' > "$runtime_dir/source"
`
	if err := os.WriteFile(fakeControl, []byte(fakeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDirectory := filepath.Join(directory, "runtime")
	if err := os.Mkdir(runtimeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(directory, "fallback-password")
	if err := os.WriteFile(passwordFile, []byte("fallback-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(directory, "calls")
	commonScript := filepath.Join("..", "..", "scripts", "compose-backup-common.sh")
	command := exec.Command("bash", "-c", `
set -euo pipefail
source "$COMMON_SCRIPT"
load_backup_runtime "$RUNTIME_DIRECTORY"
[[ "$RESTIC_PASSWORD_FILE" == "$RUNTIME_DIRECTORY/restic-password" ]]
[[ -z "${RESTIC_PASSWORD+x}" ]]
rm -f "$RUNTIME_DIRECTORY"/*
load_backup_runtime "$RUNTIME_DIRECTORY"
[[ "$RESTIC_PASSWORD_FILE" == "$RUNTIME_DIRECTORY/restic-password" ]]
[[ -z "${RESTIC_PASSWORD+x}" ]]
`)
	command.Env = append(os.Environ(),
		"PATH="+binDirectory+":"+os.Getenv("PATH"),
		"COMMON_SCRIPT="+commonScript,
		"RUNTIME_DIRECTORY="+runtimeDirectory,
		"EXPECTED_PASSWORD_FILE="+passwordFile,
		"CALL_LOG="+callLog,
		"RESTIC_REPOSITORY=s3:https://fallback.example.test/bucket",
		"RESTIC_PASSWORD=",
		"RESTIC_PASSWORD_FILE="+passwordFile,
		"AWS_ACCESS_KEY_ID=fallback-access",
		"AWS_SECRET_ACCESS_KEY=fallback-secret",
		"AWS_DEFAULT_REGION=auto",
		"BACKUP_TIME=03:25",
		"BACKUP_RANDOM_DELAY_SECONDS=1200",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("reload backup runtime: %v\n%s", err, output)
	}
	contents, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Split(strings.TrimSpace(string(contents)), "\n"), []string{passwordFile, passwordFile}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("fallback password paths = %#v, want %#v", got, want)
	}
}
