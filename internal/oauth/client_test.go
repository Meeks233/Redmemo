package oauth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/config"
)

// --- pure helpers ---

func TestBasicAuth(t *testing.T) {
	cases := []struct{ user, pass string }{
		{"clientid", "secret"},
		{"clientid", ""}, // mobile spoof uses an empty secret
		{"", ""},
	}
	for _, c := range cases {
		got := basicAuth(c.user, c.pass)
		want := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.pass))
		if got != want {
			t.Errorf("basicAuth(%q,%q) = %q, want %q", c.user, c.pass, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},        // shorter than limit — unchanged
		{"hello", 5, "hello"},         // exactly the limit — unchanged
		{"hello world", 5, "hello..."}, // longer — truncated with ellipsis
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := truncate([]byte(c.in), c.maxLen); got != c.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", c.in, c.maxLen, got, c.want)
		}
	}
}

// --- Authenticate (all auth traffic pinned to a local server) ---

func TestAuthenticate_MobileSpoofSuccess(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Header().Set("x-reddit-loid", "LOID-123")
		w.Header().Set("x-reddit-session", "SESS-456")
		w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
	}))
	defer srv.Close()

	c := newClientToServer(t, srv)
	res, err := c.Authenticate(config.OAuthTokenConfig{Backend: "mobile_spoof"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("auth request method = %q, want POST", method)
	}
	if res.AccessToken != "tok-abc" {
		t.Errorf("AccessToken = %q", res.AccessToken)
	}
	if res.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600", res.ExpiresIn)
	}
	if res.Headers["x-reddit-loid"] != "LOID-123" {
		t.Errorf("loid header = %q", res.Headers["x-reddit-loid"])
	}
	if res.Headers["x-reddit-session"] != "SESS-456" {
		t.Errorf("session header = %q", res.Headers["x-reddit-session"])
	}
	if res.Identity.DeviceID == "" {
		t.Error("Identity.DeviceID should be populated")
	}
}

// generic_web is decoupled: a failing mobile_spoof must NOT fall through to
// the browser-spoof endpoint. It exhausts the mobile retry budget and errors.
func TestAuthenticate_NoGenericWebFallthrough(t *testing.T) {
	var mobileHits, genericHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/auth/v2/oauth/access-token"):
			mobileHits++
			w.WriteHeader(http.StatusInternalServerError) // mobile spoof always fails
		case strings.Contains(r.URL.Path, "/api/v1/access_token"):
			genericHits++
			w.Write([]byte(`{"access_token":"web-tok","expires_in":7200}`))
		default:
			t.Errorf("unexpected auth path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newClientToServer(t, srv)
	_, err := c.Authenticate(config.OAuthTokenConfig{Backend: "mobile_spoof"})
	if err == nil {
		t.Fatal("expected an error — generic_web fallthrough is decoupled")
	}
	if mobileHits != maxRetries {
		t.Errorf("mobile attempts = %d, want %d (full retry budget)", mobileHits, maxRetries)
	}
	if genericHits != 0 {
		t.Errorf("generic web endpoint hit %d times, want 0 (decoupled)", genericHits)
	}
}

func TestAuthenticate_AllAttemptsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 200 OK but an empty access_token — every parse fails validation.
		w.Write([]byte(`{"access_token":"","expires_in":0}`))
	}))
	defer srv.Close()

	c := newClientToServer(t, srv)
	_, err := c.Authenticate(config.OAuthTokenConfig{Backend: "mobile_spoof"})
	if err == nil {
		t.Fatal("expected an error when every auth attempt yields an empty token")
	}
	if !strings.Contains(err.Error(), "all auth attempts failed") {
		t.Errorf("err = %v, want 'all auth attempts failed'", err)
	}
}

