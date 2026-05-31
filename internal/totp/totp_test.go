package totp

import (
	"testing"
	"time"
)

func TestCodeRoundtrip(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	code, err := Code(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != Digits {
		t.Fatalf("digits=%d, want %d", len(code), Digits)
	}
	if !Verify(secret, code, now) {
		t.Fatal("verify failed for fresh code")
	}
	if Verify(secret, code, now.Add(120*time.Second)) {
		t.Fatal("code accepted outside skew window")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	secret, _ := NewSecret()
	now := time.Now()
	if Verify(secret, "12345", now) {
		t.Fatal("short code accepted")
	}
	if Verify(secret, "abcdef", now) {
		t.Fatal("non-digit code accepted")
	}
}

func TestOTPAuthURI(t *testing.T) {
	uri := OTPAuthURI("ABCD")
	if uri == "" {
		t.Fatal("empty uri")
	}
}
