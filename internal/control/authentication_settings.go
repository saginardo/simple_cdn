package control

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"simple_cdn/internal/auth"
)

func (s *Server) beginTOTPReplacement(response http.ResponseWriter, _ *http.Request) {
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	issuer := "simple_cdn"
	if s.Settings != nil {
		if name := strings.TrimSpace(s.Settings.Branding().Name); name != "" {
			issuer = name
		}
	}
	writeJSON(response, http.StatusOK, map[string]string{
		"totp_secret": secret,
		"otpauth_url": totpProvisioningURL(secret, issuer, "admin"),
	})
}

func (s *Server) replaceTOTP(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Secret string `json:"totp_secret"`
		Code   string `json:"code"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	admin, err := s.Store.Admin()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	secret := auth.NormalizeTOTPSecret(input.Secret)
	if !auth.ValidTOTPSecret(secret) {
		writeError(response, http.StatusBadRequest, errors.New("invalid TOTP secret"))
		return
	}
	counter, valid := auth.MatchTOTP(secret, strings.TrimSpace(input.Code), time.Now())
	if !valid {
		writeError(response, http.StatusBadRequest, errors.New("new TOTP code is invalid"))
		return
	}
	encrypted, err := s.encryptTOTPSecret(secret)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.ReplaceAdminTOTPSecretWithCounter(admin.ID, encrypted, counter); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := s.revokeOtherAdminSessions(request); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.audit(request, admin.ID, "replace_totp", "settings", "authentication", "TOTP secret replaced after recent authentication and new-code verification")
	writeJSON(response, http.StatusOK, map[string]bool{"totp_enabled": true})
}

func totpProvisioningURL(secret, issuer, account string) string {
	query := url.Values{}
	query.Set("secret", secret)
	query.Set("issuer", issuer)
	return (&url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + account,
		RawQuery: query.Encode(),
	}).String()
}
