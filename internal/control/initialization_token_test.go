package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/auth"
	"simple_cdn/internal/store"
)

func TestInitializationTokenLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initialization-token")
	created, err := EnsureInitializationToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first initialization token was not created")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != initializationTokenFileMode {
		t.Fatalf("token file mode = %o, want %o", got, initializationTokenFileMode)
	}
	token, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyInitializationToken(path, strings.TrimSpace(string(token))); err != nil {
		t.Fatalf("verify generated token: %v", err)
	}
	if err := ConsumeInitializationToken(path, "wrong-token"); err == nil {
		t.Fatal("wrong token was consumed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("wrong token removed the file: %v", err)
	}
	if err := ConsumeInitializationToken(path, strings.TrimSpace(string(token))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file remains after consumption: %v", err)
	}
}

func TestSetupRequiresOneTimeTokenAndEncryptsTOTP(t *testing.T) {
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "control.db"))
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
	tokenPath := filepath.Join(directory, "initialization-token")
	if _, err := EnsureInitializationToken(tokenPath); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database, Cipher: cipher, InitializationTokenPath: tokenPath}
	initializationToken := strings.TrimSpace(string(token))

	for _, candidate := range []string{"", "wrong-token"} {
		response := setupJSONRequest(t, server, "/api/setup/begin", map[string]any{
			"initialization_token": candidate,
			"password":             "correct horse battery staple",
		})
		if response.Code != http.StatusForbidden {
			t.Fatalf("setup with token %q status = %d, want %d: %s", candidate, response.Code, http.StatusForbidden, response.Body.String())
		}
	}

	response := setupJSONRequest(t, server, "/api/setup/begin", map[string]any{
		"initialization_token": initializationToken,
		"password":             "correct horse battery staple",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("begin setup status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var material struct {
		TOTPSecret    string   `json:"totp_secret"`
		TOTPURL       string   `json:"otpauth_url"`
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &material); err != nil {
		t.Fatal(err)
	}
	if !auth.ValidTOTPSecret(material.TOTPSecret) || !strings.HasPrefix(material.TOTPURL, "otpauth://totp/") || len(material.RecoveryCodes) != 10 {
		t.Fatalf("setup material = %#v", material)
	}
	if initialized, err := database.HasAdmin(); err != nil || initialized {
		t.Fatalf("administrator exists before TOTP confirmation: initialized=%t err=%v", initialized, err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("begin setup consumed initialization token: %v", err)
	}

	finishInput := map[string]any{
		"initialization_token": initializationToken,
		"password":             "correct horse battery staple",
		"totp_secret":          material.TOTPSecret,
		"totp_code":            "not-a-code",
		"recovery_codes":       material.RecoveryCodes,
	}
	response = setupJSONRequest(t, server, "/api/setup/finish", finishInput)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("finish setup with invalid TOTP = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if initialized, err := database.HasAdmin(); err != nil || initialized {
		t.Fatalf("administrator exists after failed TOTP confirmation: initialized=%t err=%v", initialized, err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("failed setup consumed initialization token: %v", err)
	}

	finishInput["totp_code"] = testTOTPCode(t, material.TOTPSecret, time.Now())
	response = setupJSONRequest(t, server, "/api/setup/finish", finishInput)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("setup cache control = %q", got)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initialization token remains after setup: %v", err)
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.TOTPSecret == material.TOTPSecret || !strings.HasPrefix(admin.TOTPSecret, encryptedTOTPSecretPrefix) {
		t.Fatalf("TOTP secret was not encrypted: %q", admin.TOTPSecret)
	}
	if admin.LastTOTPCounter == nil {
		t.Fatal("setup did not record the TOTP confirmation counter")
	}
	secret, legacy, err := server.decryptTOTPSecret(admin.TOTPSecret)
	if err != nil || legacy || secret != material.TOTPSecret {
		t.Fatalf("decrypt TOTP secret = %q, legacy=%t, err=%v", secret, legacy, err)
	}

	loginResponse := setupJSONRequest(t, server, "/api/login", map[string]any{
		"password":      "correct horse battery staple",
		"recovery_code": material.RecoveryCodes[0],
	})
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login with issued recovery code = %d %s", loginResponse.Code, loginResponse.Body.String())
	}
}

func setupJSONRequest(t *testing.T, server *Server, path string, input any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestMigrateAdminTOTPSecret(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("password-hash", "JBSWY3DPEHPK3PXP"); err != nil {
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
	server := &Server{Store: database, Cipher: cipher}
	if err := server.MigrateAdminTOTPSecret(); err != nil {
		t.Fatal(err)
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(admin.TOTPSecret, encryptedTOTPSecretPrefix) {
		t.Fatalf("migrated secret = %q", admin.TOTPSecret)
	}
	secret, legacy, err := server.decryptTOTPSecret(admin.TOTPSecret)
	if err != nil || legacy || secret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("decrypt migrated secret = %q, legacy=%t, err=%v", secret, legacy, err)
	}
}

func TestPrivateAPIResponsesAreNotStored(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	(&Server{}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("session status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("session cache control = %q", got)
	}
}
