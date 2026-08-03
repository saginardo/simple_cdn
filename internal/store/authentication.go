package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AuthenticationAttemptLimit struct {
	Scope string
	Key   string
	Limit int
}

func (s *Store) CreateAuthenticatedSession(userID, token, csrf, method, authenticatorID string, authenticatedAt, elevatedUntil, expiresAt time.Time) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(token) == "" || strings.TrimSpace(csrf) == "" || strings.TrimSpace(method) == "" {
		return errors.New("complete authenticated session details are required")
	}
	_, err := s.db.Exec(`INSERT INTO sessions(
		id, user_id, token_hash, csrf_token, auth_method, authenticator_id, authenticated_at, elevated_until, expires_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), userID, hashToken(token), csrf, method, strings.TrimSpace(authenticatorID), stamp(authenticatedAt), stamp(elevatedUntil), stamp(expiresAt), stamp(now()))
	return err
}

func (s *Store) ElevateSession(currentToken, replacementToken, method, authenticatorID string, authenticatedAt, elevatedUntil time.Time) error {
	if strings.TrimSpace(currentToken) == "" || strings.TrimSpace(replacementToken) == "" || strings.TrimSpace(method) == "" {
		return errors.New("complete session elevation details are required")
	}
	result, err := s.db.Exec(`UPDATE sessions SET token_hash = ?, auth_method = ?, authenticator_id = ?, authenticated_at = ?, elevated_until = ?
		WHERE token_hash = ? AND expires_at > ?`, hashToken(replacementToken), method, strings.TrimSpace(authenticatorID), stamp(authenticatedAt), stamp(elevatedUntil), hashToken(currentToken), stamp(now()))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteOtherSessions(userID, keepToken string) error {
	result, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, hashToken(keepToken))
	if err != nil {
		return err
	}
	_, err = result.RowsAffected()
	return err
}

func (s *Store) ConsumeAdminTOTPCounter(userID string, counter int64) error {
	result, err := s.db.Exec(`UPDATE admin_users SET last_totp_counter = ?, updated_at = ?
		WHERE id = ? AND (last_totp_counter IS NULL OR last_totp_counter < ?)`, counter, stamp(now()), userID, counter)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReserveAuthenticationAttempts(limits []AuthenticationAttemptLimit, window time.Duration, attemptedAt time.Time) (bool, error) {
	if len(limits) == 0 || window <= 0 {
		return false, errors.New("authentication rate limits are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	cutoff := attemptedAt.Add(-window)
	if _, err := tx.Exec(`DELETE FROM authentication_attempts WHERE attempted_at <= ?`, stamp(cutoff)); err != nil {
		return false, err
	}
	for _, limit := range limits {
		if strings.TrimSpace(limit.Scope) == "" || strings.TrimSpace(limit.Key) == "" || limit.Limit <= 0 {
			return false, errors.New("invalid authentication rate limit")
		}
		var count int
		keyHash := hashToken(limit.Scope + "\x00" + limit.Key)
		if err := tx.QueryRow(`SELECT COUNT(*) FROM authentication_attempts WHERE scope = ? AND key_hash = ? AND attempted_at > ?`, limit.Scope, keyHash, stamp(cutoff)).Scan(&count); err != nil {
			return false, err
		}
		if count >= limit.Limit {
			return false, nil
		}
	}
	for _, limit := range limits {
		if _, err := tx.Exec(`INSERT INTO authentication_attempts(id, scope, key_hash, attempted_at) VALUES (?, ?, ?, ?)`,
			uuid.NewString(), limit.Scope, hashToken(limit.Scope+"\x00"+limit.Key), stamp(attemptedAt)); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ClearAuthenticationAttempts(limits []AuthenticationAttemptLimit) error {
	if len(limits) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, limit := range limits {
		if _, err := tx.Exec(`DELETE FROM authentication_attempts WHERE scope = ? AND key_hash = ?`,
			limit.Scope, hashToken(limit.Scope+"\x00"+limit.Key)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AuthenticationAttemptCount(scope, key string, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM authentication_attempts WHERE scope = ? AND key_hash = ? AND attempted_at > ?`,
		scope, hashToken(scope+"\x00"+key), stamp(since)).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return count, err
}
