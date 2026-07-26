package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"simple_cdn/internal/auth"
)

const initializationTokenFileMode = 0o600

// EnsureInitializationToken creates the local bootstrap token once and retains it
// across restarts until setup succeeds.
func EnsureInitializationToken(path string) (bool, error) {
	path, err := cleanInitializationTokenPath(path)
	if err != nil {
		return false, err
	}
	if _, err := readInitializationToken(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create initialization token directory: %w", err)
	}
	token, err := auth.NewOpaqueToken(32)
	if err != nil {
		return false, fmt.Errorf("generate initialization token: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, initializationTokenFileMode)
	if errors.Is(err, os.ErrExist) {
		_, readErr := readInitializationToken(path)
		return false, readErr
	}
	if err != nil {
		return false, fmt.Errorf("create initialization token file: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(initializationTokenFileMode); err != nil {
		return false, fmt.Errorf("set initialization token permissions: %w", err)
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		return false, fmt.Errorf("write initialization token: %w", err)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync initialization token: %w", err)
	}
	return true, nil
}

func VerifyInitializationToken(path, supplied string) error {
	path, err := cleanInitializationTokenPath(path)
	if err != nil {
		return err
	}
	expected, err := readInitializationToken(path)
	if err != nil {
		return err
	}
	expectedHash := sha256.Sum256([]byte(expected))
	suppliedHash := sha256.Sum256([]byte(strings.TrimSpace(supplied)))
	if subtle.ConstantTimeCompare(expectedHash[:], suppliedHash[:]) != 1 {
		return errors.New("initialization token does not match")
	}
	return nil
}

// ConsumeInitializationToken removes a token only after it still matches the
// value used for the completed setup request.
func ConsumeInitializationToken(path, supplied string) error {
	if err := VerifyInitializationToken(path, supplied); err != nil {
		return err
	}
	return RemoveInitializationToken(path)
}

func RemoveInitializationToken(path string) error {
	path, err := cleanInitializationTokenPath(path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect initialization token: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("initialization token path must be a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove initialization token: %w", err)
	}
	return nil
}

func readInitializationToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("initialization token path must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("initialization token file permissions must be 0600")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read initialization token: %w", err)
	}
	token := strings.TrimSpace(string(contents))
	if len(token) < 32 || len(token) > 256 {
		return "", errors.New("initialization token file is invalid")
	}
	return token, nil
}

func cleanInitializationTokenPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("initialization token path is required")
	}
	return filepath.Clean(path), nil
}
