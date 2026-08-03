package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
)

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("random salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

func NewOpaqueToken(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func NewRecoveryCode() (string, error) {
	value, err := NewOpaqueToken(12)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(value[:8] + "-" + value[8:]), nil
}

func RecoveryCodeHash(code string) string {
	sum := sha256.Sum256([]byte("cdn-platform-recovery-v1:" + strings.TrimSpace(code)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func NewTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

func NormalizeTOTPSecret(secret string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
}

func ValidTOTPSecret(secret string) bool {
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(NormalizeTOTPSecret(secret))
	return err == nil && len(decoded) >= 10
}

// TOTP is intentionally implemented locally to avoid a dependency for a small, offline-safe control plane.
func VerifyTOTP(secret, code string, now time.Time) bool {
	_, valid := MatchTOTP(secret, code, now)
	return valid
}

// MatchTOTP returns the exact time-step counter so callers can atomically
// prevent a valid one-time password from being accepted more than once.
func MatchTOTP(secret, code string, now time.Time) (int64, bool) {
	secret = NormalizeTOTPSecret(secret)
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(code) != 6 {
		return 0, false
	}
	for _, offset := range []int64{0, -1, 1} {
		candidate := now.Add(time.Duration(offset*30) * time.Second)
		if subtle.ConstantTimeCompare([]byte(totp(decoded, candidate)), []byte(code)) == 1 {
			return candidate.Unix() / 30, true
		}
	}
	return 0, false
}

func totp(secret []byte, now time.Time) string {
	counter := uint64(now.Unix() / 30)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(buf)
	sum := mac.Sum(nil)
	offset := int(sum[len(sum)-1] & 0x0f)
	value := (uint32(sum[offset])&0x7f)<<24 | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	return fmt.Sprintf("%06d", value%1_000_000)
}
