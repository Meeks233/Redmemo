package oauth

import (
	"context"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/store"
)

func newTestHolder(active *ManagedToken) *TokenHolder {
	return &TokenHolder{active: active}
}

func TestToken_Available(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 50,
		RateResetAt:   future,
	}
	p := newTestHolder(mt)
	best := p.Token()
	if best == nil {
		t.Fatal("expected non-nil token")
	}
	if best.StoredToken.ID != 1 {
		t.Errorf("got token ID %d, want 1", best.StoredToken.ID)
	}
}

func TestToken_Exhausted(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 0,
		RateResetAt:   future,
	}
	p := newTestHolder(mt)
	if got := p.Token(); got != nil {
		t.Errorf("expected nil, got token ID %d", got.StoredToken.ID)
	}
}

func TestToken_Empty(t *testing.T) {
	p := newTestHolder(nil)
	if got := p.Token(); got != nil {
		t.Errorf("expected nil when no token held, got %+v", got)
	}
}

func TestToken_ResetsAfterWindow(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute)
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 0,
		RateResetAt:   past,
	}
	p := newTestHolder(mt)
	best := p.Token()
	if best == nil {
		t.Fatal("expected non-nil token after window reset")
	}
	if best.RateRemaining != 99 {
		t.Errorf("RateRemaining = %d, want 99 after reset", best.RateRemaining)
	}
}

func TestOnRequestComplete_ParsesHeaders(t *testing.T) {
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 99,
	}
	p := newTestHolder(mt)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Remaining", "554.0")
	resp.Header.Set("X-Ratelimit-Reset", "300")

	p.OnRequestComplete(1, resp)

	if mt.RateRemaining != 554 {
		t.Errorf("RateRemaining = %d, want 554", mt.RateRemaining)
	}
	if time.Until(mt.RateResetAt) < 290*time.Second {
		t.Error("RateResetAt not updated correctly")
	}
}

func TestOnRequestComplete_FloatRemaining(t *testing.T) {
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 99,
	}
	p := newTestHolder(mt)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Remaining", "95.5")

	p.OnRequestComplete(1, resp)

	if mt.RateRemaining != 95 {
		t.Errorf("RateRemaining = %d, want 95 (truncated from 95.5)", mt.RateRemaining)
	}
}

func TestOnRequestComplete_UnknownTokenID(t *testing.T) {
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 99,
	}
	p := newTestHolder(mt)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Remaining", "50.0")

	p.OnRequestComplete(999, resp)

	if mt.RateRemaining != 99 {
		t.Errorf("token should be unchanged, got RateRemaining = %d", mt.RateRemaining)
	}
}

func TestOnRequestComplete_NoHeaders(t *testing.T) {
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 99,
	}
	p := newTestHolder(mt)

	resp := &http.Response{Header: http.Header{}}
	p.OnRequestComplete(1, resp)

	if mt.RateRemaining != 99 {
		t.Errorf("should be unchanged without headers, got %d", mt.RateRemaining)
	}
}

func TestRemainingBudget_SingleToken(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	mt := &ManagedToken{RateRemaining: 50, RateResetAt: future}
	p := newTestHolder(mt)

	budget, err := p.RemainingBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if budget != 50 {
		t.Errorf("budget = %d, want 50", budget)
	}
}

func TestRemainingBudget_Empty(t *testing.T) {
	p := newTestHolder(nil)
	budget, err := p.RemainingBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if budget != 0 {
		t.Errorf("budget = %d, want 0", budget)
	}
}

func TestRemainingBudget_ResetsAfterWindow(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute)
	mt := &ManagedToken{RateRemaining: 0, RateResetAt: past}
	p := newTestHolder(mt)

	budget, err := p.RemainingBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if budget != 99 {
		t.Errorf("budget = %d, want 99 after window reset", budget)
	}
}

// Regression: the nav ring (RemainingBudget / WindowInfo) and the debug page
// (TokenStatuses) must report the SAME remaining count. The old code left
// TokenStatuses showing the raw pre-reset header value while RemainingBudget
// optimistically replenished once the window elapsed — producing the "debug
// says 92 but the ring is full" mismatch. After a window elapse both must show
// the replenished ceiling; within an active window both must show the live count.
func TestBudgetSurfacesAgree(t *testing.T) {
	t.Run("window elapsed -> both replenished", func(t *testing.T) {
		past := time.Now().Add(-1 * time.Minute)
		mt := &ManagedToken{RateRemaining: 92, RateResetAt: past}
		p := newTestHolder(mt)

		budget, _ := p.RemainingBudget(context.Background())
		_, capacity, winRem := p.WindowInfo()
		statuses := p.TokenStatuses()
		if len(statuses) != 1 {
			t.Fatalf("TokenStatuses len = %d, want 1", len(statuses))
		}
		debugRem := statuses[0].RateRemaining

		if budget != windowCapacity {
			t.Errorf("RemainingBudget = %d, want %d after reset", budget, windowCapacity)
		}
		if winRem != windowCapacity {
			t.Errorf("WindowInfo remaining = %d, want %d after reset", winRem, windowCapacity)
		}
		if capacity != windowCapacity {
			t.Errorf("WindowInfo capacity = %d, want %d", capacity, windowCapacity)
		}
		if debugRem != budget || debugRem != winRem {
			t.Errorf("debug=%d ring-budget=%d window=%d; all three must agree", debugRem, budget, winRem)
		}
	})

	t.Run("active window -> both live count", func(t *testing.T) {
		future := time.Now().Add(5 * time.Minute)
		mt := &ManagedToken{RateRemaining: 92, RateResetAt: future}
		p := newTestHolder(mt)

		budget, _ := p.RemainingBudget(context.Background())
		_, _, winRem := p.WindowInfo()
		debugRem := p.TokenStatuses()[0].RateRemaining

		if budget != 92 || winRem != 92 || debugRem != 92 {
			t.Errorf("within active window all must read 92: budget=%d window=%d debug=%d", budget, winRem, debugRem)
		}
	})
}

func TestHasAvailableTokens(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)

	p := newTestHolder(nil)
	if p.HasAvailableTokens() {
		t.Error("expected false for nil active")
	}

	p = newTestHolder(&ManagedToken{RateRemaining: 10, RateResetAt: future})
	if !p.HasAvailableTokens() {
		t.Error("expected true with remaining > 0")
	}

	past := time.Now().Add(-1 * time.Minute)
	p = newTestHolder(&ManagedToken{RateRemaining: 0, RateResetAt: past})
	if !p.HasAvailableTokens() {
		t.Error("expected true after window reset")
	}

	p = newTestHolder(&ManagedToken{RateRemaining: 0, RateResetAt: future})
	if p.HasAvailableTokens() {
		t.Error("expected false with 0 remaining and future reset")
	}
}
