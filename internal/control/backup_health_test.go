package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/store"
)

func TestBackupHealthFreshnessAndStalls(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, state  string
		age, runtime time.Duration
		want         string
	}{
		{"recent", BackupRunSucceeded, time.Hour, 0, "healthy"},
		{"missed daily backup", BackupRunSucceeded, 27 * time.Hour, 0, "stale"},
		{"fresh run cannot hide stale backup", BackupRunRunning, 27 * time.Hour, time.Minute, "stale"},
		{"hung backup", BackupRunRunning, time.Hour, 7 * time.Hour, "stalled"},
		{"hung retry", BackupRunRetrying, time.Hour, 7 * time.Hour, "stalled"},
		{"running", BackupRunRunning, time.Hour, time.Minute, "running"},
		{"failed with recovery point", BackupRunFailed, time.Hour, 0, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			last := now.Add(-test.age)
			run := BackupRunStatus{State: test.state, LastSucceededAt: &last, FinishedAt: &last,
				StartedAt: now.Add(-test.runtime), UpdatedAt: now.Add(-time.Minute)}
			got := evaluateBackupHealth(&run, nil, true, now, 26*time.Hour, 6*time.Hour)
			if got.State != test.want || got.LastSucceededAt == nil || !got.LastSucceededAt.Equal(last) {
				t.Fatalf("health: %#v", got)
			}
		})
	}
	if got := evaluateBackupHealth(nil, os.ErrNotExist, true, now, time.Hour, time.Hour); got.State != "unknown" {
		t.Fatalf("missing status: %#v", got)
	}
	if got := evaluateBackupHealth(nil, nil, false, now, time.Hour, time.Hour); got.State != "disabled" {
		t.Fatalf("disabled: %#v", got)
	}
}

func TestBackupRunStatusKeepsLastSuccessThroughRetryAndFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	last := time.Now().UTC().Add(-time.Hour)
	success, _ := NewBackupRunStatus(BackupRunSucceeded, 1, 3, "host", last.Add(-time.Minute), last, "")
	if err := WriteBackupRunStatus(path, success); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{BackupRunRunning, BackupRunRetrying, BackupRunFailed} {
		now := time.Now().UTC()
		detail := ""
		if state != BackupRunRunning {
			detail = "attempt failed"
		}
		run, err := NewBackupRunStatus(state, 1, 3, "host", now, now, detail)
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteBackupRunStatus(path, run); err != nil {
			t.Fatal(err)
		}
		loaded, err := ReadBackupRunStatus(path)
		if err != nil || loaded.LastSucceededAt == nil || !loaded.LastSucceededAt.Equal(last) {
			t.Fatalf("%s lost recovery point: %#v %v", state, loaded, err)
		}
	}
}

func TestBackupVerificationHistorySurvivesFailures(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	point := now.Add(-time.Hour)
	verified := OnlineRestoreJob{ID: "verified", VerifyOnly: true, State: OnlineRestoreVerified,
		SnapshotID: "snapshot", SnapshotTime: point, CreatedAt: now.Add(-time.Minute), FinishedAt: &now}
	if err := recordBackupVerification(root, verified); err != nil {
		t.Fatal(err)
	}
	failed := OnlineRestoreJob{ID: "failed", VerifyOnly: true, State: OnlineRestoreFailed, Error: "corrupt object", CreatedAt: now, FinishedAt: &now}
	if err := recordBackupVerification(root, failed); err != nil {
		t.Fatal(err)
	}
	status, err := readBackupVerification(root)
	if err != nil || status.State != OnlineRestoreFailed || status.LastVerifiedSnapshotID != "snapshot" || status.LastVerifiedSnapshotTime == nil || !status.LastVerifiedSnapshotTime.Equal(point) {
		t.Fatalf("history: %#v %v", status, err)
	}
}

func TestBackupHealthAlertsAreDeduplicated(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := &BackupHealthManager{Server: &Server{Store: database}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := manager.notify(context.Background(), "same-backup", "stale", "backup is stale"); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := database.Messages(50, false)
	if err != nil || len(messages.Messages) != 1 {
		t.Fatalf("alerts: %#v %v", messages, err)
	}
}
