package control

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"simple_cdn/internal/store"
)

func TestPasskeyRegistrationAndLoginCeremony(t *testing.T) {
	const (
		rpID   = "control.example.test"
		origin = "https://control.example.test"
	)
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("password-hash", "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.CreateAuthenticatedSession("admin", "session-token", "csrf-token", "totp", "", now, now.Add(5*time.Minute), now.Add(time.Hour)); err != nil {
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
	server := &Server{Store: database, Cipher: cipher, ControlURL: origin}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := make([]byte, 32)
	if _, err := rand.Read(credentialID); err != nil {
		t.Fatal(err)
	}

	beginRegistration := settingsRequest(t, server, http.MethodPost, "/api/settings/authentication/passkeys/begin", map[string]string{"name": "Integration key"}, true)
	if beginRegistration.Code != http.StatusOK {
		t.Fatalf("begin passkey registration = %d %s", beginRegistration.Code, beginRegistration.Body.String())
	}
	var registrationOptions struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(beginRegistration.Body.Bytes(), &registrationOptions); err != nil {
		t.Fatal(err)
	}
	if registrationOptions.Challenge == "" {
		t.Fatal("registration options did not include a challenge")
	}
	registrationCookie := responseCookie(t, beginRegistration, passkeyRegisterCookieName)
	registrationBody := registrationCredentialResponse(t, rpID, origin, registrationOptions.Challenge, credentialID, privateKey)
	finishRegistration := passkeyCeremonyRequest(t, server, "/api/settings/authentication/passkeys/finish", registrationBody, true, registrationCookie)
	if finishRegistration.Code != http.StatusCreated {
		t.Fatalf("finish passkey registration = %d %s", finishRegistration.Code, finishRegistration.Body.String())
	}
	admin, err := database.Admin()
	if err != nil {
		t.Fatal(err)
	}
	if !admin.PasskeyEnabled {
		t.Fatal("successful registration did not enable passkey login")
	}
	records, err := database.PasskeyCredentialRecords(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Name != "Integration key" || bytes.Contains(records[0].Ciphertext, credentialID) {
		t.Fatalf("stored passkey record = %#v", records)
	}
	registeredCredential, err := server.decryptPasskeyCredential(records[0].Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(registeredCredential.ID, credentialID) || registeredCredential.Authenticator.SignCount != 0 {
		t.Fatalf("registered credential = %#v", registeredCredential)
	}

	beginLogin := httptest.NewRecorder()
	server.Handler().ServeHTTP(beginLogin, httptest.NewRequest(http.MethodPost, "/api/auth/passkey/begin", bytes.NewReader([]byte(`{}`))))
	if beginLogin.Code != http.StatusOK {
		t.Fatalf("begin passkey login = %d %s", beginLogin.Code, beginLogin.Body.String())
	}
	var loginOptions struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(beginLogin.Body.Bytes(), &loginOptions); err != nil {
		t.Fatal(err)
	}
	if loginOptions.Challenge == "" {
		t.Fatal("login options did not include a challenge")
	}
	loginCookie := responseCookie(t, beginLogin, passkeyLoginCookieName)
	userHandle, err := database.WebAuthnUserHandle(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	loginBody := assertionCredentialResponse(t, rpID, origin, loginOptions.Challenge, credentialID, userHandle, privateKey, 1)
	finishLogin := passkeyCeremonyRequest(t, server, "/api/auth/passkey/finish", loginBody, false, loginCookie)
	if finishLogin.Code != http.StatusOK {
		t.Fatalf("finish passkey login = %d %s", finishLogin.Code, finishLogin.Body.String())
	}
	var loginResult struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(finishLogin.Body.Bytes(), &loginResult); err != nil {
		t.Fatal(err)
	}
	if loginResult.CSRFToken == "" {
		t.Fatal("passkey login did not return a CSRF token")
	}
	sessionCookie := responseCookie(t, finishLogin, "cdn_session")
	session, err := database.Session(sessionCookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if session.UserID != admin.ID || session.CSRFToken != loginResult.CSRFToken || session.AuthMethod != "passkey" || session.AuthenticatorID != base64.RawURLEncoding.EncodeToString(credentialID) || session.ElevatedUntil == nil || !session.ElevatedUntil.After(time.Now()) {
		t.Fatalf("passkey session = %#v", session)
	}
	records, err = database.PasskeyCredentialRecords(admin.ID, rpID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].LastUsedAt == nil {
		t.Fatalf("passkey use metadata = %#v", records)
	}
	updatedCredential, err := server.decryptPasskeyCredential(records[0].Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if updatedCredential.Authenticator.SignCount != 1 || updatedCredential.Authenticator.CloneWarning {
		t.Fatalf("updated passkey credential = %#v", updatedCredential)
	}

	replay := passkeyCeremonyRequest(t, server, "/api/auth/passkey/finish", loginBody, false, loginCookie)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed passkey challenge = %d %s", replay.Code, replay.Body.String())
	}
}

func registrationCredentialResponse(t *testing.T, rpID, origin, challenge string, credentialID []byte, privateKey *ecdsa.PrivateKey) []byte {
	t.Helper()
	publicKeyCBOR, err := webauthncbor.Marshal(map[int]any{
		1:  int64(webauthncose.EllipticKey),
		3:  int64(webauthncose.AlgES256),
		-1: int64(webauthncose.P256),
		-2: privateKey.PublicKey.X.FillBytes(make([]byte, 32)),
		-3: privateKey.PublicKey.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	rpIDHash := sha256.Sum256([]byte(rpID))
	authenticatorData := bytes.NewBuffer(make([]byte, 0, 37+18+len(credentialID)+len(publicKeyCBOR)))
	authenticatorData.Write(rpIDHash[:])
	authenticatorData.WriteByte(byte(protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagAttestedCredentialData))
	_ = binary.Write(authenticatorData, binary.BigEndian, uint32(0))
	authenticatorData.Write(make([]byte, 16))
	_ = binary.Write(authenticatorData, binary.BigEndian, uint16(len(credentialID)))
	authenticatorData.Write(credentialID)
	authenticatorData.Write(publicKeyCBOR)
	attestationObject, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authenticatorData.Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clientData := clientDataJSON(t, "webauthn.create", challenge, origin)
	return marshalCredentialResponse(t, map[string]any{
		"id":    base64.RawURLEncoding.EncodeToString(credentialID),
		"rawId": base64.RawURLEncoding.EncodeToString(credentialID),
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attestationObject),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults":  map[string]any{"credProps": map[string]bool{"rk": true}},
		"authenticatorAttachment": "platform",
	})
}

func assertionCredentialResponse(t *testing.T, rpID, origin, challenge string, credentialID, userHandle []byte, privateKey *ecdsa.PrivateKey, counter uint32) []byte {
	t.Helper()
	rpIDHash := sha256.Sum256([]byte(rpID))
	authenticatorData := bytes.NewBuffer(make([]byte, 0, 37))
	authenticatorData.Write(rpIDHash[:])
	authenticatorData.WriteByte(byte(protocol.FlagUserPresent | protocol.FlagUserVerified))
	_ = binary.Write(authenticatorData, binary.BigEndian, counter)
	clientData := clientDataJSON(t, "webauthn.get", challenge, origin)
	clientDataHash := sha256.Sum256(clientData)
	signedData := make([]byte, 0, authenticatorData.Len()+len(clientDataHash))
	signedData = append(signedData, authenticatorData.Bytes()...)
	signedData = append(signedData, clientDataHash[:]...)
	signedDataHash := sha256.Sum256(signedData)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, signedDataHash[:])
	if err != nil {
		t.Fatal(err)
	}
	return marshalCredentialResponse(t, map[string]any{
		"id":    base64.RawURLEncoding.EncodeToString(credentialID),
		"rawId": base64.RawURLEncoding.EncodeToString(credentialID),
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authenticatorData.Bytes()),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
			"userHandle":        base64.RawURLEncoding.EncodeToString(userHandle),
		},
		"clientExtensionResults":  map[string]any{},
		"authenticatorAttachment": "platform",
	})
}

func clientDataJSON(t *testing.T, ceremonyType, challenge, origin string) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"type":        ceremonyType,
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func marshalCredentialResponse(t *testing.T, response map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func passkeyCeremonyRequest(t *testing.T, server *Server, path string, body []byte, authenticated bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authenticated {
		request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "session-token"})
		request.Header.Set("X-CSRF-Token", "csrf-token")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func responseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q: %#v", name, response.Result().Cookies())
	return nil
}
