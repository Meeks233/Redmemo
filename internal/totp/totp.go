// Package totp implements RFC 6238 time-based one-time passwords.
//
// Settings authentication uses a single TOTP secret stored in the
// site_settings table. The secret is generated once during enrollment, never
// shown again, and rotated only by `redmemo --reset-totp` (or an env-secret
// re-entry that clears totp_secret).
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	Period   = 30
	Digits   = 6
	Issuer   = "RedMemo"
	Account  = "settings"
	secretLn = 20 // 160 bits — SHA-1 native block
)

// NewSecret returns a fresh base32-encoded secret (no padding, uppercase).
func NewSecret() (string, error) {
	buf := make([]byte, secretLn)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.TrimRight(base32.StdEncoding.EncodeToString(buf), "="), nil
}

// Code returns the 6-digit code valid at t for secret.
func Code(secret string, t time.Time) (string, error) {
	key, err := decode(secret)
	if err != nil {
		return "", err
	}
	counter := uint64(t.Unix()) / Period
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, bin%mod), nil
}

// Verify checks code against the current 30s window and the immediately
// adjacent windows (±1) for small clock skew.
func Verify(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return false
	}
	if _, err := strconv.Atoi(code); err != nil {
		return false
	}
	for _, delta := range []int{0, -1, 1} {
		want, err := Code(secret, now.Add(time.Duration(delta)*Period*time.Second))
		if err != nil {
			return false
		}
		if hmac.Equal([]byte(want), []byte(code)) {
			return true
		}
	}
	return false
}

// OTPAuthURI builds the otpauth:// URI suitable for QR or manual import.
func OTPAuthURI(secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", Issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", strconv.Itoa(Digits))
	v.Set("period", strconv.Itoa(Period))
	return "otpauth://totp/" + url.PathEscape(Issuer+":"+Account) + "?" + v.Encode()
}

// QRCodePNG renders the otpauth URI as a PNG of the given pixel size.
func QRCodePNG(secret string, size int) ([]byte, error) {
	return qrcode.Encode(OTPAuthURI(secret), qrcode.Medium, size)
}

func decode(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	if pad := len(s) % 8; pad != 0 {
		s += strings.Repeat("=", 8-pad)
	}
	return base32.StdEncoding.DecodeString(s)
}
