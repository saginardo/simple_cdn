package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func gcTestNginxArtifact(version, digest string) domain.NginxArtifact {
	return domain.NginxArtifact{
		SHA256: digest, Version: version, ReleaseTag: "nginx-v" + version,
		SourceURL:         "https://github.com/example/project/releases/download/nginx-v" + version + "/cdn-nginx-linux-amd64.tar.gz",
		OfficialSourceURL: "https://nginx.org/download/nginx-" + version + ".tar.gz",
		SourceSHA256:      strings.Repeat("a", 64), BuildCommit: strings.Repeat("b", 40),
		SizeBytes: 1024, DownloadedAt: time.Now().UTC(),
	}
}

func TestNginxArtifactGCKeepsReferencedAndReclaimsStaleFiles(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	artifactDirectory := t.TempDir()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	fresh := now.Add(-30 * time.Minute)
	manager, err := NewNginxUpdateManager(NginxUpdateManagerConfig{
		Store: database, Directory: artifactDirectory, Repository: "example/project",
		Interval: 24 * time.Hour, GCRetention: 24 * time.Hour, Enabled: false,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	// Catalog order: current = "1", candidate = "2", retired = "3".
	retired := gcTestNginxArtifact("1.30.3", strings.Repeat("3", 64))
	if _, _, err := database.SaveNginxArtifactCandidate(retired); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.PromoteNginxArtifact(retired.SHA256); err != nil {
		t.Fatal(err)
	}
	current := gcTestNginxArtifact("1.30.4", strings.Repeat("1", 64))
	if _, _, err := database.SaveNginxArtifactCandidate(current); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.PromoteNginxArtifact(current.SHA256); err != nil {
		t.Fatal(err)
	}
	candidate := gcTestNginxArtifact("1.30.5", strings.Repeat("2", 64))
	if _, _, err := database.SaveNginxArtifactCandidate(candidate); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{
		current.SHA256 + ".tar.gz":          "current-bundle",
		candidate.SHA256 + ".tar.gz":        "candidate-bundle",
		retired.SHA256 + ".tar.gz":          "retired-bundle",
		strings.Repeat("4", 64) + ".tar.gz": "orphan-old",
		strings.Repeat("5", 64) + ".tar.gz": "orphan-fresh",
		strings.Repeat("6", 64) + ".tar.gz": "task-target",
		".nginx-download-abc.tmp":           "stale-tmp",
		".nginx-download-def.tmp":           "fresh-tmp",
		"unexpected.txt":                    "unknown",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(artifactDirectory, name), []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		current.SHA256 + ".tar.gz", retired.SHA256 + ".tar.gz",
		strings.Repeat("4", 64) + ".tar.gz",
		strings.Repeat("6", 64) + ".tar.gz",
		".nginx-download-abc.tmp",
	} {
		pathname := filepath.Join(artifactDirectory, name)
		if err := os.Chtimes(pathname, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(artifactDirectory, candidate.SHA256+".tar.gz"), recent, recent); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(artifactDirectory, ".nginx-download-def.tmp"), fresh, fresh); err != nil {
		t.Fatal(err)
	}

	// An in-flight upgrade task references the un-cataloged bundle "6"*64.
	node, err := database.CreateNode("edge-gc", "203.0.113.190")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatWithArtifacts(node.ID, 0, "", nil, "v1", strings.Repeat("9", 64), "1.30.3", strings.Repeat("a", 64), ""); err != nil {
		t.Fatal(err)
	}
	taskTarget := strings.Repeat("6", 64)
	instruction := domain.NodeUpgradeInstruction{
		Binary:         domain.UpgradeArtifact{URL: "https://control.example.test/edge", SHA256: strings.Repeat("8", 64)},
		Installer:      domain.UpgradeArtifact{URL: "https://control.example.test/install", SHA256: strings.Repeat("a", 64)},
		AgentService:   domain.UpgradeArtifact{URL: "https://control.example.test/agent-service", SHA256: strings.Repeat("b", 64)},
		UpdaterService: domain.UpgradeArtifact{URL: "https://control.example.test/updater-service", SHA256: strings.Repeat("c", 64)},
		NginxBundle:    &domain.UpgradeArtifact{URL: "https://control.example.test/nginx", SHA256: taskTarget},
	}
	if _, _, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := manager.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertGCFile := func(t *testing.T, name string, wantPresent bool) {
		t.Helper()
		pathname := filepath.Join(artifactDirectory, name)
		_, statErr := os.Lstat(pathname)
		present := statErr == nil
		if present != wantPresent {
			t.Fatalf("file %s present = %v, want %v", name, present, wantPresent)
		}
	}
	// Candidate and current are always retained.
	assertGCFile(t, current.SHA256+".tar.gz", true)
	assertGCFile(t, candidate.SHA256+".tar.gz", true)
	// The stale retired bundle is reclaimed.
	assertGCFile(t, retired.SHA256+".tar.gz", false)
	// Old orphans are reclaimed, fresh orphans stay.
	assertGCFile(t, strings.Repeat("4", 64)+".tar.gz", false)
	assertGCFile(t, strings.Repeat("5", 64)+".tar.gz", true)
	// The in-flight task target is protected.
	assertGCFile(t, taskTarget+".tar.gz", true)
	// Stale temporary files are removed; fresh ones and unknown files stay.
	assertGCFile(t, ".nginx-download-abc.tmp", false)
	assertGCFile(t, ".nginx-download-def.tmp", true)
	assertGCFile(t, "unexpected.txt", true)

	// Once the task completes, the protected file becomes reclaimable.
	task, err := database.LatestNodeUpgrade(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{
		TaskID: task.ID, Status: domain.NodeUpgradeSucceeded, InstalledSHA256: strings.Repeat("8", 64), InstalledNginxSHA256: taskTarget,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(artifactDirectory, taskTarget+".tar.gz")); err != nil {
		t.Fatal(err)
	}
	manager.gcRetention = 0 // min retention so the next GC reclaims immediately
	if err := manager.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(artifactDirectory, taskTarget+".tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("completed task target still present: %v", err)
	}
}

func TestNginxArtifactGCWaitsForBackupOperationLock(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	artifactDirectory := t.TempDir()
	operationLockRoot := t.TempDir()
	manager, err := NewNginxUpdateManager(NginxUpdateManagerConfig{
		Store: database, Directory: artifactDirectory, OperationLockRoot: operationLockRoot,
		Repository: "example/project", Interval: time.Hour, GCRetention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.gcRetention = 0
	staleArtifact := filepath.Join(artifactDirectory, strings.Repeat("7", 64)+".tar.gz")
	if err := os.WriteFile(staleArtifact, []byte("stale"), 0o640); err != nil {
		t.Fatal(err)
	}

	lockFile, err := os.OpenFile(onlineRestoreOperationLockPath(operationLockRoot), os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		t.Fatal(err)
	}
	locked := false
	t.Cleanup(func() {
		if locked {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		}
		_ = lockFile.Close()
	})
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	locked = true

	ctx, cancel := context.WithCancel(context.Background())
	gcResult := make(chan error, 1)
	go func() { gcResult <- manager.GC(ctx) }()
	select {
	case err := <-gcResult:
		cancel()
		t.Fatalf("GC completed while the backup lock was held: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	if _, err := os.Stat(staleArtifact); err != nil {
		cancel()
		t.Fatalf("GC removed an artifact while the backup lock was held: %v", err)
	}
	cancel()
	select {
	case err := <-gcResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel blocked GC: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GC did not stop after its lock wait was cancelled")
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	locked = false

	if err := manager.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale artifact remains after the backup lock was released: %v", err)
	}
}
