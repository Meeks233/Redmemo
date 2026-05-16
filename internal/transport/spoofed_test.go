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
	if _, ok := c.Transport.(*http.Transport); !ok {
		t.Errorf("Transport is %T, want *http.Transport", c.Transport)
	}
}

func TestNewSpoofedTransport(t *testing.T) {
	tr := NewSpoofedTransport()
	if tr == nil {
		t.Fatal("NewSpoofedTransport returned nil")
	}
	// The uTLS spoofing hangs off DialTLSContext — without it the transport
	// would fall back to Go's stdlib ClientHello.
	if tr.DialTLSContext == nil {
		t.Error("DialTLSContext must be set for uTLS fingerprint spoofing")
	}
	// HTTP/2 must stay off: the spoofed ClientHello advertises only http/1.1,
	// and Go's transport cannot drive h2 over this connection.
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be false")
	}
	if tr.MaxIdleConns != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 10 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 10", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
}
