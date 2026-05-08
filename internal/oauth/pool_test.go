package oauth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

func newTestPool(tokens []*ManagedToken) *Pool {
	return &Pool{tokens: tokens}
}

func TestGetBestToken_HighestRemaining(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 50},
		{StoredToken: store.StoredToken{ID: 2}, RateRemaining: 200},
		{StoredToken: store.StoredToken{ID: 3}, RateRemaining: 100},
	}
	p := newTestPool(tokens)
	best := p.GetBestToken()
	if best == nil {
		t.Fatal("expected non-nil token")
	}
	if best.StoredToken.ID != 2 {
		t.Errorf("got token ID %d, want 2", best.StoredToken.ID)
	}
}

func TestGetBestToken_AllExhausted(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 0},
		{StoredToken: store.StoredToken{ID: 2}, RateRemaining: -5},
	}
	p := newTestPool(tokens)
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

func TestGetBestToken_SingleToken(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 7}, RateRemaining: 42},
	}
	p := newTestPool(tokens)
	best := p.GetBestToken()
	if best == nil || best.StoredToken.ID != 7 {
		t.Errorf("expected token 7, got %+v", best)
	}
}

func TestOnRequestComplete_ParsesHeaders(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 99},
	}
	p := newTestPool(tokens)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Remaining", "554.0")
	resp.Header.Set("X-Ratelimit-Reset", "300")

	p.OnRequestComplete(1, resp)

	if tokens[0].RateRemaining != 554 {
		t.Errorf("RateRemaining = %d, want 554", tokens[0].RateRemaining)
	}
	if time.Until(tokens[0].RateResetAt) < 290*time.Second {
		t.Error("RateResetAt not updated correctly")
	}
}

func TestOnRequestComplete_FloatRemaining(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 99},
	}
	p := newTestPool(tokens)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Remaining", "95.5")

	p.OnRequestComplete(1, resp)

	if tokens[0].RateRemaining != 95 {
		t.Errorf("RateRemaining = %d, want 95 (truncated from 95.5)", tokens[0].RateRemaining)
	}
}

func TestOnRequestComplete_UnknownTokenID(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 99},
	}
	p := newTestPool(tokens)

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Remaining", "50.0")

	p.OnRequestComplete(999, resp)

	if tokens[0].RateRemaining != 99 {
		t.Errorf("token should be unchanged, got RateRemaining = %d", tokens[0].RateRemaining)
	}
}

func TestOnRequestComplete_NoHeaders(t *testing.T) {
	tokens := []*ManagedToken{
		{StoredToken: store.StoredToken{ID: 1}, RateRemaining: 99},
	}
	p := newTestPool(tokens)

	resp := &http.Response{Header: http.Header{}}
	p.OnRequestComplete(1, resp)

	if tokens[0].RateRemaining != 99 {
		t.Errorf("should be unchanged without headers, got %d", tokens[0].RateRemaining)
	}
}

func TestRemainingBudget_SumsAll(t *testing.T) {
	tokens := []*ManagedToken{
		{RateRemaining: 50},
		{RateRemaining: 30},
		{RateRemaining: 20},
	}
	p := newTestPool(tokens)

	budget, err := p.RemainingBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if budget != 100 {
		t.Errorf("budget = %d, want 100", budget)
	}
}

func TestRemainingBudget_SkipsNegative(t *testing.T) {
	tokens := []*ManagedToken{
		{RateRemaining: 50},
		{RateRemaining: -10},
		{RateRemaining: 0},
	}
	p := newTestPool(tokens)

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
