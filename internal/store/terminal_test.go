package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalAuditLifecycleAndRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	session := TerminalSession{ID: "first", NodeID: "edge", Actor: "admin", RemoteAddr: "192.0.2.10", User: "root", Shell: "/bin/bash", Status: "pending", CreatedAt: created}
	if err := database.CreateTerminalSession(session); err != nil {
		t.Fatal(err)
	}
	duplicate := session
	duplicate.ID = "second"
	if err := database.CreateTerminalSession(duplicate); err == nil {
		t.Fatal("second active session accepted")
	}
	session.Status = "running"
	session.StartedAt = &created
	if err := database.StartTerminalSession(session); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.ListTerminalSessions("edge")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Reason != "control_restarted" || rows[0].EndedAt == nil || rows[0].StartedAt == nil || rows[0].Actor != "admin" || rows[0].RemoteAddr != "192.0.2.10" {
		t.Fatalf("recovered audit = %+v", rows)
	}
	var count int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action LIKE 'terminal_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("audit events=%d", count)
	}
	if err := database.CreateTerminalSession(duplicate); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalCreationFailsWhenAuditCannotBeWritten(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`CREATE TRIGGER fail_terminal_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateTerminalSession(TerminalSession{ID: "blocked", NodeID: "edge", CreatedAt: time.Now()}); err == nil {
		t.Fatal("session created without audit")
	}
	rows, err := database.ListTerminalSessions("edge")
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial transaction: %+v, %v", rows, err)
	}
}
