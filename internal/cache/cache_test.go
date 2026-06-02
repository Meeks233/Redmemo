package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/redmemo/redmemo/internal/config"
)

func newTestCache(t *testing.T) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := New(config.RedisConfig{Addr: mr.Addr()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestNew_PingFailure(t *testing.T) {
	// Use a bogus client with no retries / short timeouts to avoid hanging.
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1", // closed port
		MaxRetries:  -1,
		DialTimeout: 200 * time.Millisecond,
	})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := client.Ping(ctx).Err(); err == nil {
		t.Fatalf("expected ping to fail against closed port")
	}
}

func TestHTMLCacheRoundTrip(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	got, err := c.GetHTML(ctx, "/r/test")
	if err != nil || got != nil {
		t.Fatalf("expected nil cache miss, got %v err=%v", got, err)
	}

	if err := c.PutHTML(ctx, "/r/test", []byte("<html>hi</html>"), time.Minute); err != nil {
		t.Fatalf("PutHTML: %v", err)
	}
	got, err = c.GetHTML(ctx, "/r/test")
	if err != nil {
		t.Fatalf("GetHTML: %v", err)
	}
	if string(got) != "<html>hi</html>" {
		t.Fatalf("unexpected payload: %q", got)
	}

	if err := c.InvalidateHTML(ctx, "/r/test"); err != nil {
		t.Fatalf("InvalidateHTML: %v", err)
	}
	got, _ = c.GetHTML(ctx, "/r/test")
	if got != nil {
		t.Fatalf("expected nil after invalidate, got %q", got)
	}
}

func TestHTMLCache_TTLExpiry(t *testing.T) {
	c, mr := newTestCache(t)
	ctx := context.Background()
	if err := c.PutHTML(ctx, "/r/x", []byte("payload"), 30*time.Second); err != nil {
		t.Fatalf("PutHTML: %v", err)
	}
	mr.FastForward(31 * time.Second)
	got, _ := c.GetHTML(ctx, "/r/x")
	if got != nil {
		t.Fatalf("expected expired entry, got %q", got)
	}
}

func TestRateLimitStateRoundTrip(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	// Empty state when nothing stored.
	st, err := c.GetRateLimitState(ctx)
	if err != nil {
		t.Fatalf("GetRateLimitState empty: %v", err)
	}
	if st.Remaining != 0 || st.Used != 0 || !st.ResetAt.IsZero() {
		t.Fatalf("expected zero state, got %+v", st)
	}

	in := &RateLimitState{Remaining: 42, Used: 8, ResetAt: time.Unix(1_700_000_000, 0)}
	if err := c.SetRateLimitState(ctx, in); err != nil {
		t.Fatalf("SetRateLimitState: %v", err)
	}
	out, err := c.GetRateLimitState(ctx)
	if err != nil {
		t.Fatalf("GetRateLimitState: %v", err)
	}
	if out.Remaining != 42 || out.Used != 8 || !out.ResetAt.Equal(in.ResetAt) {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestOAuthRuntimeRoundTrip(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	tok, rolling, err := c.GetOAuthRuntime(ctx)
	if err != nil {
		t.Fatalf("GetOAuthRuntime empty: %v", err)
	}
	if tok != 0 || rolling {
		t.Fatalf("expected zero defaults, got tok=%d rolling=%v", tok, rolling)
	}

	if err := c.SetOAuthRuntime(ctx, 7, true); err != nil {
		t.Fatalf("SetOAuthRuntime: %v", err)
	}
	tok, rolling, err = c.GetOAuthRuntime(ctx)
	if err != nil || tok != 7 || !rolling {
		t.Fatalf("round-trip mismatch: tok=%d rolling=%v err=%v", tok, rolling, err)
	}

	if err := c.SetOAuthRuntime(ctx, 9, false); err != nil {
		t.Fatalf("SetOAuthRuntime false: %v", err)
	}
	tok, rolling, _ = c.GetOAuthRuntime(ctx)
	if tok != 9 || rolling {
		t.Fatalf("expected tok=9 rolling=false, got tok=%d rolling=%v", tok, rolling)
	}
}

func TestMediaAccessBatch(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	// Empty flush returns nil without error.
	out, err := c.FlushMediaAccess(ctx)
	if err != nil || out != nil {
		t.Fatalf("expected empty flush, got %v err=%v", out, err)
	}

	if err := c.RecordMediaAccess(ctx, "https://i.example.com/a.jpg"); err != nil {
		t.Fatalf("RecordMediaAccess a: %v", err)
	}
	if err := c.RecordMediaAccess(ctx, "https://i.example.com/b.jpg"); err != nil {
		t.Fatalf("RecordMediaAccess b: %v", err)
	}

	out, err = c.FlushMediaAccess(ctx)
	if err != nil {
		t.Fatalf("FlushMediaAccess: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d (%v)", len(out), out)
	}
	if _, ok := out["https://i.example.com/a.jpg"]; !ok {
		t.Fatalf("missing entry a")
	}
	if _, ok := out["https://i.example.com/b.jpg"]; !ok {
		t.Fatalf("missing entry b")
	}

	// Flush is destructive — subsequent flush returns empty.
	out, err = c.FlushMediaAccess(ctx)
	if err != nil || out != nil {
		t.Fatalf("expected empty flush after drain, got %v err=%v", out, err)
	}
}

func TestGenericKV(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()

	v, err := c.Get(ctx, "missing")
	if err != nil || v != "" {
		t.Fatalf("expected empty miss, got %q err=%v", v, err)
	}

	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err = c.Get(ctx, "k")
	if err != nil || v != "v" {
		t.Fatalf("expected v, got %q err=%v", v, err)
	}
}

func TestPingAndClient(t *testing.T) {
	c, _ := newTestCache(t)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if c.Client() == nil {
		t.Fatalf("Client() returned nil")
	}
}

func TestRateLimitStateBinary(t *testing.T) {
	in := &RateLimitState{Remaining: 1, Used: 2, ResetAt: time.Unix(123, 0)}
	data, err := in.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	out := &RateLimitState{}
	if err := out.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if out.Remaining != 1 || out.Used != 2 || !out.ResetAt.Equal(in.ResetAt) {
		t.Fatalf("binary round-trip mismatch: %+v", out)
	}
}
