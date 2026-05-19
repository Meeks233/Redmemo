package transport

import (
	"net/http"
	"testing"
	"time"
)

func TestNewSpoofedClient(t *testing.T) {
	c := NewSpoofedClient(7 * time.Second)
	if c == nil {
		t.Fatal("NewSpoofedClient returned nil")
	}
	if c.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want 7s", c.Timeout)
	}
	if c.Transport == nil {
		t.Fatal("Transport is nil")
	}
	if _, ok := c.Transport.(http.RoundTripper); !ok {
		t.Errorf("Transport is %T, want http.RoundTripper", c.Transport)
	}
}

func TestNewSpoofedTransport(t *testing.T) {
	rt := NewSpoofedTransport()
	if rt == nil {
		t.Fatal("NewSpoofedTransport returned nil")
	}
	srt, ok := rt.(*spoofedRoundTripper)
	if !ok {
		t.Fatalf("Transport is %T, want *spoofedRoundTripper", rt)
	}
	if srt.h1 == nil {
		t.Fatal("h1 transport must be set")
	}
	if srt.h2 == nil {
		t.Fatal("h2 transport must be set")
	}
	// The uTLS spoofing hangs off DialTLSContext on both transports — without
	// it the transport would fall back to Go's stdlib ClientHello.
	if srt.h1.DialTLSContext == nil {
		t.Error("h1.DialTLSContext must be set for uTLS fingerprint spoofing")
	}
	if srt.h2.DialTLSContext == nil {
		t.Error("h2.DialTLSContext must be set for uTLS fingerprint spoofing")
	}
	// HTTP/2 stays off on the h1 transport — protocol selection happens via
	// real ALPN, not via Go's auto-upgrade path.
	if srt.h1.ForceAttemptHTTP2 {
		t.Error("h1.ForceAttemptHTTP2 must be false")
	}
	if srt.h1.MaxIdleConns != 100 {
		t.Errorf("h1.MaxIdleConns = %d, want 100", srt.h1.MaxIdleConns)
	}
	if srt.h1.MaxIdleConnsPerHost != 10 {
		t.Errorf("h1.MaxIdleConnsPerHost = %d, want 10", srt.h1.MaxIdleConnsPerHost)
	}
	if srt.h1.IdleConnTimeout != 90*time.Second {
		t.Errorf("h1.IdleConnTimeout = %v, want 90s", srt.h1.IdleConnTimeout)
	}
}
