package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

func TestPasswordAndTOTP(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("expected password verification")
	}
	if VerifyPassword(hash, "incorrect") {
		t.Fatal("unexpected password verification")
	}
	secret := "JBSWY3DPEHPK3PXP"
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	code := totp(decoded, time.Unix(59, 0))
	if !VerifyTOTP(secret, code, time.Unix(59, 0)) {
		t.Fatal("RFC 6238 TOTP vector did not verify")
	}
	counter, matched := MatchTOTP(secret, code, time.Unix(59, 0))
	if !matched || counter != 1 {
		t.Fatalf("matched TOTP counter = %d, matched=%t, want counter 1", counter, matched)
	}
	if !ValidTOTPSecret("jbswy3dpehpk3pxp") || ValidTOTPSecret("not a secret!") {
		t.Fatal("unexpected TOTP secret validation")
	}
}

func TestRecoveryCode(t *testing.T) {
	code, err := NewRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(code, "-") {
		t.Fatalf("unexpected recovery-code format: %q", code)
	}
	if RecoveryCodeHash(code) == RecoveryCodeHash(code+"x") {
		t.Fatal("recovery code hash collision")
	}
}
