package ratelimit

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/config"
)

type mockBudget struct{ budget int }

func (m *mockBudget) RemainingBudget(_ context.Context) (int, error) { return m.budget, nil }

func newTestManager(windowSize, safety int) *Manager {
	return New(config.RateLimitConfig{
		WindowSize:     windowSize,
		WindowDuration: 10 * time.Minute,
		SafetyBuffer:   safety,
	}, nil)
}

func TestOnRequestComplete_ParsesHeaders(t *testing.T) {
	m := newTestManager(600, 50)
	h := http.Header{}
	h.Set("X-Ratelimit-Remaining", "95.0")
	h.Set("X-Ratelimit-Reset", "342")
	h.Set("X-Ratelimit-Used", "5")
	m.OnRequestComplete(h)
	s := m.Status()
	if s.Remaining != 95 {
		t.Errorf("remaining = %d, want 95", s.Remaining)
	}
	if s.Used != 5 {
		t.Errorf("used = %d, want 5", s.Used)
	}
	// resetAt should be ~342s from now
	until := time.Until(s.ResetAt)
	if until < 340*time.Second || until > 344*time.Second {
		t.Errorf("resetAt should be ~342s from now, got %v", until)
	}
}

func TestOnRequestComplete_FloatRemaining(t *testing.T) {
	m := newTestManager(600, 50)
	h := http.Header{}
	h.Set("X-Ratelimit-Remaining", "554.0")
	m.OnRequestComplete(h)
	if m.Status().Remaining != 554 {
		t.Errorf("remaining = %d, want 554", m.Status().Remaining)
	}
}

func TestOnRequestComplete_EmptyHeaders(t *testing.T) {
	m := newTestManager(600, 50)
	initialRemaining := m.Status().Remaining
	m.OnRequestComplete(http.Header{})
	if m.Status().Remaining != initialRemaining {
		t.Error("empty headers should not change state")
	}
}

func TestResetWindow(t *testing.T) {
	m := newTestManager(600, 50)
	m.ResetWindow()
	s := m.Status()
	if s.Remaining != 600 {
		t.Errorf("remaining after reset = %d, want 600", s.Remaining)
	}
	if s.Used != 0 {
		t.Errorf("used after reset = %d, want 0", s.Used)
	}
	if s.Exhausted {
		t.Error("should not be exhausted after reset")
	}
}

func TestCanRequest_NilBudget(t *testing.T) {
	m := newTestManager(600, 50)
	if m.CanRequest(context.Background()) {
		t.Error("should return false with nil budget source")
	}
}

func TestCanRequest_WithBudget(t *testing.T) {
	m := New(config.RateLimitConfig{
		WindowSize:     600,
		WindowDuration: 10 * time.Minute,
		SafetyBuffer:   50,
	}, &mockBudget{budget: 100})
	if !m.CanRequest(context.Background()) {
		t.Error("should allow fallback with budget > 0")
	}
}

func TestCanRequest_ZeroBudget(t *testing.T) {
	m := New(config.RateLimitConfig{
		WindowSize:     600,
		WindowDuration: 10 * time.Minute,
		SafetyBuffer:   50,
	}, &mockBudget{budget: 0})
	if m.CanRequest(context.Background()) {
		t.Error("should not allow fallback with zero budget")
	}
}

func TestCanPrefetch_NoBudgetNoWindow(t *testing.T) {
	m := newTestManager(600, 50)
	ok, budget := m.CanPrefetch(context.Background())
	if ok || budget != 0 {
		t.Errorf("no budget source and window not near reset: ok=%v budget=%d", ok, budget)
	}
}
