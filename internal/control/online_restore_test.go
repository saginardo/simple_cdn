package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/project"
	"simple_cdn/internal/store"
)

type fakeRestoreClickHouse struct {
	mu        sync.Mutex
	databases map[string]bool
}

type responseLostRestoreClickHouse struct {
	*fakeRestoreClickHouse
}

func (f *responseLostRestoreClickHouse) RenameDatabase(ctx context.Context, source, target string) error {
	if err := f.fakeRestoreClickHouse.RenameDatabase(ctx, source, target); err != nil {
		return err
	}
	return errors.New("simulated lost ClickHouse response")
}

func (f *fakeRestoreClickHouse) DatabaseExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.databases[name], nil
}

func (f *fakeRestoreClickHouse) RestoreDatabase(_ context.Context, source, target, diskPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if (source != project.ClickHouseDatabase && source != project.LegacyClickHouseDatabase) || !strings.Contains(diskPath, "online-restore/jobs/") {
		return errors.New("invalid fake restore request")
	}
	f.databases[target] = true
	return nil
}

func (f *fakeRestoreClickHouse) ValidateDatabase(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.databases[name] {
		return errors.New("database does not exist")
	}
	return nil
}

func (f *fakeRestoreClickHouse) RenameDatabase(_ context.Context, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.databases[source] || f.databases[target] {
		return errors.New("invalid fake database rename")
	}
	delete(f.databases, source)
	f.databases[target] = true
	return nil
}

func (f *fakeRestoreClickHouse) DropDatabase(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.databases, name)
	return nil
}

func TestOnlineRestoreStagesCommitsAndAppliesVerifiedSnapshot(t *testing.T) {
	temporary := t.TempDir()
	fixtureRoot := filepath.Join(temporary, "fixture")
	controlFixture := filepath.Join(fixtureRoot, "backup", "staging", "control")
	clickHouseFixture := filepath.Join(fixtureRoot, "backup", "staging", "clickhouse", "cdn-platform-current")
	if err := os.MkdirAll(controlFixture, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(clickHouseFixture, 0o700); err != nil {
		t.Fatal(err)
	}

	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	restoredDatabase, err := store.Open(filepath.Join(controlFixture, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.Encrypt([]byte("restored-token"))
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredDatabase.SetSecret(store.SecretCloudflareAPIToken, ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := restoredDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	restoredSecrets := filepath.Join(temporary, "restored-secrets")
	if _, err := LoadOrCreateInternalCA(filepath.Join(restoredSecrets, "pki")); err != nil {
		t.Fatal(err)
	}
	restoredNginxDirectory := filepath.Join(restoredSecrets, "nginx-artifacts")
	if err := os.MkdirAll(restoredNginxDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	restoredBundlePath := filepath.Join(temporary, "restored-nginx.tar.gz")
	writeTestNginxArchive(t, restoredBundlePath, validTestNginxEntries("1.30.5"))
	restoredBundle, err := os.ReadFile(restoredBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	restoredMetadata, err := ResolveNginxBundle(restoredBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	restoredNginxName := restoredMetadata.SHA256 + ".tar.gz"
	if err := os.WriteFile(filepath.Join(restoredNginxDirectory, restoredNginxName), restoredBundle, 0o640); err != nil {
		t.Fatal(err)
	}
	restoredDatabase, err = store.Open(filepath.Join(controlFixture, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	restoredArtifact := domain.NginxArtifact{
		SHA256: restoredMetadata.SHA256, Version: restoredMetadata.Version, State: domain.NginxArtifactCandidate,
		ReleaseTag: "nginx-v1.30.5", SourceURL: "https://downloads.example.test/restored",
		OfficialSourceURL: "https://nginx.org/download/nginx-1.30.5.tar.gz",
		SourceSHA256:      strings.Repeat("e", 64), BuildCommit: strings.Repeat("f", 40),
		SizeBytes: int64(len(restoredBundle)), DownloadedAt: time.Now().UTC(),
	}
	if _, _, err := restoredDatabase.SaveNginxArtifactCandidate(restoredArtifact); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restoredDatabase.PromoteNginxArtifact(restoredArtifact.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := restoredDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeRestoreTestArchive(filepath.Join(controlFixture, "control-secrets.tar.gz"), restoredSecrets); err != nil {
		t.Fatal(err)
	}
	emptyTLS := filepath.Join(temporary, "empty-tls")
	if err := os.MkdirAll(emptyTLS, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeRestoreTestArchive(filepath.Join(controlFixture, "control-tls.tar.gz"), emptyTLS); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clickHouseFixture, ".backup"), []byte("metadata"), 0o600); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(temporary, "data")
	tlsDir := filepath.Join(temporary, "tls")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	liveDatabase, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := liveDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateInternalCA(filepath.Join(dataDir, "pki")); err != nil {
		t.Fatal(err)
	}
	liveNginxDirectory := filepath.Join(dataDir, "nginx-artifacts")
	if err := os.MkdirAll(liveNginxDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveNginxDirectory, "old.tar.gz"), []byte("old-nginx-artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "old.pem"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshotID := strings.Repeat("a", 64)
	resticPath := filepath.Join(temporary, "restic")
	resticScript := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  snapshots)
	printf '%s\n' "$*" >>"$RESTIC_CALLS"
    printf '[{"id":"%s","short_id":"aaaaaaaa","time":"2026-07-18T01:02:03Z","tags":["cdn-control-compose"]}]' "$SNAPSHOT_ID"
    ;;
  restore)
    shift
    target=""
    while (($#)); do
      if [[ "$1" == "--target" ]]; then target="$2"; shift 2; else shift; fi
    done
    mkdir -p "$target"
    cp -a "$FIXTURE_ROOT/." "$target/"
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(resticPath, []byte(resticScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SNAPSHOT_ID", snapshotID)
	t.Setenv("FIXTURE_ROOT", fixtureRoot)
	resticCalls := filepath.Join(temporary, "restic-calls")
	t.Setenv("RESTIC_CALLS", resticCalls)

	settingsDatabase, err := store.Open(filepath.Join(temporary, "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer settingsDatabase.Close()
	settings, err := NewSettingsManager(settingsDatabase, cipher, EnvironmentSettings{
		Backup: domain.BackupSettings{
			Repository:  "s3:https://s3.example.test/backups",
			AccessKeyID: "access-key",
			Region:      "us-east-1",
			BackupTime:  "03:25",
		},
		BackupAccessKey: "secret-key",
		BackupPassword:  "repository-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	clickHouse := &fakeRestoreClickHouse{databases: map[string]bool{project.ClickHouseDatabase: true}}
	restoreRoot := filepath.Join(temporary, "online-restore")
	manager, err := NewOnlineRestoreManager(OnlineRestoreManagerConfig{
		Root:              restoreRoot,
		Settings:          settings,
		Cipher:            cipher,
		ClickHouse:        clickHouse,
		ResticBinary:      resticPath,
		ClickHouseGroupID: -1,
		RestoreTimeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshots, err := manager.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != snapshotID {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	calls, err := os.ReadFile(resticCalls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "snapshots --no-lock --json --tag cdn-control-compose") {
		t.Fatalf("snapshot listing acquired a repository lock: %s", calls)
	}
	job, err := manager.Start(snapshotID, snapshotID[:8])
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		current := manager.Current()
		if current != nil && current.State == OnlineRestoreReady {
			job = *current
			break
		}
		if current != nil && current.State == OnlineRestoreFailed {
			t.Fatalf("restore preparation failed: %s", current.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("restore did not become ready: %#v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.DatabaseSHA256 == "" || job.CAFingerprint == "" || job.SchemaVersion != store.LatestSchemaVersion() || job.Database != project.ClickHouseDatabase || job.SourceDatabase != project.LegacyClickHouseDatabase {
		t.Fatalf("verified job = %#v", job)
	}
	job, err = manager.Commit(job.ID, "RESTORE")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != OnlineRestoreCommitting {
		t.Fatalf("committing job = %#v", job)
	}
	manager.Stop()

	applied, err := ApplyPendingOnlineRestore(context.Background(), OnlineRestoreApplyConfig{
		Root:         restoreRoot,
		DataDir:      dataDir,
		TLSDir:       tlsDir,
		Cipher:       cipher,
		ClickHouse:   clickHouse,
		ApplyTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("pending restore was not applied")
	}
	if got, err := fileSHA256(filepath.Join(dataDir, "control.db")); err != nil || got != job.DatabaseSHA256 {
		t.Fatalf("promoted database hash = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "control.db.before-restore-"+job.ID)); err != nil {
		t.Fatalf("previous SQLite database was not retained: %v", err)
	}
	restoredNginx, err := os.ReadFile(filepath.Join(dataDir, "nginx-artifacts", restoredNginxName))
	if err != nil || !bytes.Equal(restoredNginx, restoredBundle) {
		t.Fatalf("managed Nginx artifact was not restored: size=%d, err=%v", len(restoredNginx), err)
	}
	previousNginx, err := os.ReadFile(filepath.Join(dataDir, "nginx-artifacts.before-restore-"+job.ID, "old.tar.gz"))
	if err != nil || string(previousNginx) != "old-nginx-artifact" {
		t.Fatalf("previous managed Nginx artifacts were not retained: contents=%q, err=%v", previousNginx, err)
	}
	restoredDatabase, err = store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	restoredCurrent, err := restoredDatabase.CurrentNginxArtifact()
	if err != nil || restoredCurrent.SHA256 != restoredMetadata.SHA256 || restoredCurrent.State != domain.NginxArtifactCurrent {
		t.Fatalf("restored Nginx catalog = %#v, err=%v", restoredCurrent, err)
	}
	if err := restoredDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tlsDir, "before-restore-"+job.ID, "old.pem")); err != nil {
		t.Fatalf("previous TLS state was not retained: %v", err)
	}
	if exists, _ := clickHouse.DatabaseExists(context.Background(), job.RollbackDatabase); !exists {
		t.Fatal("previous ClickHouse database was not retained")
	}
	if _, err := os.Stat(onlineRestoreMaintenancePath(restoreRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("maintenance lock still exists: %v", err)
	}
	completed, err := readOnlineRestoreJob(restoreRoot)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != OnlineRestoreCompleted {
		t.Fatalf("completed job = %#v", completed)
	}
}

func TestBackupSnapshotDeleteAPIValidatesConflictsAndPrunes(t *testing.T) {
	temporary := t.TempDir()
	snapshotID := strings.Repeat("b", 64)
	resticPath := filepath.Join(temporary, "restic")
	resticScript := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$RESTIC_CALLS"
case "$1" in
  snapshots)
    printf '[{"id":"%s","short_id":"bbbbbbbb","time":"2026-07-18T01:02:03Z","tags":["cdn-control-compose"]}]' "$SNAPSHOT_ID"
    ;;
  forget)
    [[ "$2" == "$SNAPSHOT_ID" && "$3" == "--prune" ]]
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(resticPath, []byte(resticScript), 0o700); err != nil {
		t.Fatal(err)
	}
	resticCalls := filepath.Join(temporary, "restic-calls")
	t.Setenv("RESTIC_CALLS", resticCalls)
	t.Setenv("SNAPSHOT_ID", snapshotID)

	database, err := store.Open(filepath.Join(temporary, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "totp"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettingsManager(database, cipher, EnvironmentSettings{
		Backup: domain.BackupSettings{
			Repository:  "s3:https://s3.example.test/backups",
			AccessKeyID: "access-key",
			Region:      "us-east-1",
			BackupTime:  "03:25",
		},
		BackupAccessKey: "secret-key",
		BackupPassword:  "repository-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewOnlineRestoreManager(OnlineRestoreManagerConfig{
		Root:                  filepath.Join(temporary, "online-restore"),
		Settings:              settings,
		Cipher:                cipher,
		ClickHouse:            &fakeRestoreClickHouse{databases: map[string]bool{project.ClickHouseDatabase: true}},
		ResticBinary:          resticPath,
		ClickHouseGroupID:     -1,
		SnapshotDeleteTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	server := &Server{Store: database, OnlineRestore: manager}
	deleteSnapshot := func(id, confirmation string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(deleteBackupSnapshotRequest{Confirmation: confirmation})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodDelete, "/api/backups/snapshots/"+id, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", "csrf-token")
		request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	response := deleteSnapshot(snapshotID, "wrong")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid confirmation = %d %s", response.Code, response.Body.String())
	}
	missingID := strings.Repeat("c", 64)
	response = deleteSnapshot(missingID, missingID[:8])
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing snapshot = %d %s", response.Code, response.Body.String())
	}
	manager.mu.Lock()
	manager.job = &OnlineRestoreJob{State: OnlineRestoreReady}
	manager.mu.Unlock()
	response = deleteSnapshot(snapshotID, snapshotID[:8])
	if response.Code != http.StatusConflict {
		t.Fatalf("active restore conflict = %d %s", response.Code, response.Body.String())
	}
	manager.mu.Lock()
	manager.job = nil
	manager.mu.Unlock()
	response = deleteSnapshot(snapshotID, snapshotID[:8])
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), snapshotID) {
		t.Fatalf("delete snapshot = %d %s", response.Code, response.Body.String())
	}
	calls, err := os.ReadFile(resticCalls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "forget ") != 1 || !strings.Contains(string(calls), "forget "+snapshotID+" --prune") {
		t.Fatalf("snapshot was not forgotten and pruned exactly once: %s", calls)
	}
}

func TestExtractRestoreArchiveRejectsTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "unsafe.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "../outside", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractRestoreArchive(archivePath, filepath.Join(t.TempDir(), "target")); err == nil {
		t.Fatal("accepted path traversal in restore archive")
	}
}

func TestRestorePathSwapResumesAndRollsBackAppliedState(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	staged := filepath.Join(root, "staged")
	backup := filepath.Join(root, "backup")
	if err := os.WriteFile(live, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&restorePathSwap{live: live, staged: staged, backup: backup}).apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := &restorePathSwap{live: live, staged: staged, backup: backup}
	if err := resumed.apply(); err != nil {
		t.Fatal(err)
	}
	if err := resumed.rollback(); err != nil {
		t.Fatal(err)
	}
	assertRestoreFileContents(t, live, "old")
	assertRestoreFileContents(t, staged, "new")
}

func TestRestorePathSwapTracksPreviouslyAbsentLivePath(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	staged := filepath.Join(root, "staged")
	backup := filepath.Join(root, "backup")
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&restorePathSwap{live: live, staged: staged, backup: backup}).apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := &restorePathSwap{live: live, staged: staged, backup: backup}
	if err := resumed.apply(); err != nil {
		t.Fatal(err)
	}
	if err := resumed.rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(live); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previously absent live path was restored: %v", err)
	}
	assertRestoreFileContents(t, staged, "new")
	if _, err := os.Lstat(backup + ".absent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absence marker remains after rollback: %v", err)
	}
}

func TestRestoreTLSCutoverResumesAndRollsBackAppliedState(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, ".stage")
	backup := filepath.Join(root, "before-restore-job")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "live.pem"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "live.pem"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "live.pem"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	cutover := &restoreTLSCutover{root: root, stage: stage, backup: backup}
	if err := cutover.apply(); err != nil {
		t.Fatal(err)
	}
	if err := cutover.rollback(); err != nil {
		t.Fatal(err)
	}
	assertRestoreFileContents(t, filepath.Join(root, "live.pem"), "old")
	assertRestoreFileContents(t, filepath.Join(stage, "live.pem"), "new")
}

func TestRestoreTLSCutoverRetainsAmbiguousPartialState(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, ".stage")
	backup := filepath.Join(root, "before-restore-job")
	for _, directory := range []string{stage, backup} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "pending.pem"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "live.pem"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&restoreTLSCutover{root: root, stage: stage, backup: backup}).apply()
	if !errors.Is(err, errOnlineRestoreManualRollback) {
		t.Fatalf("partial TLS cutover error = %v", err)
	}
}

func TestPromoteRestoreClickHouseObservesRenameAfterLostResponse(t *testing.T) {
	clickHouse := &responseLostRestoreClickHouse{fakeRestoreClickHouse: &fakeRestoreClickHouse{databases: map[string]bool{
		project.ClickHouseDatabase: true,
		"restore_temp":             true,
	}}}
	job := &OnlineRestoreJob{Database: project.ClickHouseDatabase, TemporaryDatabase: "restore_temp", RollbackDatabase: "restore_old"}
	promoted, oldRenamed, err := promoteRestoreClickHouse(context.Background(), clickHouse, job)
	if err != nil {
		t.Fatal(err)
	}
	if !promoted || !oldRenamed {
		t.Fatalf("promotion state = promoted %v, old renamed %v", promoted, oldRenamed)
	}
	if exists, _ := clickHouse.DatabaseExists(context.Background(), project.ClickHouseDatabase); !exists {
		t.Fatal("restored database was not promoted")
	}
	if exists, _ := clickHouse.DatabaseExists(context.Background(), "restore_old"); !exists {
		t.Fatal("previous database was not retained")
	}
}

func TestMigrateLegacyClickHouseDatabase(t *testing.T) {
	clickHouse := &fakeRestoreClickHouse{databases: map[string]bool{project.LegacyClickHouseDatabase: true}}
	migrated, err := MigrateLegacyClickHouseDatabase(context.Background(), clickHouse)
	if err != nil || !migrated {
		t.Fatalf("migration = %v, err = %v", migrated, err)
	}
	if exists, _ := clickHouse.DatabaseExists(context.Background(), project.ClickHouseDatabase); !exists {
		t.Fatal("current ClickHouse database does not exist after migration")
	}
	if exists, _ := clickHouse.DatabaseExists(context.Background(), project.LegacyClickHouseDatabase); exists {
		t.Fatal("legacy ClickHouse database still exists after migration")
	}
}

func TestMigrateLegacyClickHouseDatabaseRejectsAmbiguousState(t *testing.T) {
	clickHouse := &fakeRestoreClickHouse{databases: map[string]bool{
		project.ClickHouseDatabase:       true,
		project.LegacyClickHouseDatabase: true,
	}}
	if migrated, err := MigrateLegacyClickHouseDatabase(context.Background(), clickHouse); err == nil || migrated {
		t.Fatalf("ambiguous migration = %v, err = %v", migrated, err)
	}
}

func TestOnlineRestoreMaintenanceLockIsExclusive(t *testing.T) {
	root := t.TempDir()
	if err := writeOnlineRestoreMaintenanceLock(root, "job-a"); err != nil {
		t.Fatal(err)
	}
	if err := writeOnlineRestoreMaintenanceLock(root, "job-b"); err == nil {
		t.Fatal("a second restore replaced the active maintenance marker")
	}
	contents, err := os.ReadFile(onlineRestoreMaintenancePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "job-a") || strings.Contains(string(contents), "job-b") {
		t.Fatalf("maintenance marker = %s", contents)
	}
	if err := removeOnlineRestoreMaintenanceLock(root, "job-b"); err == nil {
		t.Fatal("another job removed the active maintenance marker")
	}
	if err := removeOnlineRestoreMaintenanceLock(root, "job-a"); err != nil {
		t.Fatal(err)
	}
}

func assertRestoreFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("%s = %q, want %q", path, contents, want)
	}
}

func writeRestoreTestArchive(path, root string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(archive, input)
		closeErr := input.Close()
		return errors.Join(copyErr, closeErr)
	})
	return errors.Join(walkErr, archive.Close(), compressed.Close(), file.Close())
}
