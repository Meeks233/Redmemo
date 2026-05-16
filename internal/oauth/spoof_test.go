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

func TestGenerateWebIdentity_NilPool(t *testing.T) {
	// A nil UA pool must fall back to the built-in web user-agent list
	// rather than panicking.
	id := GenerateWebIdentity(nil)
	if id.UserAgent == "" {
		t.Error("UserAgent is empty")
	}
	if id.Headers["User-Agent"] != id.UserAgent {
		t.Errorf("Headers[User-Agent] = %q, want %q", id.Headers["User-Agent"], id.UserAgent)
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(id.DeviceID) {
		t.Errorf("DeviceID is not a valid UUID v4: %s", id.DeviceID)
	}
}

func TestGenerateWebIdentity_Uniqueness(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		id := GenerateWebIdentity(nil)
		if seen[id.DeviceID] {
			t.Fatalf("duplicate DeviceID: %s", id.DeviceID)
		}
		seen[id.DeviceID] = true
	}
}

func TestParseOSRange(t *testing.T) {
	cases := []struct {
		in       string
		min, max int
		ok       bool
	}{
		{"13", 13, 13, true},
		{"12-15", 12, 15, true},
		{" 12 - 15 ", 12, 15, true},
		{"15-12", 0, 0, false},
		{"abc", 0, 0, false},
		{"0", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		min, max, ok := parseOSRange(c.in)
		if ok != c.ok || (ok && (min != c.min || max != c.max)) {
			t.Errorf("parseOSRange(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, min, max, ok, c.min, c.max, c.ok)
		}
	}
}

func TestDeriveAppVersionFromDate(t *testing.T) {
	v, ok := deriveAppVersionFromDate("2026-02-20")
	if !ok {
		t.Fatal("deriveAppVersionFromDate(2026-02-20) failed")
	}
	// Expect "Version 2026.WW.0/Build 26WWSSS" where the WW in the version
	// string matches the WW embedded in the build number.
	re := regexp.MustCompile(`^Version 2026\.(\d{2})\.0/Build 26(\d{2})\d{3}$`)
	m := re.FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("unexpected derived format: %s", v)
	}
	if m[1] != m[2] {
		t.Errorf("version week %s != build week %s in %s", m[1], m[2], v)
	}

	for _, bad := range []string{"", "not-a-date", "2026/02/20", "2026-13-01", "2026-02-29"} {
		if _, ok := deriveAppVersionFromDate(bad); ok {
			t.Errorf("deriveAppVersionFromDate(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestBuildUA_FullOverride(t *testing.T) {
	s := androidUASettings{fullUA: "Reddit/Custom/Android 99"}
	if got := s.buildUA(); got != "Reddit/Custom/Android 99" {
		t.Errorf("buildUA() = %q, want verbatim override", got)
	}
}

func TestBuildUA_FixedOSVersion(t *testing.T) {
	s := androidUASettings{
		appVersions: []string{"Version 2026.07.0/Build 2607141"},
		osMin:       14,
		osMax:       14,
	}
	for range 20 {
		if got := s.buildUA(); got != "Reddit/Version 2026.07.0/Build 2607141/Android 14" {
			t.Fatalf("buildUA() = %q", got)
		}
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
		if v < defaultAndroidOSMin || v > defaultAndroidOSMax {
			t.Errorf("Android version %d out of range [%d,%d]", v, defaultAndroidOSMin, defaultAndroidOSMax)
		}
	}
}
