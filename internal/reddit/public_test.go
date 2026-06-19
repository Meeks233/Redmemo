package reddit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// --- token bucket (tryAcquire) ---

func newBucket(tokens, max int, refill time.Duration, lastRefill time.Time) *PublicClient {
	return &PublicClient{
		tokens:     tokens,
		maxTokens:  max,
		refillRate: refill,
		lastRefill: lastRefill,
	}
}

func TestTryAcquire_DrainsFullBucket(t *testing.T) {
	// No time-based refill: lastRefill=now with a long refillRate.
	c := newBucket(8, 8, time.Hour, time.Now())
	for i := 0; i < 8; i++ {
		if !c.tryAcquire() {
			t.Fatalf("acquire %d should have succeeded", i+1)
		}
	}
	if c.tryAcquire() {
		t.Error("9th acquire should fail — bucket is empty")
	}
}

func TestTryAcquire_EmptyBucketNoRefill(t *testing.T) {
	c := newBucket(0, 8, time.Hour, time.Now())
	if c.tryAcquire() {
		t.Error("acquire on an empty, un-refilled bucket should fail")
	}
}

func TestTryAcquire_RefillsOverTime(t *testing.T) {
	refill := 8 * time.Second
	// Empty bucket, last refilled 3 intervals ago → 3 tokens become available.
	c := newBucket(0, 8, refill, time.Now().Add(-3*refill-time.Second))
	for i := 0; i < 3; i++ {
		if !c.tryAcquire() {
			t.Fatalf("acquire %d should succeed after refill", i+1)
		}
	}
	if c.tryAcquire() {
		t.Error("4th acquire should fail — only 3 tokens were refilled")
	}
}

func TestTryAcquire_RefillClampsAtMax(t *testing.T) {
	refill := time.Second
	// Last refilled 1000 intervals ago — far more than maxTokens.
	c := newBucket(0, 8, refill, time.Now().Add(-1000*refill))
	got := 0
	for c.tryAcquire() {
		got++
		if got > 100 {
			t.Fatal("tryAcquire never stopped — refill not clamped at maxTokens")
		}
	}
	if got != 8 {
		t.Errorf("drained %d tokens, want 8 (clamped at maxTokens)", got)
	}
}

// TestTryAcquire_Concurrent hammers the bucket from many goroutines. With a
// pinned lastRefill and a long refillRate no time-based refill occurs, so the
// number of successful acquisitions must equal exactly the initial token
// count — never more (which would mean a lost-update race).
func TestTryAcquire_Concurrent(t *testing.T) {
	const initial = 500
	c := newBucket(initial, initial, time.Hour, time.Now())

	const goroutines = 64
	const attemptsPer = 50
	var success int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < attemptsPer; i++ {
				if c.tryAcquire() {
					atomic.AddInt64(&success, 1)
				}
			}
		}()
	}
	wg.Wait()

	if success != initial {
		t.Errorf("successful acquisitions = %d, want exactly %d", success, initial)
	}
}

// --- fetch / FetchSubreddit (local server only — never hits reddit.com) ---

func newTestPublicClient(t *testing.T, tokens int, handler http.Handler) *PublicClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	return &PublicClient{
		httpClient:  &fhttp.Client{Transport: &rewriteTransport{scheme: u.Scheme, host: u.Host}},
		userAgentFn: func() string { return "test-public-ua/1.0" },
		tokens:      tokens,
		maxTokens:   8,
		refillRate:  time.Hour,
		lastRefill:  time.Now(),
	}
}

func TestPublicFetch_LocalRateLimit(t *testing.T) {
	// tokens=0 → tryAcquire fails before any network call is made.
	c := newTestPublicClient(t, 0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be hit when locally rate limited")
	}))
	_, err := c.fetch(context.Background(), "/r/test/hot.json")
	if err == nil || !strings.Contains(err.Error(), "rate limited locally") {
		t.Fatalf("err = %v, want a local rate-limit error", err)
	}
}

func TestPublicFetch_429(t *testing.T) {
	c := newTestPublicClient(t, 8, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	_, err := c.fetch(context.Background(), "/r/test/hot.json")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want a 429 error", err)
	}
}

func TestPublicFetch_Non200(t *testing.T) {
	c := newTestPublicClient(t, 8, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	_, err := c.fetch(context.Background(), "/r/test/hot.json")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want a status 403 error", err)
	}
}

func TestPublicFetch_SetsHeaders(t *testing.T) {
	var ua, accept, cookie string
	c := newTestPublicClient(t, 8, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		accept = r.Header.Get("Accept")
		cookie = r.Header.Get("Cookie")
		w.Write([]byte(`{}`))
	}))
	if _, err := c.fetch(context.Background(), "/api/test.json"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ua != "test-public-ua/1.0" {
		t.Errorf("User-Agent = %q", ua)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q", accept)
	}
	if cookie != publicCookie {
		t.Errorf("Cookie = %q, want public opt-in cookie", cookie)
	}
}
