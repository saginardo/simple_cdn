package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/auth"
	"simple_cdn/internal/store"
)

func TestLoginRejectsReplayedTOTPAndRecordsAuthenticationMethod(t *testing.T) {
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
	server := &Server{Store: database, Cipher: cipher}
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

	codeTime := time.Now()
	code := testTOTPCode(t, secret, codeTime)
	matchedCounter, matched := auth.MatchTOTP(secret, code, codeTime)
	if !matched {
		t.Fatal("generated TOTP did not match")
	}
	first := loginRequestWithTOTP(t, server, password, code)
	if first.Code != http.StatusOK {
		t.Fatalf("first TOTP login = %d %s", first.Code, first.Body.String())
	}
	sessionCookie := responseCookie(t, first, "cdn_session")
	session, err := database.Session(sessionCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if session.AuthMethod != "totp" || session.AuthenticatedAt == nil || session.ElevatedUntil == nil || !session.ElevatedUntil.After(time.Now()) {
		t.Fatalf("TOTP login session = %#v", session)
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if admin.LastTOTPCounter == nil || *admin.LastTOTPCounter != matchedCounter {
		t.Fatalf("consumed TOTP counter = %#v, want %d", admin.LastTOTPCounter, matchedCounter)
	}

	replay := loginRequestWithTOTP(t, server, password, code)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed TOTP login = %d %s", replay.Code, replay.Body.String())
	}
	for _, cookie := range replay.Result().Cookies() {
		if cookie.Name == "cdn_session" && cookie.Value != "" {
			t.Fatalf("replayed TOTP issued session cookie %#v", cookie)
		}
	}
}

func loginRequestWithTOTP(t *testing.T, server *Server, password, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"password": password, "totp": code})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "203.0.113.10:12345"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}
