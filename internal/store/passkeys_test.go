package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPasskeyCredentialLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("password-hash", "totp-secret"); err != nil {
		t.Fatal(err)
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.PasskeyEnabled {
		t.Fatal("new administrator unexpectedly has passkey login enabled")
	}

	const rpID = "control.example.test"
	handle, err := database.WebAuthnUserHandle(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	secondHandle, err := database.WebAuthnUserHandle(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	if len(handle) != webAuthnUserHandleBytes || !bytes.Equal(handle, secondHandle) {
		t.Fatalf("WebAuthn handle is not stable: first=%x second=%x", handle, secondHandle)
	}
	if err := database.SetPasskeyEnabled(admin.ID, rpID, true); err == nil {
		t.Fatal("enabled passkey login without a registered credential")
	}

	const credentialID = "credential-id"
	if err := database.SavePasskeyCredential(admin.ID, rpID, credentialID, "Laptop", []byte("encrypted-v1")); err != nil {
		t.Fatal(err)
	}
	admin, err = database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if !admin.PasskeyEnabled {
		t.Fatal("registering the first passkey did not enable passkey login")
	}
	records, err := database.PasskeyCredentialRecords(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != credentialID || records[0].Name != "Laptop" || !bytes.Equal(records[0].Ciphertext, []byte("encrypted-v1")) {
		t.Fatalf("stored passkey = %#v", records)
	}
	owner, err := database.PasskeyOwner(rpID, credentialID, handle)
	if err != nil || owner != admin.ID {
		t.Fatalf("passkey owner = %q, %v", owner, err)
	}
	if _, err := database.PasskeyOwner(rpID, credentialID, []byte("wrong-handle")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong user handle returned %v", err)
	}

	usedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := database.UpdatePasskeyCredential(admin.ID, rpID, credentialID, []byte("encrypted-v2"), usedAt); err != nil {
		t.Fatal(err)
	}
	records, err = database.PasskeyCredentialRecords(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !bytes.Equal(records[0].Ciphertext, []byte("encrypted-v2")) || records[0].LastUsedAt == nil || !records[0].LastUsedAt.Equal(usedAt) {
		t.Fatalf("updated passkey = %#v", records)
	}

	if err := database.SetPasskeyEnabled(admin.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := database.PasskeyOwner(rpID, credentialID, handle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled passkey remained usable: %v", err)
	}
	if err := database.SetPasskeyEnabled(admin.ID, rpID, true); err != nil {
		t.Fatal(err)
	}
	if err := database.DeletePasskeyCredential(admin.ID, rpID, credentialID); err != nil {
		t.Fatal(err)
	}
	admin, err = database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.PasskeyEnabled {
		t.Fatal("removing the last passkey did not disable passkey login")
	}
	if err := database.SetPasskeyEnabled(admin.ID, rpID, true); err == nil {
		t.Fatal("re-enabled passkey login after the last credential was removed")
	}
}

func TestPasskeyCredentialsRemainManageableAcrossRelyingParties(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("password-hash", "totp-secret"); err != nil {
		t.Fatal(err)
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	for _, rpID := range []string{"old-control.example.test", "new-control.example.test"} {
		if _, err := database.WebAuthnUserHandle(admin.ID, rpID); err != nil {
			t.Fatal(err)
		}
		if err := database.SavePasskeyCredential(admin.ID, rpID, rpID+"-credential", rpID, []byte("encrypted-"+rpID)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := database.PasskeyCredentialRecordsForUser(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].RPID != "new-control.example.test" || records[1].RPID != "old-control.example.test" {
		t.Fatalf("credentials across relying parties = %#v", records)
	}

	if err := database.DeletePasskeyCredential(admin.ID, "old-control.example.test", "old-control.example.test-credential"); err != nil {
		t.Fatal(err)
	}
	admin, err = database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if !admin.PasskeyEnabled {
		t.Fatal("deleting an old-RP credential disabled a remaining current-RP credential")
	}
	if err := database.SetPasskeyEnabled(admin.ID, "old-control.example.test", true); err == nil {
		t.Fatal("enabled passkey login for an RP without a credential")
	}
	if err := database.SetPasskeyEnabled(admin.ID, "new-control.example.test", true); err != nil {
		t.Fatalf("enable passkey login for remaining RP: %v", err)
	}

	if err := database.DeletePasskeyCredential(admin.ID, "new-control.example.test", "new-control.example.test-credential"); err != nil {
		t.Fatal(err)
	}
	admin, err = database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.PasskeyEnabled {
		t.Fatal("deleting the final credential did not disable passkey login")
	}
}

func TestWebAuthnChallengeIsScopedExpiringAndSingleUse(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	expiresAt := time.Now().UTC().Add(time.Minute)
	if err := database.CreateWebAuthnChallenge("one-time-token", "register", "admin", "control.example.test", "Laptop", []byte(`{"challenge":"value"}`), expiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConsumeWebAuthnChallenge("one-time-token", "login"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong challenge purpose returned %v", err)
	}
	challenge, err := database.ConsumeWebAuthnChallenge("one-time-token", "register")
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserID != "admin" || challenge.RPID != "control.example.test" || challenge.Label != "Laptop" || !bytes.Equal(challenge.SessionJSON, []byte(`{"challenge":"value"}`)) {
		t.Fatalf("consumed challenge = %#v", challenge)
	}
	if _, err := database.ConsumeWebAuthnChallenge("one-time-token", "register"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reused challenge returned %v", err)
	}

	if err := database.CreateWebAuthnChallenge("expired-token", "login", "", "control.example.test", "", []byte(`{"challenge":"expired"}`), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConsumeWebAuthnChallenge("expired-token", "login"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired challenge returned %v", err)
	}
}
