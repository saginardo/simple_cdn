package control

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"simple_cdn/internal/auth"
	"simple_cdn/internal/store"
)

func TestAuthenticationSettingsKeepTOTPEnabledAndManagePasskeys(t *testing.T) {
	const (
		password  = "correct horse battery staple"
		oldSecret = "JBSWY3DPEHPK3PXP"
		rpID      = "control.example.test"
	)
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Cipher: cipher, ControlURL: "https://" + rpID}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	encryptedSecret, err := server.encryptTOTPSecret(oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateInitialAdmin(passwordHash, encryptedSecret); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateAuthenticatedSession("admin", "session-token", "csrf-token", "totp", "", now, now.Add(5*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateAuthenticatedSession("admin", "other-session-token", "other-csrf-token", "passkey", "old-credential", now, now.Add(5*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	response := settingsRequest(t, server, http.MethodGet, "/api/settings/authentication", nil, false)
	view := decodeAuthenticationSettings(t, response, http.StatusOK)
	if !view.TOTPEnabled || !view.RecentAuthentication || view.PasskeyEnabled || view.PasskeyOperational || !view.PasskeyAvailable || view.RPID != rpID || len(view.Passkeys) != 0 {
		t.Fatalf("initial authentication settings = %#v", view)
	}

	response = settingsRequest(t, server, http.MethodPost, "/api/settings/authentication/totp/begin", nil, true)
	if response.Code != http.StatusOK {
		t.Fatalf("begin TOTP replacement = %d %s", response.Code, response.Body.String())
	}
	var replacement struct {
		Secret string `json:"totp_secret"`
		URL    string `json:"otpauth_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if !auth.ValidTOTPSecret(replacement.Secret) || !strings.HasPrefix(replacement.URL, "otpauth://totp/") || !strings.Contains(replacement.URL, "secret="+replacement.Secret) {
		t.Fatalf("TOTP replacement material = %#v", replacement)
	}
	code := testTOTPCode(t, replacement.Secret, time.Now())
	response = settingsRequest(t, server, http.MethodPut, "/api/settings/authentication/totp", map[string]any{
		"totp_secret": replacement.Secret, "code": "not-a-code",
	}, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("TOTP replacement with invalid new code = %d %s", response.Code, response.Body.String())
	}
	response = settingsRequest(t, server, http.MethodPut, "/api/settings/authentication/totp", map[string]any{
		"totp_secret": replacement.Secret, "code": code,
	}, true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("replace TOTP = %d %s", response.Code, response.Body.String())
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	storedSecret, err := server.adminTOTPSecret(admin)
	if err != nil {
		t.Fatal(err)
	}
	if storedSecret != replacement.Secret || storedSecret == oldSecret || !strings.HasPrefix(admin.TOTPSecret, encryptedTOTPSecretPrefix) || admin.LastTOTPCounter == nil {
		t.Fatalf("stored replacement secret = %q, record = %q", storedSecret, admin.TOTPSecret)
	}
	if _, err := database.Session("other-session-token"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other session survived TOTP replacement: %v", err)
	}
	response = settingsRequest(t, server, http.MethodDelete, "/api/settings/authentication/totp", nil, true)
	if response.Code != http.StatusNotFound {
		t.Fatalf("TOTP disable endpoint unexpectedly exists: %d %s", response.Code, response.Body.String())
	}

	response = settingsRequest(t, server, http.MethodPost, "/api/settings/authentication/passkeys/begin", map[string]string{"name": "Laptop"}, true)
	if response.Code != http.StatusOK {
		t.Fatalf("begin passkey registration = %d %s", response.Code, response.Body.String())
	}
	registrationBody := response.Body.String()
	for _, required := range []string{`"residentKey":"required"`, `"userVerification":"required"`, `"id":"` + rpID + `"`} {
		if !strings.Contains(registrationBody, required) {
			t.Fatalf("passkey registration options lack %s: %s", required, registrationBody)
		}
	}
	registerCookie := response.Result().Cookies()
	if len(registerCookie) != 1 || registerCookie[0].Name != passkeyRegisterCookieName || !registerCookie[0].HttpOnly || !registerCookie[0].Secure {
		t.Fatalf("passkey registration cookies = %#v", registerCookie)
	}
	challenge, err := database.ConsumeWebAuthnChallenge(registerCookie[0].Value, passkeyRegisterChallengePurpose)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserID != admin.ID || challenge.RPID != rpID || challenge.Label != "Laptop" {
		t.Fatalf("registration challenge = %#v", challenge)
	}

	credential := &webauthn.Credential{ID: []byte("credential-bytes")}
	credentialCiphertext, err := server.encryptPasskeyCredential(credential)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := base64.RawURLEncoding.EncodeToString(credential.ID)
	if err := database.SavePasskeyCredential(admin.ID, rpID, credentialID, "Laptop", credentialCiphertext); err != nil {
		t.Fatal(err)
	}
	oldRPID := "old-control.example.test"
	oldCredential := &webauthn.Credential{ID: []byte("old-credential-bytes")}
	oldCredentialCiphertext, err := server.encryptPasskeyCredential(oldCredential)
	if err != nil {
		t.Fatal(err)
	}
	oldCredentialID := base64.RawURLEncoding.EncodeToString(oldCredential.ID)
	if _, err := database.WebAuthnUserHandle(admin.ID, oldRPID); err != nil {
		t.Fatal(err)
	}
	if err := database.SavePasskeyCredential(admin.ID, oldRPID, oldCredentialID, "Old domain key", oldCredentialCiphertext); err != nil {
		t.Fatal(err)
	}
	response = settingsRequest(t, server, http.MethodGet, "/api/settings/authentication", nil, false)
	view = decodeAuthenticationSettings(t, response, http.StatusOK)
	if !view.TOTPEnabled || !view.PasskeyEnabled || !view.PasskeyOperational || len(view.Passkeys) != 2 {
		t.Fatalf("settings after passkey registration = %#v", view)
	}
	currentCount := 0
	oldRPCount := 0
	for _, passkey := range view.Passkeys {
		if passkey.RPID == rpID && passkey.CurrentRP {
			currentCount++
		}
		if passkey.RPID == oldRPID && !passkey.CurrentRP {
			oldRPCount++
		}
	}
	if currentCount != 1 || oldRPCount != 1 {
		t.Fatalf("cross-RP passkey metadata = %#v", view.Passkeys)
	}
	assertSetupPasskeyStatus(t, server, true)

	response = settingsRequest(t, server, http.MethodPut, "/api/settings/authentication/passkeys", map[string]bool{"enabled": false}, true)
	view = decodeAuthenticationSettings(t, response, http.StatusOK)
	if !view.TOTPEnabled || view.PasskeyEnabled || view.PasskeyOperational || len(view.Passkeys) != 2 {
		t.Fatalf("settings after disabling passkeys = %#v", view)
	}
	assertSetupPasskeyStatus(t, server, false)
	publicLogin := httptest.NewRecorder()
	server.Handler().ServeHTTP(publicLogin, httptest.NewRequest(http.MethodPost, "/api/auth/passkey/begin", nil))
	if publicLogin.Code != http.StatusConflict {
		t.Fatalf("disabled passkey login = %d %s", publicLogin.Code, publicLogin.Body.String())
	}

	if err := database.CreateAuthenticatedSession(admin.ID, "passkey-other-session", "passkey-other-csrf", "passkey", credentialID, now, now.Add(5*time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	response = settingsRequest(t, server, http.MethodPut, "/api/settings/authentication/passkeys", map[string]bool{"enabled": true}, true)
	view = decodeAuthenticationSettings(t, response, http.StatusOK)
	if !view.TOTPEnabled || !view.PasskeyEnabled || !view.PasskeyOperational {
		t.Fatalf("settings after re-enabling passkeys = %#v", view)
	}
	if _, err := database.Session("passkey-other-session"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other session survived passkey setting change: %v", err)
	}
	publicLogin = httptest.NewRecorder()
	server.Handler().ServeHTTP(publicLogin, httptest.NewRequest(http.MethodPost, "/api/auth/passkey/begin", nil))
	if publicLogin.Code != http.StatusOK || !strings.Contains(publicLogin.Body.String(), `"userVerification":"required"`) {
		t.Fatalf("begin passkey login = %d %s", publicLogin.Code, publicLogin.Body.String())
	}
	loginCookies := publicLogin.Result().Cookies()
	if len(loginCookies) != 1 || loginCookies[0].Name != passkeyLoginCookieName || !loginCookies[0].HttpOnly || !loginCookies[0].Secure {
		t.Fatalf("passkey login cookies = %#v", loginCookies)
	}

	response = settingsRequest(t, server, http.MethodDelete, "/api/settings/authentication/passkeys/"+oldCredentialID+"?rp_id="+url.QueryEscape(oldRPID), nil, true)
	view = decodeAuthenticationSettings(t, response, http.StatusOK)
	if !view.PasskeyEnabled || !view.PasskeyOperational || len(view.Passkeys) != 1 {
		t.Fatalf("settings after deleting old-RP passkey = %#v", view)
	}

	response = settingsRequest(t, server, http.MethodDelete, "/api/settings/authentication/passkeys/"+credentialID+"?rp_id="+url.QueryEscape(rpID), nil, true)
	view = decodeAuthenticationSettings(t, response, http.StatusOK)
	if !view.TOTPEnabled || view.PasskeyEnabled || view.PasskeyOperational || len(view.Passkeys) != 0 {
		t.Fatalf("settings after deleting last passkey = %#v", view)
	}
	assertSetupPasskeyStatus(t, server, false)
}

func TestAuthenticationSettingsRequireAndRefreshRecentAuthentication(t *testing.T) {
	const (
		password = "correct horse battery staple"
		secret   = "JBSWY3DPEHPK3PXP"
	)
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Cipher: cipher, ControlURL: "https://control.example.test"}
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	encryptedSecret, err := server.encryptTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateInitialAdmin(passwordHash, encryptedSecret); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "legacy-session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	response := settingsRequestWithSession(t, server, http.MethodGet, "/api/settings/authentication", nil, "legacy-session-token", "")
	view := decodeAuthenticationSettings(t, response, http.StatusOK)
	if view.RecentAuthentication {
		t.Fatalf("legacy session unexpectedly has recent authentication: %#v", view)
	}
	response = settingsRequestWithSession(t, server, http.MethodPost, "/api/settings/authentication/totp/begin", nil, "legacy-session-token", "csrf-token")
	if response.Code != http.StatusPreconditionRequired || !strings.Contains(response.Body.String(), `"code":"reauthentication_required"`) {
		t.Fatalf("sensitive request without recent authentication = %d %s", response.Code, response.Body.String())
	}

	response = settingsRequestWithSession(t, server, http.MethodPost, "/api/settings/authentication/reauthenticate", map[string]string{
		"password": password,
		"totp":     testTOTPCode(t, secret, time.Now()),
	}, "legacy-session-token", "csrf-token")
	if response.Code != http.StatusOK {
		t.Fatalf("reauthenticate = %d %s", response.Code, response.Body.String())
	}
	replacementCookie := responseCookie(t, response, "cdn_session")
	if replacementCookie.Value == "legacy-session-token" {
		t.Fatal("reauthentication did not rotate the session token")
	}
	if _, err := database.Session("legacy-session-token"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old session token remained valid after reauthentication: %v", err)
	}
	session, err := database.Session(replacementCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if session.AuthMethod != "totp" || session.ElevatedUntil == nil || !session.ElevatedUntil.After(time.Now()) {
		t.Fatalf("elevated session = %#v", session)
	}
	response = settingsRequestWithSession(t, server, http.MethodPost, "/api/settings/authentication/totp/begin", nil, replacementCookie.Value, "csrf-token")
	if response.Code != http.StatusOK {
		t.Fatalf("sensitive request after reauthentication = %d %s", response.Code, response.Body.String())
	}
}

func settingsRequestWithSession(t *testing.T, server *Server, method, path string, input any, token, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: token})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func decodeAuthenticationSettings(t *testing.T, response *httptest.ResponseRecorder, status int) authenticationSettingsView {
	t.Helper()
	if response.Code != status {
		t.Fatalf("authentication settings = %d %s", response.Code, response.Body.String())
	}
	var view authenticationSettingsView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func assertSetupPasskeyStatus(t *testing.T, server *Server, enabled bool) {
	t.Helper()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/setup/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("setup status = %d %s", response.Code, response.Body.String())
	}
	var status struct {
		Initialized    bool `json:"initialized"`
		PasskeyEnabled bool `json:"passkey_enabled"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Initialized || status.PasskeyEnabled != enabled {
		t.Fatalf("setup status = %#v, want passkey_enabled=%t", status, enabled)
	}
}

func testTOTPCode(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(auth.NormalizeTOTPSecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	counter := uint64(now.Unix() / 30)
	buffer := make([]byte, 8)
	binary.BigEndian.PutUint64(buffer, counter)
	mac := hmac.New(sha1.New, decoded)
	_, _ = mac.Write(buffer)
	sum := mac.Sum(nil)
	offset := int(sum[len(sum)-1] & 0x0f)
	value := (uint32(sum[offset])&0x7f)<<24 | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", value%1_000_000)
}
