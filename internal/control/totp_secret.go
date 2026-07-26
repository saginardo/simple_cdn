package control

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"simple_cdn/internal/auth"
	"simple_cdn/internal/store"
)

const encryptedTOTPSecretPrefix = "enc:totp:v1:"

func (s *Server) MigrateAdminTOTPSecret() error {
	if s.Store == nil {
		return errors.New("admin store is not configured")
	}
	admin, err := s.Store.Admin()
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read administrator: %w", err)
	}
	_, err = s.adminTOTPSecret(admin)
	if err != nil {
		return fmt.Errorf("migrate administrator TOTP secret: %w", err)
	}
	return nil
}

func (s *Server) adminTOTPSecret(admin store.Admin) (string, error) {
	secret, legacy, err := s.decryptTOTPSecret(admin.TOTPSecret)
	if err != nil {
		return "", err
	}
	if !legacy {
		return secret, nil
	}
	encrypted, err := s.encryptTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	if err := s.Store.ReplaceAdminTOTPSecret(admin.ID, encrypted); err != nil {
		return "", fmt.Errorf("save encrypted TOTP secret: %w", err)
	}
	return secret, nil
}

func (s *Server) encryptTOTPSecret(secret string) (string, error) {
	if s.Cipher == nil {
		return "", errors.New("CONTROL_ENCRYPTION_KEY is not configured")
	}
	secret = auth.NormalizeTOTPSecret(secret)
	if !auth.ValidTOTPSecret(secret) {
		return "", errors.New("invalid TOTP secret")
	}
	ciphertext, err := s.Cipher.Encrypt([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("encrypt TOTP secret: %w", err)
	}
	return encryptedTOTPSecretPrefix + base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

func (s *Server) decryptTOTPSecret(stored string) (secret string, legacy bool, err error) {
	if s.Cipher == nil {
		return "", false, errors.New("CONTROL_ENCRYPTION_KEY is not configured")
	}
	if !strings.HasPrefix(stored, encryptedTOTPSecretPrefix) {
		secret = auth.NormalizeTOTPSecret(stored)
		if !auth.ValidTOTPSecret(secret) {
			return "", false, errors.New("stored TOTP secret is invalid")
		}
		return secret, true, nil
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(stored, encryptedTOTPSecretPrefix))
	if err != nil {
		return "", false, fmt.Errorf("decode encrypted TOTP secret: %w", err)
	}
	plaintext, err := s.Cipher.Decrypt(ciphertext)
	if err != nil {
		return "", false, fmt.Errorf("decrypt encrypted TOTP secret: %w", err)
	}
	secret = auth.NormalizeTOTPSecret(string(plaintext))
	if !auth.ValidTOTPSecret(secret) {
		return "", false, errors.New("decrypted TOTP secret is invalid")
	}
	return secret, false, nil
}
