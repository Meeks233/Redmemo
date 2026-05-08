package ratelimit

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/config"
)

// BudgetSource provides OAuth token budget information.
// Implemented by oauth.Pool in batch 2.
type BudgetSource interface {
	RemainingBudget(ctx context.Context) (int, error)
}

type Manager struct {
	mu sync.RWMutex

	remaining    int
	used         int
	windowSize   int
	windowDur    time.Duration
	safetyBuffer int
	resetAt      time.Time
	exhausted    bool

	budget BudgetSource
	stopCh chan struct{}
}

type StatusSnapshot struct {
	Remaining    int       `json:"remaining"`
	Used         int       `json:"used"`
	WindowSize   int       `json:"window_size"`
	ResetAt      time.Time `json:"reset_at"`
	Exhausted    bool      `json:"exhausted"`
	SafetyBuffer int       `json:"safety_buffer"`
}

func New(cfg config.RateLimitConfig, budget BudgetSource) *Manager {
	return &Manager{
		remaining:    cfg.WindowSize,
		windowSize:   cfg.WindowSize,
		windowDur:    cfg.WindowDuration,
		safetyBuffer: cfg.SafetyBuffer,
		resetAt:      time.Now().Add(cfg.WindowDuration),
		budget:       budget,
		stopCh:       make(chan struct{}),
	}
}

// CanRequestRedlib reports whether a request can be forwarded to redlib.
func (m *Manager) CanRequestRedlib() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.exhausted && m.remaining > m.safetyBuffer
}

// CanRequestFallback reports whether a fallback request using own OAuth tokens
// is possible. It checks Redis for any token with remaining quota.
func (m *Manager) CanRequestFallback(ctx context.Context) bool {
	if m.budget == nil {
		return false
	}
	budget, err := m.budget.RemainingBudget(ctx)
	if err != nil {
		return false
	}
	return budget > 0
}

// CanPrefetch reports whether prefetching is allowed and the available budget.
// Prefetch uses own OAuth tokens (not redlib quota). Additionally, when the
// redlib window is about to reset (< 2 min remaining), the safety buffer is
// released for prefetch use via redlib.
func (m *Manager) CanPrefetch(ctx context.Context) (bool, int) {
	var oauthBudget int
	if m.budget != nil {
		b, err := m.budget.RemainingBudget(ctx)
		if err == nil {
			oauthBudget = b
		}
	}

	m.mu.RLock()
	redlibBudget := 0
	if !m.exhausted && time.Until(m.resetAt) < 2*time.Minute && m.remaining > 0 {
		redlibBudget = m.remaining
	}
	m.mu.RUnlock()

	total := oauthBudget + redlibBudget
	return total > 0, total
}

// Status returns a snapshot of the current rate limit state.
func (m *Manager) Status() StatusSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return StatusSnapshot{
		Remaining:    m.remaining,
		Used:         m.used,
		WindowSize:   m.windowSize,
		ResetAt:      m.resetAt,
		Exhausted:    m.exhausted,
		SafetyBuffer: m.safetyBuffer,
	}
}

// Increment records one request forwarded to redlib.
func (m *Manager) Increment() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remaining--
	m.used++
}

// OnRedlibRateLimited is called when redlib returns a rate-limited response.
func (m *Manager) OnRedlibRateLimited() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remaining = 0
	m.exhausted = true
}

// OnRequestComplete parses Reddit's X-Ratelimit-* headers from a direct API
// response (own OAuth token) and updates state accordingly. This is only useful
// when RedMemo makes its own requests (fallback/prefetch), not for redlib proxy.
func (m *Manager) OnRequestComplete(headers http.Header) {
	remainStr := headers.Get("X-Ratelimit-Remaining")
	resetStr := headers.Get("X-Ratelimit-Reset")
	usedStr := headers.Get("X-Ratelimit-Used")

	if remainStr == "" && resetStr == "" && usedStr == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if remainStr != "" {
		remainStr = strings.TrimSpace(remainStr)
		if f, err := strconv.ParseFloat(remainStr, 64); err == nil {
			m.remaining = int(f)
		}
	}

	if resetStr != "" {
		resetStr = strings.TrimSpace(resetStr)
		if secs, err := strconv.ParseFloat(resetStr, 64); err == nil {
			m.resetAt = time.Now().Add(time.Duration(secs) * time.Second)
		}
	}

	if usedStr != "" {
		usedStr = strings.TrimSpace(usedStr)
		if u, err := strconv.Atoi(usedStr); err == nil {
			m.used = u
		}
	}
}

// ResetWindow resets the rate limit window to full quota.
func (m *Manager) ResetWindow() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remaining = m.windowSize
	m.used = 0
	m.exhausted = false
	m.resetAt = time.Now().Add(m.windowDur)
}

// Start launches the background window-reset timer. It blocks until ctx is
// cancelled or Stop is called.
func (m *Manager) Start(ctx context.Context) {
	go m.run(ctx)
}

func (m *Manager) run(ctx context.Context) {
	for {
		m.mu.RLock()
		waitDur := time.Until(m.resetAt)
		m.mu.RUnlock()

		if waitDur <= 0 {
			m.ResetWindow()
			log.Printf("ratelimit: window reset, remaining=%d", m.windowSize)
			continue
		}

		timer := time.NewTimer(waitDur)
		select {
		case <-timer.C:
			m.ResetWindow()
			log.Printf("ratelimit: window reset, remaining=%d", m.windowSize)
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.stopCh:
			timer.Stop()
			return
		}
	}
}

// Stop signals the background goroutine to exit.
func (m *Manager) Stop() {
	select {
	case m.stopCh <- struct{}{}:
	default:
	}
}
