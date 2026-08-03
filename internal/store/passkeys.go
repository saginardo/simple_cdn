package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const webAuthnUserHandleBytes = 64

type PasskeyCredential struct {
	ID         string
	RPID       string
	UserID     string
	Name       string
	Ciphertext []byte
	LastUsedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type WebAuthnChallenge struct {
	Purpose     string
	UserID      string
	RPID        string
	Label       string
	SessionJSON []byte
	ExpiresAt   time.Time
}

func (s *Store) WebAuthnUserHandle(userID, rpID string) ([]byte, error) {
	userID = strings.TrimSpace(userID)
	rpID = strings.TrimSpace(rpID)
	if userID == "" || rpID == "" {
		return nil, errors.New("WebAuthn user and relying party are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var handle []byte
	err = tx.QueryRow(`SELECT user_handle FROM webauthn_users WHERE rpid = ? AND user_id = ?`, rpID, userID).Scan(&handle)
	if err == nil {
		return append([]byte(nil), handle...), tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	handle = make([]byte, webAuthnUserHandleBytes)
	if _, err := rand.Read(handle); err != nil {
		return nil, fmt.Errorf("generate WebAuthn user handle: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO webauthn_users(rpid, user_id, user_handle, created_at) VALUES (?, ?, ?, ?)`, rpID, userID, handle, stamp(now())); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return append([]byte(nil), handle...), nil
}

func (s *Store) PasskeyCredentialRecords(userID, rpID string) ([]PasskeyCredential, error) {
	rows, err := s.db.Query(`SELECT credential_id, rpid, user_id, name, credential_ciphertext, last_used_at, created_at, updated_at
		FROM passkey_credentials WHERE user_id = ? AND rpid = ? ORDER BY created_at`, userID, rpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := make([]PasskeyCredential, 0)
	for rows.Next() {
		credential, err := scanPasskeyCredential(rows)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

func (s *Store) PasskeyCredentialRecordsForUser(userID string) ([]PasskeyCredential, error) {
	rows, err := s.db.Query(`SELECT credential_id, rpid, user_id, name, credential_ciphertext, last_used_at, created_at, updated_at
		FROM passkey_credentials WHERE user_id = ? ORDER BY rpid, created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := make([]PasskeyCredential, 0)
	for rows.Next() {
		credential, err := scanPasskeyCredential(rows)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

func (s *Store) PasskeyOwner(rpID, credentialID string, userHandle []byte) (string, error) {
	var userID string
	err := s.db.QueryRow(`SELECT credentials.user_id
		FROM passkey_credentials AS credentials
		JOIN webauthn_users AS users ON users.rpid = credentials.rpid AND users.user_id = credentials.user_id
		JOIN admin_users AS admins ON admins.id = credentials.user_id
		WHERE credentials.rpid = ? AND credentials.credential_id = ? AND users.user_handle = ? AND admins.passkey_enabled = 1`,
		rpID, credentialID, userHandle).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return userID, err
}

func (s *Store) SavePasskeyCredential(userID, rpID, credentialID, name string, ciphertext []byte) error {
	userID = strings.TrimSpace(userID)
	rpID = strings.TrimSpace(rpID)
	credentialID = strings.TrimSpace(credentialID)
	name = strings.TrimSpace(name)
	if userID == "" || rpID == "" || credentialID == "" || name == "" || len(ciphertext) == 0 {
		return errors.New("complete passkey credential details are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := stamp(now())
	if _, err := tx.Exec(`INSERT INTO passkey_credentials(rpid, credential_id, user_id, name, credential_ciphertext, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rpID, credentialID, userID, name, ciphertext, ts, ts); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE admin_users SET passkey_enabled = 1, updated_at = ? WHERE id = ?`, ts, userID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) UpdatePasskeyCredential(userID, rpID, credentialID string, ciphertext []byte, usedAt time.Time) error {
	result, err := s.db.Exec(`UPDATE passkey_credentials SET credential_ciphertext = ?, last_used_at = ?, updated_at = ?
		WHERE user_id = ? AND rpid = ? AND credential_id = ?`, ciphertext, stamp(usedAt), stamp(now()), userID, rpID, credentialID)
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

func (s *Store) SetPasskeyEnabled(userID, rpID string, enabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if enabled {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM passkey_credentials WHERE user_id = ? AND rpid = ?`, userID, rpID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return errors.New("register a passkey before enabling passkey login")
		}
	}
	result, err := tx.Exec(`UPDATE admin_users SET passkey_enabled = ?, updated_at = ? WHERE id = ?`, enabled, stamp(now()), userID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) DeletePasskeyCredential(userID, rpID, credentialID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM passkey_credentials WHERE user_id = ? AND rpid = ? AND credential_id = ?`, userID, rpID, credentialID)
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
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM passkey_credentials WHERE user_id = ?`, userID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.Exec(`UPDATE admin_users SET passkey_enabled = 0, updated_at = ? WHERE id = ?`, stamp(now()), userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CreateWebAuthnChallenge(token, purpose, userID, rpID, label string, sessionJSON []byte, expiresAt time.Time) error {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(purpose) == "" || strings.TrimSpace(rpID) == "" || len(sessionJSON) == 0 {
		return errors.New("complete WebAuthn challenge details are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM webauthn_challenges WHERE expires_at <= ?`, stamp(now())); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO webauthn_challenges(token_hash, purpose, user_id, rpid, label, session_json, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, hashToken(token), purpose, userID, rpID, label, sessionJSON, stamp(expiresAt), stamp(now()))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConsumeWebAuthnChallenge(token, purpose string) (WebAuthnChallenge, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WebAuthnChallenge{}, err
	}
	defer tx.Rollback()
	var challenge WebAuthnChallenge
	var expiresAt string
	err = tx.QueryRow(`SELECT purpose, user_id, rpid, label, session_json, expires_at FROM webauthn_challenges WHERE token_hash = ? AND purpose = ?`,
		hashToken(token), purpose).Scan(&challenge.Purpose, &challenge.UserID, &challenge.RPID, &challenge.Label, &challenge.SessionJSON, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WebAuthnChallenge{}, ErrNotFound
	}
	if err != nil {
		return WebAuthnChallenge{}, err
	}
	result, err := tx.Exec(`DELETE FROM webauthn_challenges WHERE token_hash = ?`, hashToken(token))
	if err != nil {
		return WebAuthnChallenge{}, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return WebAuthnChallenge{}, err
	}
	if deleted != 1 {
		return WebAuthnChallenge{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return WebAuthnChallenge{}, err
	}
	challenge.ExpiresAt, err = parseTime(expiresAt)
	if err != nil {
		return WebAuthnChallenge{}, err
	}
	if !challenge.ExpiresAt.After(now()) {
		return WebAuthnChallenge{}, ErrNotFound
	}
	return challenge, nil
}

type passkeyCredentialScanner interface {
	Scan(dest ...any) error
}

func scanPasskeyCredential(scanner passkeyCredentialScanner) (PasskeyCredential, error) {
	var credential PasskeyCredential
	var lastUsedAt sql.NullString
	var createdAt, updatedAt string
	if err := scanner.Scan(&credential.ID, &credential.RPID, &credential.UserID, &credential.Name, &credential.Ciphertext, &lastUsedAt, &createdAt, &updatedAt); err != nil {
		return PasskeyCredential{}, err
	}
	var err error
	if credential.CreatedAt, err = parseTime(createdAt); err != nil {
		return PasskeyCredential{}, err
	}
	if credential.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return PasskeyCredential{}, err
	}
	if lastUsedAt.Valid {
		value, err := parseTime(lastUsedAt.String)
		if err != nil {
			return PasskeyCredential{}, err
		}
		credential.LastUsedAt = &value
	}
	return credential, nil
}
