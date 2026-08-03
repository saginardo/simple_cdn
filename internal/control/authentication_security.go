package control

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"simple_cdn/internal/auth"
	"simple_cdn/internal/store"
)

const (
	adminSessionLifetime         = 12 * time.Hour
	recentAuthenticationLifetime = 5 * time.Minute
	authenticationAttemptWindow  = 10 * time.Minute
)

func sessionRecentlyAuthenticated(session store.Session, current time.Time) bool {
	return session.ElevatedUntil != nil && session.ElevatedUntil.After(current)
}

func (s *Server) reserveAuthenticationAttempt(request *http.Request, scope, userID string, ipLimit, userLimit int) ([]store.AuthenticationAttemptLimit, bool, error) {
	limits := []store.AuthenticationAttemptLimit{
		{Scope: scope + "-ip", Key: s.requestIP(request), Limit: ipLimit},
		{Scope: scope + "-account", Key: userID, Limit: userLimit},
	}
	allowed, err := s.Store.ReserveAuthenticationAttempts(limits, authenticationAttemptWindow, time.Now().UTC())
	return limits, allowed, err
}

func (s *Server) reservePasskeyAttempt(request *http.Request) ([]store.AuthenticationAttemptLimit, bool, error) {
	limits := s.passkeyAttemptLimits(request)
	allowed, err := s.Store.ReserveAuthenticationAttempts(limits, authenticationAttemptWindow, time.Now().UTC())
	return limits, allowed, err
}

func (s *Server) passkeyAttemptLimits(request *http.Request) []store.AuthenticationAttemptLimit {
	return []store.AuthenticationAttemptLimit{{Scope: "passkey-ip", Key: s.requestIP(request), Limit: 30}}
}

func (s *Server) verifyCurrentSecondFactor(admin store.Admin, totpCode, recoveryCode string) (string, bool, error) {
	secret, err := s.adminTOTPSecret(admin)
	if err != nil {
		return "", false, err
	}
	if counter, valid := auth.MatchTOTP(secret, strings.TrimSpace(totpCode), time.Now()); valid {
		if err := s.Store.ConsumeAdminTOTPCounter(admin.ID, counter); err == nil {
			return "totp", true, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	}
	if strings.TrimSpace(recoveryCode) != "" {
		userID, err := s.Store.ConsumeRecoveryCode(auth.RecoveryCodeHash(recoveryCode))
		if err == nil && userID == admin.ID {
			return "recovery", true, nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	}
	return "", false, nil
}

func (s *Server) reauthenticate(response http.ResponseWriter, request *http.Request) {
	var input loginRequest
	if !readJSON(response, request, &input) {
		return
	}
	admin, err := s.Store.Admin()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	limits, allowed, err := s.reserveAuthenticationAttempt(request, "reauthenticate", admin.ID, 8, 12)
	if err != nil {
		writeError(response, http.StatusInternalServerError, errors.New("authentication rate limit is unavailable"))
		return
	}
	if !allowed {
		writeError(response, http.StatusTooManyRequests, errors.New("too many authentication attempts"))
		return
	}
	if !auth.VerifyPassword(admin.PasswordHash, input.Password) {
		writeError(response, http.StatusForbidden, errors.New("administrator credentials are incorrect"))
		return
	}
	method, valid, err := s.verifyCurrentSecondFactor(admin, input.TOTP, input.RecoveryCode)
	if err != nil {
		writeError(response, http.StatusInternalServerError, errors.New("two-factor authentication is unavailable"))
		return
	}
	if !valid {
		writeError(response, http.StatusForbidden, errors.New("administrator credentials are incorrect"))
		return
	}
	cookie, err := request.Cookie("cdn_session")
	if err != nil {
		writeError(response, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}
	replacementToken, err := auth.NewOpaqueToken(32)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	if err := s.Store.ElevateSession(cookie.Value, replacementToken, method, "", now, now.Add(recentAuthenticationLifetime)); err != nil {
		writeError(response, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}
	s.setAdminSessionCookie(response, request, replacementToken)
	_ = s.Store.ClearAuthenticationAttempts(limits)
	s.audit(request, admin.ID, "reauthenticate", "session", "", "method="+method)
	writeJSON(response, http.StatusOK, map[string]any{"ok": true, "elevated_until": now.Add(recentAuthenticationLifetime)})
}

func (s *Server) revokeOtherAdminSessions(request *http.Request) error {
	cookie, err := request.Cookie("cdn_session")
	if err != nil {
		return err
	}
	return s.Store.DeleteOtherSessions(adminID(request.Context()), cookie.Value)
}
