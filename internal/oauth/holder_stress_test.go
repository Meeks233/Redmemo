package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/store"
)

// --- helpers ---

// authRewriteTransport routes the auth client's www.reddit.com requests to a
// local httptest server. No request ever reaches Reddit.
type authRewriteTransport struct {
	scheme string
	host   string
}

func (t *authRewriteTransport) RoundTrip(req *fhttp.Request) (*fhttp.Response, error) {
	req.URL.Scheme = t.scheme
	req.URL.Host = t.host
	return fhttp.DefaultTransport.RoundTrip(req)
}

// newClientToServer builds an oauth.Client whose HTTP traffic is pinned to srv.
func newClientToServer(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	return &Client{
		httpClient: &fhttp.Client{
			Timeout:   2 * time.Second,
			Transport: &authRewriteTransport{scheme: u.Scheme, host: u.Host},
		},
	}
}

// rlResp builds a bare response carrying only rate-limit headers.
func rlResp(remaining, reset string) *fhttp.Response {
	h := fhttp.Header{}
	if remaining != "" {
		h.Set("X-Ratelimit-Remaining", remaining)
	}
	if reset != "" {
		h.Set("X-Ratelimit-Reset", reset)
	}
	return &fhttp.Response{Header: h}
}

// --- concurrency / stress ---

// TestHolder_ConcurrentAccess drives every locked TokenHolder accessor from many
// goroutines at once. Run with -race to catch data races between the RWMutex
// readers and the Token/OnRequestComplete writers. Quota is kept high
// so the low-quota refresh path (which would spawn background goroutines) is
// never triggered — this isolates pure lock contention.
func TestHolder_ConcurrentAccess(t *testing.T) {
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 500,
		RateResetAt:   time.Now().Add(10 * time.Minute),
	}
	p := newTestHolder(mt)

	const goroutines = 64
	const iters = 400
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				switch (g + i) % 7 {
				case 0:
					p.Token()
				case 1:
					// remaining stays well above the low-quota threshold
					p.OnRequestComplete(1, rlResp("500.0", "600"))
				case 2:
					p.HasAvailableTokens()
				case 3:
					if _, err := p.RemainingBudget(ctx); err != nil {
						t.Errorf("RemainingBudget: %v", err)
					}
				case 4:
					p.WindowInfo()
				case 5:
					p.EarliestReset()
				case 6:
					p.TokenStatuses()
				}
			}
		}(g)
	}
	wg.Wait()

	if got := p.Token(); got == nil {
		t.Fatal("token should still be available after concurrent access")
	}
}

// --- WindowInfo boundaries ---

func TestWindowInfo_Boundaries(t *testing.T) {
	t.Run("future reset keeps remaining", func(t *testing.T) {
		reset := time.Now().Add(5 * time.Minute)
		p := newTestHolder(&ManagedToken{RateRemaining: 30, RateResetAt: reset})
		got, capacity, rem := p.WindowInfo()
		if capacity != 99 {
			t.Errorf("capacity = %d, want 99", capacity)
		}
		if rem != 30 {
			t.Errorf("remaining = %d, want 30", rem)
		}
		if !got.Equal(reset) {
			t.Errorf("resetAt = %v, want %v", got, reset)
		}
	})

	t.Run("past reset rolls window forward", func(t *testing.T) {
		// 25 min in the past spans more than two 10-min windows.
		p := newTestHolder(&ManagedToken{RateRemaining: 0, RateResetAt: time.Now().Add(-25 * time.Minute)})
		got, _, rem := p.WindowInfo()
		if rem != 99 {
			t.Errorf("remaining = %d, want 99 after rollover", rem)
		}
		if !got.After(time.Now()) {
			t.Errorf("rolled resetAt %v is not in the future", got)
		}
	})

	t.Run("exhausted with future reset reports zero", func(t *testing.T) {
		p := newTestHolder(&ManagedToken{RateRemaining: 0, RateResetAt: time.Now().Add(5 * time.Minute)})
		_, _, rem := p.WindowInfo()
		if rem != 0 {
			t.Errorf("remaining = %d, want 0", rem)
		}
	})

	t.Run("nil active", func(t *testing.T) {
		p := newTestHolder(nil)
		got, capacity, rem := p.WindowInfo()
		if !got.IsZero() || capacity != 0 || rem != 0 {
			t.Errorf("nil active: got resetAt=%v capacity=%d remaining=%d", got, capacity, rem)
		}
	})
}

// --- EarliestReset boundaries ---

func TestEarliestReset_Boundaries(t *testing.T) {
	t.Run("future reset", func(t *testing.T) {
		p := newTestHolder(&ManagedToken{RateResetAt: time.Now().Add(3 * time.Minute)})
		secs, windowSec := p.EarliestReset()
		if windowSec != 600 {
			t.Errorf("windowSec = %d, want 600", windowSec)
		}
		if secs < 150 || secs > 190 {
			t.Errorf("secs = %d, want ~180", secs)
		}
	})

	t.Run("past reset rolls forward into a future window", func(t *testing.T) {
		p := newTestHolder(&ManagedToken{RateResetAt: time.Now().Add(-90 * time.Minute)})
		secs, _ := p.EarliestReset()
		if secs < 0 || secs > 600 {
			t.Errorf("secs = %d, want within (0,600]", secs)
		}
	})

	t.Run("nil active", func(t *testing.T) {
		p := newTestHolder(nil)
		secs, windowSec := p.EarliestReset()
		if secs != 0 || windowSec != 600 {
			t.Errorf("nil active: secs=%d windowSec=%d", secs, windowSec)
		}
	})
}

// --- OnRequestComplete header parsing boundaries ---

func TestOnRequestComplete_MalformedHeaders(t *testing.T) {
	t.Run("non-numeric remaining is ignored", func(t *testing.T) {
		mt := &ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 88}
		p := newTestHolder(mt)
		p.OnRequestComplete(1, rlResp("not-a-number", ""))
		if mt.RateRemaining != 88 {
			t.Errorf("RateRemaining = %d, want 88 (unchanged)", mt.RateRemaining)
		}
	})

	t.Run("whitespace around values is trimmed", func(t *testing.T) {
		mt := &ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 88}
		p := newTestHolder(mt)
		p.OnRequestComplete(1, rlResp("  42.0  ", "  300  "))
		if mt.RateRemaining != 42 {
			t.Errorf("RateRemaining = %d, want 42", mt.RateRemaining)
		}
		if d := time.Until(mt.RateResetAt); d < 290*time.Second || d > 310*time.Second {
			t.Errorf("RateResetAt delta = %v, want ~300s", d)
		}
	})

	t.Run("non-numeric reset leaves resetAt unchanged", func(t *testing.T) {
		orig := time.Now().Add(7 * time.Minute)
		mt := &ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50, RateResetAt: orig}
		p := newTestHolder(mt)
		p.OnRequestComplete(1, rlResp("40.0", "garbage"))
		if !mt.RateResetAt.Equal(orig) {
			t.Error("RateResetAt changed despite an unparseable reset header")
		}
	})
}

// --- forceRefresh behaviour (all auth traffic goes to a local server) ---

func TestForceRefresh_FailureIncrementsConsecutive(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestHolder(&ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50})
	p.client = newClientToServer(t, srv)
	p.backend = "mobile_spoof"

	p.forceRefresh("test")

	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("auth endpoint was never reached")
	}
	if p.consecutiveFail != 1 {
		t.Errorf("consecutiveFail = %d, want 1 after one failed refresh", p.consecutiveFail)
	}
}

// The generic_web auto-switch is removed: repeated mobile_spoof failures keep
// the backend on mobile_spoof and just accumulate the consecutive-fail count.
func TestForceRefresh_DoesNotSwitchBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestHolder(&ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50})
	p.client = newClientToServer(t, srv)
	p.backend = "mobile_spoof"
	p.consecutiveFail = maxConsecutiveFails - 1

	p.forceRefresh("test")

	if p.backend != "mobile_spoof" {
		t.Errorf("backend = %q, want mobile_spoof (generic_web auto-switch removed)", p.backend)
	}
	if p.consecutiveFail != maxConsecutiveFails {
		t.Errorf("consecutiveFail = %d, want %d", p.consecutiveFail, maxConsecutiveFails)
	}
}

// effectiveCooldown stays flat below the failure threshold, then doubles per
// extra failure, capped at maxRefreshCooldown.
func TestEffectiveCooldown_Escalates(t *testing.T) {
	if got := effectiveCooldown(0); got != refreshCooldown {
		t.Errorf("fails=0: cooldown = %v, want %v", got, refreshCooldown)
	}
	if got := effectiveCooldown(maxConsecutiveFails - 1); got != refreshCooldown {
		t.Errorf("just below threshold: cooldown = %v, want %v", got, refreshCooldown)
	}
	// At the threshold the cooldown is the first doubling (2x).
	if got := effectiveCooldown(maxConsecutiveFails); got != refreshCooldown*2 {
		t.Errorf("at threshold: cooldown = %v, want %v", got, refreshCooldown*2)
	}
	// One past the threshold quadruples.
	if got := effectiveCooldown(maxConsecutiveFails + 1); got != refreshCooldown*4 {
		t.Errorf("threshold+1: cooldown = %v, want %v", got, refreshCooldown*4)
	}
	// Backoff is bounded.
	if got := effectiveCooldown(maxConsecutiveFails + 100); got != maxRefreshCooldown {
		t.Errorf("far past threshold: cooldown = %v, want cap %v", got, maxRefreshCooldown)
	}
	if got := effectiveCooldown(maxConsecutiveFails + 1000000); got != maxRefreshCooldown {
		t.Errorf("overflow guard: cooldown = %v, want cap %v", got, maxRefreshCooldown)
	}
}

// Once the failure threshold is crossed, the escalated cooldown blocks a
// refresh whose elapsed time would have cleared the flat refreshCooldown.
func TestForceRefresh_BackoffBlocksWithinEscalatedCooldown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestHolder(&ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50})
	p.client = newClientToServer(t, srv)
	p.backend = "mobile_spoof"
	// Past the threshold: effectiveCooldown is at least refreshCooldown*2.
	p.consecutiveFail = maxConsecutiveFails
	// Last refresh was longer ago than the flat cooldown but well inside the
	// escalated one, so the attempt must still be suppressed.
	p.lastRefreshAt = time.Now().Add(-(refreshCooldown + time.Second))

	p.forceRefresh("test")

	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Errorf("auth endpoint hit %d times despite escalated backoff cooldown", h)
	}
}

func TestForceRefresh_RespectsCooldown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestHolder(&ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50})
	p.client = newClientToServer(t, srv)
	p.backend = "mobile_spoof"
	p.lastRefreshAt = time.Now() // still inside the cooldown window

	p.forceRefresh("test")

	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Errorf("auth endpoint hit %d times despite an active cooldown", h)
	}
}

// TestOnRequestComplete_LowQuotaTriggersRefresh verifies that a critically low
// remaining count (below the threshold of 2) kicks off an async refresh.
func TestOnRequestComplete_LowQuotaTriggersRefresh(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newTestHolder(&ManagedToken{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50})
	p.client = newClientToServer(t, srv)
	p.backend = "mobile_spoof"

	p.OnRequestComplete(1, rlResp("1.0", "600")) // remaining 1 < threshold 2

	select {
	case <-hit:
		// a refresh attempt was made — expected
	case <-time.After(3 * time.Second):
		t.Fatal("low quota did not trigger a background refresh")
	}
}
