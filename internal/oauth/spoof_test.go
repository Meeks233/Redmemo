package oauth

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestGenerateIdentity_UserAgent(t *testing.T) {
	id := GenerateIdentity()
	if !strings.Contains(id.UserAgent, "Reddit/") {
		t.Errorf("UserAgent missing 'Reddit/': %s", id.UserAgent)
	}
	if !strings.Contains(id.UserAgent, "Android") {
		t.Errorf("UserAgent missing 'Android': %s", id.UserAgent)
	}
}

func TestGenerateIdentity_DeviceIDFormat(t *testing.T) {
	id := GenerateIdentity()
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(id.DeviceID) {
		t.Errorf("DeviceID not valid UUID v4: %s", id.DeviceID)
	}
}

func TestGenerateIdentity_HeadersNonEmpty(t *testing.T) {
	id := GenerateIdentity()
	if len(id.Headers) == 0 {
		t.Error("Headers map is empty")
	}
}

func TestGenerateIdentity_DeviceIDInHeaders(t *testing.T) {
	id := GenerateIdentity()
	if v, ok := id.Headers["client-vendor-id"]; !ok || v != id.DeviceID {
		t.Errorf("client-vendor-id = %q, want %q", v, id.DeviceID)
	}
	if v, ok := id.Headers["X-Reddit-Device-Id"]; !ok || v != id.DeviceID {
		t.Errorf("X-Reddit-Device-Id = %q, want %q", v, id.DeviceID)
	}
}

func TestGenerateIdentity_Uniqueness(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		id := GenerateIdentity()
		if seen[id.DeviceID] {
			t.Fatalf("duplicate DeviceID: %s", id.DeviceID)
		}
		seen[id.DeviceID] = true
	}
}

func TestGenerateIdentity_AndroidVersionRange(t *testing.T) {
	re := regexp.MustCompile(`Android (\d+)`)
	for range 50 {
		id := GenerateIdentity()
		m := re.FindStringSubmatch(id.UserAgent)
		if m == nil {
			t.Fatalf("no Android version in UA: %s", id.UserAgent)
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("non-integer Android version: %s", m[1])
		}
		if v < 9 || v > 14 {
			t.Errorf("Android version %d out of range [9,14]", v)
		}
	}
}
