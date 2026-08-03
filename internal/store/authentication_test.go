package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTOTPCounterIsConsumedAtomically(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdminWithRecoveryCodesAndTOTPCounter("password-hash", "totp-secret", nil, 42); err != nil {
		t.Fatal(err)
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.LastTOTPCounter == nil || *admin.LastTOTPCounter != 42 {
		t.Fatalf("initial TOTP counter = %#v", admin.LastTOTPCounter)
	}
	if err := database.ConsumeAdminTOTPCounter(admin.ID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reused initial TOTP counter returned %v", err)
	}
	if err := database.ConsumeAdminTOTPCounter(admin.ID, 43); err != nil {
		t.Fatal(err)
	}
	if err := database.ConsumeAdminTOTPCounter(admin.ID, 43); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replayed TOTP counter returned %v", err)
	}
	if err := database.ReplaceAdminTOTPSecret(admin.ID, "encrypted-secret"); err != nil {
		t.Fatal(err)
	}
	admin, err = database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.TOTPSecret != "encrypted-secret" || admin.LastTOTPCounter == nil || *admin.LastTOTPCounter != 43 {
		t.Fatalf("administrator after encryption-only TOTP update = %#v", admin)
	}
	if err := database.ReplaceAdminTOTPSecretWithCounter(admin.ID, "replacement-secret", 100); err != nil {
		t.Fatal(err)
	}
	admin, err = database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.TOTPSecret != "replacement-secret" || admin.LastTOTPCounter == nil || *admin.LastTOTPCounter != 100 {
		t.Fatalf("administrator after TOTP replacement = %#v", admin)
	}
}

func TestAuthenticatedSessionElevationAndRevocation(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("password-hash", "totp-secret"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := database.CreateAuthenticatedSession("admin", "first-token", "first-csrf", "passkey", "credential-id", now, now.Add(5*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateAuthenticatedSession("admin", "second-token", "second-csrf", "totp", "", now, now.Add(5*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	session, err := database.Session("first-token")
	if err != nil {
		t.Fatal(err)
	}
	if session.AuthMethod != "passkey" || session.AuthenticatorID != "credential-id" || session.AuthenticatedAt == nil || !session.AuthenticatedAt.Equal(now) || session.ElevatedUntil == nil || !session.ElevatedUntil.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("authenticated session = %#v", session)
	}

	reauthenticatedAt := now.Add(time.Minute)
	if err := database.ElevateSession("first-token", "replacement-token", "recovery", "", reauthenticatedAt, reauthenticatedAt.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Session("first-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old elevated token returned %v", err)
	}
	session, err = database.Session("replacement-token")
	if err != nil {
		t.Fatal(err)
	}
	if session.AuthMethod != "recovery" || session.AuthenticatorID != "" || session.AuthenticatedAt == nil || !session.AuthenticatedAt.Equal(reauthenticatedAt) {
		t.Fatalf("elevated session = %#v", session)
	}
	if err := database.DeleteOtherSessions("admin", "replacement-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Session("second-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other session survived revocation: %v", err)
	}
	if _, err := database.Session("replacement-token"); err != nil {
		t.Fatalf("kept session was revoked: %v", err)
	}
}

func TestAuthenticationAttemptLimitPersistsAcrossStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	limit := []AuthenticationAttemptLimit{{Scope: "login-ip", Key: "203.0.113.10", Limit: 2}}
	attemptedAt := time.Now().UTC().Truncate(time.Millisecond)
	for attempt := 0; attempt < 2; attempt++ {
		allowed, err := database.ReserveAuthenticationAttempts(limit, 10*time.Minute, attemptedAt.Add(time.Duration(attempt)*time.Second))
		if err != nil || !allowed {
			t.Fatalf("authentication attempt %d = allowed:%t err:%v", attempt+1, allowed, err)
		}
	}
	allowed, err := database.ReserveAuthenticationAttempts(limit, 10*time.Minute, attemptedAt.Add(2*time.Second))
	if err != nil || allowed {
		t.Fatalf("third authentication attempt = allowed:%t err:%v", allowed, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	allowed, err = database.ReserveAuthenticationAttempts(limit, 10*time.Minute, attemptedAt.Add(time.Minute))
	if err != nil || allowed {
		t.Fatalf("authentication attempt after restart = allowed:%t err:%v", allowed, err)
	}
	count, err := database.AuthenticationAttemptCount(limit[0].Scope, limit[0].Key, attemptedAt.Add(-time.Second))
	if err != nil || count != 2 {
		t.Fatalf("persisted authentication attempt count = %d, err=%v", count, err)
	}
	if err := database.ClearAuthenticationAttempts(limit); err != nil {
		t.Fatal(err)
	}
	allowed, err = database.ReserveAuthenticationAttempts(limit, 10*time.Minute, attemptedAt.Add(2*time.Minute))
	if err != nil || !allowed {
		t.Fatalf("authentication attempt after clearing = allowed:%t err:%v", allowed, err)
	}
}
