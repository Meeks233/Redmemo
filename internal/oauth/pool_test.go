package oauth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

func newTestPool(active *ManagedToken) *Pool {
	return &Pool{active: active}
}

func TestGetBestToken_Available(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 50,
		RateResetAt:   future,
	}
	p := newTestPool(mt)
	best := p.GetBestToken()
	if best == nil {
		t.Fatal("expected non-nil token")
	}
	if best.StoredToken.ID != 1 {
		t.Errorf("got token ID %d, want 1", best.StoredToken.ID)
	}
}

func TestGetBestToken_Exhausted(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 0,
		RateResetAt:   future,
	}
	p := newTestPool(mt)
	if got := p.GetBestToken(); got != nil {
		t.Errorf("expected nil, got token ID %d", got.StoredToken.ID)
	}
}

func TestGetBestToken_Empty(t *testing.T) {
	p := newTestPool(nil)
	if got := p.GetBestToken(); got != nil {
		t.Errorf("expected nil for empty pool, got %+v", got)
	}
}

func TestGetBestToken_ResetsAfterWindow(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute)
	mt := &ManagedToken{
		StoredToken:   store.StoredToken{ID: 1},
		RateRemaining: 0,
		RateResetAt:   past,
	}
	p := newTestPool(mt)
	best := p.GetBestToken()
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
	p := newTestPool(mt)

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
	p := newTestPool(mt)

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
	p := newTestPool(mt)

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
	p := newTestPool(mt)

	resp := &http.Response{Header: http.Header{}}
	p.OnRequestComplete(1, resp)

	if mt.RateRemaining != 99 {
		t.Errorf("should be unchanged without headers, got %d", mt.RateRemaining)
	}
}

func TestRemainingBudget_SingleToken(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	mt := &ManagedToken{RateRemaining: 50, RateResetAt: future}
	p := newTestPool(mt)

	budget, err := p.RemainingBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if budget != 50 {
		t.Errorf("budget = %d, want 50", budget)
	}
}

func TestRemainingBudget_Empty(t *testing.T) {
	p := newTestPool(nil)
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
	p := newTestPool(mt)

	budget, err := p.RemainingBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if budget != 99 {
		t.Errorf("budget = %d, want 99 after window reset", budget)
	}
}

func TestHasAvailableTokens(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)

	p := newTestPool(nil)
	if p.HasAvailableTokens() {
		t.Error("expected false for nil active")
	}

	p = newTestPool(&ManagedToken{RateRemaining: 10, RateResetAt: future})
	if !p.HasAvailableTokens() {
		t.Error("expected true with remaining > 0")
	}

	past := time.Now().Add(-1 * time.Minute)
	p = newTestPool(&ManagedToken{RateRemaining: 0, RateResetAt: past})
	if !p.HasAvailableTokens() {
		t.Error("expected true after window reset")
	}

	p = newTestPool(&ManagedToken{RateRemaining: 0, RateResetAt: future})
	if p.HasAvailableTokens() {
		t.Error("expected false with 0 remaining and future reset")
	}
}
