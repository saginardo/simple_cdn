package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"simple_cdn/internal/auth"
	"simple_cdn/internal/store"
)

const (
	passkeyLoginChallengePurpose    = "passkey-login"
	passkeyRegisterChallengePurpose = "passkey-register"
	passkeyChallengeLifetime        = 5 * time.Minute
	passkeyLoginCookieName          = "cdn_passkey_login"
	passkeyRegisterCookieName       = "cdn_passkey_register"
)

type passkeyRelyingParty struct {
	WebAuthn *webauthn.WebAuthn
	ID       string
	Origin   string
	Secure   bool
}

type passkeyUser struct {
	id          []byte
	userID      string
	credentials []webauthn.Credential
}

func (u *passkeyUser) WebAuthnID() []byte                         { return u.id }
func (u *passkeyUser) WebAuthnName() string                       { return u.userID }
func (u *passkeyUser) WebAuthnDisplayName() string                { return "Administrator" }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

type passkeyView struct {
	ID         string     `json:"id"`
	RPID       string     `json:"rp_id"`
	Name       string     `json:"name"`
	CurrentRP  bool       `json:"current_rp"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type authenticationSettingsView struct {
	TOTPEnabled          bool          `json:"totp_enabled"`
	RecentAuthentication bool          `json:"recent_authentication"`
	PasskeyAvailable     bool          `json:"passkey_available"`
	PasskeyEnabled       bool          `json:"passkey_enabled"`
	PasskeyOperational   bool          `json:"passkey_operational"`
	PasskeyError         string        `json:"passkey_error,omitempty"`
	RPID                 string        `json:"rp_id,omitempty"`
	Passkeys             []passkeyView `json:"passkeys"`
}

func (s *Server) beginPasskeyLogin(response http.ResponseWriter, request *http.Request) {
	_, allowed, rateLimitErr := s.reservePasskeyAttempt(request)
	if rateLimitErr != nil {
		writeError(response, http.StatusInternalServerError, errors.New("authentication rate limit is unavailable"))
		return
	}
	if !allowed {
		writeError(response, http.StatusTooManyRequests, errors.New("too many login attempts"))
		return
	}
	admin, err := s.Store.Admin()
	if err != nil || !admin.PasskeyEnabled {
		writeError(response, http.StatusConflict, errors.New("passkey login is not enabled"))
		return
	}
	rp, err := s.passkeyRP(request)
	if err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	records, err := s.Store.PasskeyCredentialRecords(admin.ID, rp.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if len(records) == 0 {
		writeError(response, http.StatusConflict, errors.New("passkey login is not enabled"))
		return
	}
	assertion, session, err := rp.WebAuthn.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired),
		webauthn.WithAssertionPublicKeyCredentialHints([]protocol.PublicKeyCredentialHints{
			protocol.PublicKeyCredentialHintClientDevice,
			protocol.PublicKeyCredentialHintHybrid,
			protocol.PublicKeyCredentialHintSecurityKey,
		}),
	)
	if err != nil {
		writeError(response, http.StatusInternalServerError, fmt.Errorf("begin passkey login: %w", err))
		return
	}
	if err := s.saveWebAuthnChallenge(response, request, passkeyLoginCookieName, passkeyLoginChallengePurpose, "", rp, "", session); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, assertion.Response)
}

func (s *Server) finishPasskeyLogin(response http.ResponseWriter, request *http.Request) {
	challenge, err := s.consumeWebAuthnChallenge(response, request, passkeyLoginCookieName, passkeyLoginChallengePurpose)
	if err != nil {
		writeError(response, http.StatusUnauthorized, errors.New("passkey challenge is missing or expired"))
		return
	}
	rp, err := s.passkeyRP(request)
	if err != nil || challenge.RPID != rp.ID {
		writeError(response, http.StatusUnauthorized, errors.New("invalid passkey challenge"))
		return
	}
	admin, err := s.Store.Admin()
	if err != nil || !admin.PasskeyEnabled {
		writeError(response, http.StatusUnauthorized, errors.New("passkey login is not enabled"))
		return
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(challenge.SessionJSON, &session); err != nil {
		writeError(response, http.StatusInternalServerError, errors.New("decode passkey challenge"))
		return
	}
	validatedUser, credential, err := rp.WebAuthn.FinishPasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		credentialID := base64.RawURLEncoding.EncodeToString(rawID)
		userID, err := s.Store.PasskeyOwner(rp.ID, credentialID, userHandle)
		if err != nil {
			return nil, err
		}
		return s.loadPasskeyUser(userID, rp.ID)
	}, session, request)
	if err != nil {
		writeError(response, http.StatusUnauthorized, errors.New("passkey authentication failed"))
		return
	}
	user, ok := validatedUser.(*passkeyUser)
	if !ok || user.userID != admin.ID {
		writeError(response, http.StatusUnauthorized, errors.New("passkey authentication failed"))
		return
	}
	ciphertext, err := s.encryptPasskeyCredential(credential)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	credentialID := base64.RawURLEncoding.EncodeToString(credential.ID)
	if err := s.Store.UpdatePasskeyCredential(user.userID, rp.ID, credentialID, ciphertext, time.Now().UTC()); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	csrf, err := s.createAdminSession(response, request, user.userID, "passkey", credentialID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	detail := "method=passkey"
	if credential.Authenticator.CloneWarning {
		detail += "; authenticator_clone_warning=true"
	}
	_ = s.Store.ClearAuthenticationAttempts(s.passkeyAttemptLimits(request))
	s.audit(request, user.userID, "login", "session", "", detail)
	writeJSON(response, http.StatusOK, map[string]string{"csrf_token": csrf})
}

func (s *Server) getAuthenticationSettings(response http.ResponseWriter, request *http.Request) {
	view, err := s.authenticationSettings(request)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) beginPasskeyRegistration(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len([]rune(name)) > 64 {
		writeError(response, http.StatusBadRequest, errors.New("passkey name must be between 1 and 64 characters"))
		return
	}
	rp, err := s.passkeyRP(request)
	if err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	userID := adminID(request.Context())
	user, err := s.loadPasskeyUser(userID, rp.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	creation, session, err := rp.WebAuthn.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(user.credentials).CredentialDescriptors()),
		webauthn.WithExtensions(protocol.AuthenticationExtensions{"credProps": true}),
		webauthn.WithPublicKeyCredentialHints([]protocol.PublicKeyCredentialHints{
			protocol.PublicKeyCredentialHintClientDevice,
			protocol.PublicKeyCredentialHintHybrid,
			protocol.PublicKeyCredentialHintSecurityKey,
		}),
	)
	if err != nil {
		writeError(response, http.StatusInternalServerError, fmt.Errorf("begin passkey registration: %w", err))
		return
	}
	if err := s.saveWebAuthnChallenge(response, request, passkeyRegisterCookieName, passkeyRegisterChallengePurpose, userID, rp, name, session); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, creation.Response)
}

func (s *Server) finishPasskeyRegistration(response http.ResponseWriter, request *http.Request) {
	challenge, err := s.consumeWebAuthnChallenge(response, request, passkeyRegisterCookieName, passkeyRegisterChallengePurpose)
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("passkey challenge is missing or expired"))
		return
	}
	userID := adminID(request.Context())
	if challenge.UserID != userID {
		writeError(response, http.StatusForbidden, errors.New("passkey challenge belongs to another session"))
		return
	}
	rp, err := s.passkeyRP(request)
	if err != nil || challenge.RPID != rp.ID {
		writeError(response, http.StatusBadRequest, errors.New("invalid passkey challenge"))
		return
	}
	user, err := s.loadPasskeyUser(userID, rp.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(challenge.SessionJSON, &session); err != nil {
		writeError(response, http.StatusInternalServerError, errors.New("decode passkey challenge"))
		return
	}
	credential, err := rp.WebAuthn.FinishRegistration(user, session, request)
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("passkey registration failed"))
		return
	}
	ciphertext, err := s.encryptPasskeyCredential(credential)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	credentialID := base64.RawURLEncoding.EncodeToString(credential.ID)
	if err := s.Store.SavePasskeyCredential(userID, rp.ID, credentialID, challenge.Label, ciphertext); err != nil {
		writeError(response, http.StatusConflict, fmt.Errorf("save passkey: %w", err))
		return
	}
	if err := s.revokeOtherAdminSessions(request); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.audit(request, userID, "register_passkey", "settings", "authentication", fmt.Sprintf("name=%q; rp_id=%s", challenge.Label, rp.ID))
	view, err := s.authenticationSettings(request)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusCreated, view)
}

func (s *Server) updatePasskeyEnabled(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	rpID := ""
	if input.Enabled {
		rp, err := s.passkeyRP(request)
		if err != nil {
			writeError(response, http.StatusConflict, err)
			return
		}
		rpID = rp.ID
	}
	userID := adminID(request.Context())
	if err := s.Store.SetPasskeyEnabled(userID, rpID, input.Enabled); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	if err := s.revokeOtherAdminSessions(request); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.audit(request, userID, "update_passkey_login", "settings", "authentication", fmt.Sprintf("enabled=%t", input.Enabled))
	view, err := s.authenticationSettings(request)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) deletePasskey(response http.ResponseWriter, request *http.Request) {
	rpID := strings.TrimSpace(request.URL.Query().Get("rp_id"))
	credentialID := strings.TrimSpace(request.PathValue("id"))
	if credentialID == "" || rpID == "" {
		writeError(response, http.StatusBadRequest, errors.New("passkey id and relying party are required"))
		return
	}
	userID := adminID(request.Context())
	if err := s.Store.DeletePasskeyCredential(userID, rpID, credentialID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, err)
			return
		}
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := s.revokeOtherAdminSessions(request); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.audit(request, userID, "delete_passkey", "settings", "authentication", "credential_id="+credentialID+"; rp_id="+rpID)
	view, err := s.authenticationSettings(request)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, view)
}

func (s *Server) authenticationSettings(request *http.Request) (authenticationSettingsView, error) {
	view := authenticationSettingsView{
		TOTPEnabled:          true,
		RecentAuthentication: sessionRecentlyAuthenticated(currentAdminSession(request.Context()), time.Now()),
		Passkeys:             make([]passkeyView, 0),
	}
	admin, err := s.Store.Admin()
	if err != nil {
		return view, err
	}
	view.PasskeyEnabled = admin.PasskeyEnabled
	rp, rpErr := s.passkeyRP(request)
	if rpErr != nil {
		view.PasskeyError = rpErr.Error()
	} else {
		view.PasskeyAvailable = true
		view.RPID = rp.ID
	}
	records, err := s.Store.PasskeyCredentialRecordsForUser(admin.ID)
	if err != nil {
		return view, err
	}
	hasCurrentCredential := false
	for _, record := range records {
		currentRP := view.RPID != "" && record.RPID == view.RPID
		hasCurrentCredential = hasCurrentCredential || currentRP
		view.Passkeys = append(view.Passkeys, passkeyView{
			ID: record.ID, RPID: record.RPID, Name: record.Name, CurrentRP: currentRP,
			CreatedAt: record.CreatedAt, LastUsedAt: record.LastUsedAt,
		})
	}
	view.PasskeyOperational = admin.PasskeyEnabled && view.PasskeyAvailable && hasCurrentCredential
	return view, nil
}

func (s *Server) passkeyLoginEnabled(request *http.Request, admin store.Admin) bool {
	if !admin.PasskeyEnabled {
		return false
	}
	rp, err := s.passkeyRP(request)
	if err != nil {
		return false
	}
	records, err := s.Store.PasskeyCredentialRecords(admin.ID, rp.ID)
	return err == nil && len(records) > 0
}

func (s *Server) passkeyRP(request *http.Request) (passkeyRelyingParty, error) {
	rawURL := strings.TrimSpace(s.ControlURL)
	parsed, err := url.Parse(rawURL)
	if rawURL == "" || err != nil || parsed.Hostname() == "" || strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".invalid") {
		scheme := "http"
		if request.TLS != nil {
			scheme = "https"
		}
		parsed = &url.URL{Scheme: scheme, Host: request.Host}
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLocalWebAuthnHost(parsed.Hostname())) {
		return passkeyRelyingParty{}, errors.New("CONTROL_PUBLIC_URL must use HTTPS for passkey authentication")
	}
	if parsed.User != nil || parsed.Hostname() == "" {
		return passkeyRelyingParty{}, errors.New("CONTROL_PUBLIC_URL is invalid for passkey authentication")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	displayName := "simple_cdn"
	if s.Settings != nil {
		if name := strings.TrimSpace(s.Settings.Branding().Name); name != "" {
			displayName = name
		}
	}
	w, err := webauthn.New(&webauthn.Config{
		RPID:                  parsed.Hostname(),
		RPDisplayName:         displayName,
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyChallengeLifetime, TimeoutUVD: passkeyChallengeLifetime},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyChallengeLifetime, TimeoutUVD: passkeyChallengeLifetime},
		},
	})
	if err != nil {
		return passkeyRelyingParty{}, fmt.Errorf("configure passkey authentication: %w", err)
	}
	return passkeyRelyingParty{WebAuthn: w, ID: parsed.Hostname(), Origin: origin, Secure: parsed.Scheme == "https"}, nil
}

func isLocalWebAuthnHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) loadPasskeyUser(userID, rpID string) (*passkeyUser, error) {
	handle, err := s.Store.WebAuthnUserHandle(userID, rpID)
	if err != nil {
		return nil, err
	}
	records, err := s.Store.PasskeyCredentialRecords(userID, rpID)
	if err != nil {
		return nil, err
	}
	credentials := make([]webauthn.Credential, 0, len(records))
	for _, record := range records {
		credential, err := s.decryptPasskeyCredential(record.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt passkey %s: %w", record.ID, err)
		}
		credentials = append(credentials, *credential)
	}
	return &passkeyUser{id: handle, userID: userID, credentials: credentials}, nil
}

func (s *Server) encryptPasskeyCredential(credential *webauthn.Credential) ([]byte, error) {
	if s.Cipher == nil {
		return nil, errors.New("CONTROL_ENCRYPTION_KEY is not configured")
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return nil, fmt.Errorf("encode passkey credential: %w", err)
	}
	ciphertext, err := s.Cipher.Encrypt(encoded)
	if err != nil {
		return nil, fmt.Errorf("encrypt passkey credential: %w", err)
	}
	return ciphertext, nil
}

func (s *Server) decryptPasskeyCredential(ciphertext []byte) (*webauthn.Credential, error) {
	if s.Cipher == nil {
		return nil, errors.New("CONTROL_ENCRYPTION_KEY is not configured")
	}
	encoded, err := s.Cipher.Decrypt(ciphertext)
	if err != nil {
		return nil, err
	}
	var credential webauthn.Credential
	if err := json.Unmarshal(encoded, &credential); err != nil {
		return nil, fmt.Errorf("decode passkey credential: %w", err)
	}
	return &credential, nil
}

func (s *Server) saveWebAuthnChallenge(response http.ResponseWriter, request *http.Request, cookieName, purpose, userID string, rp passkeyRelyingParty, label string, session *webauthn.SessionData) error {
	encoded, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode WebAuthn challenge: %w", err)
	}
	token, err := auth.NewOpaqueToken(32)
	if err != nil {
		return err
	}
	expiresAt := time.Now().UTC().Add(passkeyChallengeLifetime)
	if err := s.Store.CreateWebAuthnChallenge(token, purpose, userID, rp.ID, label, encoded, expiresAt); err != nil {
		return err
	}
	http.SetCookie(response, &http.Cookie{
		Name: cookieName, Value: token, Path: "/api/", HttpOnly: true, Secure: rp.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(passkeyChallengeLifetime.Seconds()),
	})
	return nil
}

func (s *Server) consumeWebAuthnChallenge(response http.ResponseWriter, request *http.Request, cookieName, purpose string) (store.WebAuthnChallenge, error) {
	cookie, err := request.Cookie(cookieName)
	http.SetCookie(response, &http.Cookie{Name: cookieName, Value: "", Path: "/api/", HttpOnly: true, Secure: s.secureCookie(request), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	if err != nil {
		return store.WebAuthnChallenge{}, store.ErrNotFound
	}
	return s.Store.ConsumeWebAuthnChallenge(cookie.Value, purpose)
}
