package transport

import (
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
)

func TestNewSpoofedClient(t *testing.T) {
	c := NewSpoofedClient(7 * time.Second)
	if c == nil {
		t.Fatal("NewSpoofedClient returned nil")
	}
}

func TestNewMediaSpoofedClient(t *testing.T) {
	c := NewMediaSpoofedClient(7 * time.Second)
	if c == nil {
		t.Fatal("NewMediaSpoofedClient returned nil")
	}
}

// TestMediaClientProfile pins the inflated h2 windows that keep multi-MiB
// v.redd.it segment streams from being RST_STREAM'd at the 16 MiB ceiling.
func TestMediaClientProfile(t *testing.T) {
	p := mediaClientProfile()

	settings := p.GetSettings()
	if v, ok := settings[http2.SettingInitialWindowSize]; !ok || v != 67108864 {
		t.Errorf("INITIAL_WINDOW_SIZE = %d (present=%v), want 67108864 (64 MiB)", v, ok)
	}
	if cf := p.GetConnectionFlow(); cf != 268435455 {
		t.Errorf("connectionFlow = %d, want 268435455 (~256 MiB)", cf)
	}
}

// TestRedditClientProfile pins the OkHttp HTTP/2 fingerprint that closes the
// h2 gap: Akamai 4:16777216|16711681|0|m,p,a,s.
func TestRedditClientProfile(t *testing.T) {
	p := redditClientProfile()

	settings := p.GetSettings()
	if len(settings) != 1 {
		t.Fatalf("settings has %d params, want exactly 1", len(settings))
	}
	if v, ok := settings[http2.SettingInitialWindowSize]; !ok || v != 16777216 {
		t.Errorf("INITIAL_WINDOW_SIZE = %d (present=%v), want 16777216", v, ok)
	}

	order := p.GetSettingsOrder()
	if len(order) != 1 || order[0] != http2.SettingInitialWindowSize {
		t.Errorf("settingsOrder = %v, want [INITIAL_WINDOW_SIZE]", order)
	}

	if cf := p.GetConnectionFlow(); cf != 16711681 {
		t.Errorf("connectionFlow = %d, want 16711681", cf)
	}

	want := []string{":method", ":path", ":authority", ":scheme"}
	got := p.GetPseudoHeaderOrder()
	if len(got) != len(want) {
		t.Fatalf("pseudoHeaderOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pseudoHeaderOrder[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if prio := p.GetPriorities(); len(prio) != 0 {
		t.Errorf("priorities = %v, want none (no PRIORITY frames)", prio)
	}
}

// TestRedditClientHelloSpec guards the audited ClientHello against regression:
// JA3/JA4 depend on the cipher set and ALPN staying exactly as captured.
func TestRedditClientHelloSpec(t *testing.T) {
	spec := redditClientHelloSpec()
	if len(spec.CipherSuites) != 15 {
		t.Errorf("cipher suite count = %d, want 15", len(spec.CipherSuites))
	}
	if len(spec.Extensions) != 13 {
		t.Errorf("extension count = %d, want 13", len(spec.Extensions))
	}
}

func TestApplyHeaderOrder(t *testing.T) {
	req, err := http.NewRequest("GET", "https://oauth.reddit.com/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ApplyHeaderOrder(req)

	if got := req.Header[http.HeaderOrderKey]; len(got) == 0 {
		t.Error("HeaderOrderKey not set on request")
	}
	pseudo := req.Header[http.PHeaderOrderKey]
	if len(pseudo) != 4 || pseudo[0] != ":method" {
		t.Errorf("PHeaderOrderKey = %v, want method/path/authority/scheme", pseudo)
	}
}
