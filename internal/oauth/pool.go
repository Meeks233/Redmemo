package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/useragent"
)

type ManagedToken struct {
	StoredToken   store.StoredToken
	Identity      SpoofIdentity
	RateRemaining int
	RateResetAt   time.Time
}

type Pool struct {
	mu     sync.RWMutex
	active *ManagedToken
	client *Client
	store  *store.TokenStore
	cache  *cache.Cache
	cfg    config.OAuthConfig
	uaPool *useragent.Pool
	cancel context.CancelFunc
	wg     sync.WaitGroup

	refreshMu       sync.Mutex
	consecutiveFail int
	lastRefreshAt   time.Time
	backend         string // "mobile_spoof" or "generic_web"
}

const (
	refreshCooldown     = 10 * time.Second
	maxConsecutiveFails = 5
)

func NewPool(cfg config.OAuthConfig, client *Client, tokenStore *store.TokenStore, c *cache.Cache, uaPool *useragent.Pool) *Pool {
	return &Pool{
		client:  client,
		store:   tokenStore,
		cache:   c,
		cfg:     cfg,
		uaPool:  uaPool,
		backend: "mobile_spoof",
	}
}

func (p *Pool) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)

	// Clean up old dynamic tokens from previous architecture.
	if n, err := p.store.DeleteExpiredByBackend("dynamic_spoof"); err != nil {
		log.Printf("oauth: cleanup expired dynamic tokens: %v", err)
	} else if n > 0 {
		log.Printf("oauth: cleaned up %d expired dynamic tokens", n)
	}

	stored, err := p.store.ListEnabled()
	if err != nil {
		return err
	}

	now := time.Now()

	// Pick the first valid token from DB.
	for _, st := range stored {
		if st.Backend == "dynamic_spoof" {
			continue
		}

		identity := p.restoreIdentity(st)

		mt := &ManagedToken{
			StoredToken: *st,
			Identity:    identity,
		}
		if st.RateResetAt != nil && st.RateResetAt.After(now) {
			mt.RateResetAt = *st.RateResetAt
			if st.RateRemaining != nil {
				mt.RateRemaining = *st.RateRemaining
			} else {
				mt.RateRemaining = 99
			}
		} else {
			mt.RateRemaining = 99
			mt.RateResetAt = now.Add(10 * time.Minute)
		}

		p.active = mt
		p.backend = st.Backend
		log.Printf("oauth: restored token %d (%s), remaining=%d", st.ID, st.Backend, mt.RateRemaining)
		break
	}

	// No tokens in DB — authenticate from config.
	if p.active == nil {
		if err := p.authenticateFromConfig(); err != nil {
			log.Printf("oauth: initial auth failed: %v", err)
		}
	}

	if p.active == nil {
		log.Printf("oauth: WARNING: no active token, will retry on first request")
	}

	p.wg.Add(1)
	go p.refreshLoop(ctx)

	return nil
}

func (p *Pool) authenticateFromConfig() error {
	for _, tcfg := range p.cfg.Tokens {
		if tcfg.Backend == "" {
			tcfg.Backend = "mobile_spoof"
		}
		result, err := p.client.Authenticate(tcfg)
		if err != nil {
			log.Printf("oauth: auth failed for %s/%s: %v", tcfg.Backend, tcfg.ClientID, err)
			continue
		}
		p.installToken(result, tcfg.ClientID, tcfg.ClientSecret, tcfg.Backend)
		return nil
	}

	// No config tokens — try mobile_spoof anonymous.
	result, err := p.client.Authenticate(config.OAuthTokenConfig{Backend: p.backend})
	if err != nil {
		return fmt.Errorf("anonymous auth (%s): %w", p.backend, err)
	}
	p.installToken(result, "", "", p.backend)
	return nil
}

func (p *Pool) installToken(result *TokenResult, clientID, clientSecret, backend string) {
	now := time.Now()
	expiresAt := now.Add(time.Duration(result.ExpiresIn) * time.Second)
	remaining := 99

	st := &store.StoredToken{
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		AccessToken:   result.AccessToken,
		ExpiresAt:     &expiresAt,
		RateRemaining: &remaining,
		Backend:       backend,
		Enabled:       true,
		LastUsed:      &now,
		HeadersJSON:   p.identityToJSON(result.Identity),
	}

	if p.active != nil && p.active.StoredToken.ID > 0 {
		st.ID = p.active.StoredToken.ID
	}

	if err := p.store.Upsert(st); err != nil {
		log.Printf("oauth: failed to store token: %v", err)
	}

	p.mu.Lock()
	p.active = &ManagedToken{
		StoredToken:   *st,
		Identity:      result.Identity,
		RateRemaining: 99,
		RateResetAt:   now.Add(10 * time.Minute),
	}
	p.backend = backend
	p.consecutiveFail = 0
	p.lastRefreshAt = now
	p.mu.Unlock()

	log.Printf("oauth: installed new %s token (expires in %ds)", backend, result.ExpiresIn)
}

func (p *Pool) refreshLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		p.mu.RLock()
		var sleepDur time.Duration
		if p.active != nil && p.active.StoredToken.ExpiresAt != nil {
			sleepDur = time.Until(*p.active.StoredToken.ExpiresAt) - 120*time.Second
		} else {
			sleepDur = 22 * time.Minute
		}
		p.mu.RUnlock()

		if sleepDur < 10*time.Second {
			sleepDur = 10 * time.Second
		}

		timer := time.NewTimer(sleepDur)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}

		log.Printf("oauth: scheduled refresh (pre-expiry)")
		p.forceRefresh("scheduled")
	}
}

// ForceRefresh re-authenticates with a new device identity. Thread-safe with cooldown.
func (p *Pool) forceRefresh(reason string) {
	if !p.refreshMu.TryLock() {
		return
	}
	defer p.refreshMu.Unlock()

	if time.Since(p.lastRefreshAt) < refreshCooldown {
		return
	}

	log.Printf("oauth: force refresh (%s), backend=%s, consecutive_fail=%d", reason, p.backend, p.consecutiveFail)

	backend := p.backend

	// Build auth config.
	tcfg := config.OAuthTokenConfig{Backend: backend}
	p.mu.RLock()
	if p.active != nil {
		tcfg.ClientID = p.active.StoredToken.ClientID
		tcfg.ClientSecret = p.active.StoredToken.ClientSecret
	}
	p.mu.RUnlock()

	if tcfg.Backend == "password" {
		for _, tc := range p.cfg.Tokens {
			if tc.ClientID == tcfg.ClientID && tc.Backend == "password" {
				tcfg.Username = tc.Username
				tcfg.Password = tc.Password
				break
			}
		}
	}

	result, err := p.client.Authenticate(tcfg)
	if err != nil {
		p.consecutiveFail++
		log.Printf("oauth: refresh failed (%s): %v (consecutive=%d)", backend, err, p.consecutiveFail)

		if p.consecutiveFail >= maxConsecutiveFails && backend == "mobile_spoof" {
			log.Printf("oauth: mobile_spoof failed %d times, switching to generic_web", p.consecutiveFail)
			p.backend = "generic_web"
			p.consecutiveFail = 0
			p.lastRefreshAt = time.Now()
		}
		return
	}

	p.installToken(result, tcfg.ClientID, tcfg.ClientSecret, backend)
}

// NotifyUnauthorized is called when a 401 is received. Triggers re-auth.
func (p *Pool) NotifyUnauthorized() {
	go p.forceRefresh("401_unauthorized")
}

// NotifyLowQuota is called when remaining is critically low.
func (p *Pool) NotifyLowQuota() {
	go p.forceRefresh("low_quota")
}

func (p *Pool) GetBestToken() *ManagedToken {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.active == nil {
		return nil
	}

	now := time.Now()
	if now.After(p.active.RateResetAt) {
		p.active.RateRemaining = 99
		p.active.RateResetAt = now.Add(10 * time.Minute)
		remaining := 99
		resetAt := p.active.RateResetAt
		p.active.StoredToken.RateRemaining = &remaining
		p.active.StoredToken.RateResetAt = &resetAt
		if p.store != nil {
			snapshot := p.active.StoredToken
			go func() {
				if err := p.store.UpdateToken(&snapshot); err != nil {
					log.Printf("oauth: persist reset: %v", err)
				}
			}()
		}
	}

	if p.active.RateRemaining <= 0 {
		return nil
	}

	return p.active
}

func (p *Pool) OnRequestComplete(tokenID int, resp *http.Response) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.active == nil || p.active.StoredToken.ID != tokenID {
		return
	}

	mt := p.active

	if v := resp.Header.Get("X-Ratelimit-Remaining"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			mt.RateRemaining = int(f)
			remaining := int(f)
			mt.StoredToken.RateRemaining = &remaining
		}
	}
	if v := resp.Header.Get("X-Ratelimit-Reset"); v != "" {
		if secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			resetAt := time.Now().Add(time.Duration(secs) * time.Second)
			mt.RateResetAt = resetAt
			mt.StoredToken.RateResetAt = &resetAt
		}
	}

	now := time.Now()
	mt.StoredToken.LastUsed = &now

	if mt.RateRemaining < 2 {
		log.Printf("oauth: quota critically low (%d), triggering refresh", mt.RateRemaining)
		go p.NotifyLowQuota()
	}

	if p.store != nil {
		snapshot := mt.StoredToken
		go func() {
			if err := p.store.UpdateToken(&snapshot); err != nil {
				log.Printf("oauth: persist rate state: %v", err)
			}
		}()
	}
}

// RemainingBudget implements ratelimit.BudgetSource.
func (p *Pool) RemainingBudget(_ context.Context) (int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return 0, nil
	}

	remaining := p.active.RateRemaining
	if time.Now().After(p.active.RateResetAt) {
		remaining = 99
	}
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

type TokenStatusInfo struct {
	Backend       string
	RateRemaining int
	RateResetAt   time.Time
	Dynamic       bool
	UserAgent     string
	DeviceID      string
	Loid          string
	Session       string
	ExpiresAt     *time.Time
}

func (p *Pool) TokenStatuses() []TokenStatusInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return nil
	}

	return []TokenStatusInfo{{
		Backend:       p.active.StoredToken.Backend,
		RateRemaining: p.active.RateRemaining,
		RateResetAt:   p.active.RateResetAt,
		UserAgent:     p.active.Identity.UserAgent,
		DeviceID:      p.active.Identity.DeviceID,
		Loid:          p.active.Identity.Headers["x-reddit-loid"],
		Session:       p.active.Identity.Headers["x-reddit-session"],
		ExpiresAt:     p.active.StoredToken.ExpiresAt,
	}}
}

// WindowInfo returns the rate limit window state.
func (p *Pool) WindowInfo() (resetAt time.Time, capacity int, remaining int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return
	}

	const window = 10 * time.Minute
	capacity = 99
	now := time.Now()
	tokenReset := p.active.RateResetAt
	if now.After(tokenReset) {
		remaining = 99
		elapsed := now.Sub(tokenReset)
		tokenReset = tokenReset.Add((elapsed/window + 1) * window)
	} else if p.active.RateRemaining > 0 {
		remaining = p.active.RateRemaining
	}
	resetAt = tokenReset
	return
}

// EarliestReset returns (seconds until reset, window total seconds).
func (p *Pool) EarliestReset() (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	const window = 10 * time.Minute
	windowSec := int(window.Seconds())

	if p.active == nil {
		return 0, windowSec
	}

	now := time.Now()
	resetAt := p.active.RateResetAt
	if !resetAt.After(now) {
		elapsed := now.Sub(resetAt)
		resetAt = resetAt.Add((elapsed/window + 1) * window)
	}
	secs := int(time.Until(resetAt).Seconds())
	if secs < 0 {
		secs = 0
	}
	return secs, windowSec
}

func (p *Pool) EarliestResetSeconds() int {
	s, _ := p.EarliestReset()
	return s
}

// HasAvailableTokens reports whether the active token has remaining quota.
func (p *Pool) HasAvailableTokens() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return false
	}
	if p.active.RateRemaining > 0 {
		return true
	}
	return time.Now().After(p.active.RateResetAt)
}

// SpawnTokenIfNeeded is a no-op kept for API compatibility. Single-token model
// handles recovery via NotifyUnauthorized / NotifyLowQuota.
func (p *Pool) SpawnTokenIfNeeded(_ context.Context) {}

func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// --- Identity persistence helpers ---

func (p *Pool) identityToJSON(id SpoofIdentity) *string {
	data := map[string]string{
		"_user_agent": id.UserAgent,
		"_device_id":  id.DeviceID,
	}
	for k, v := range id.Headers {
		data[k] = v
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func (p *Pool) restoreIdentity(st *store.StoredToken) SpoofIdentity {
	if st.HeadersJSON != nil && *st.HeadersJSON != "" {
		var data map[string]string
		if err := json.Unmarshal([]byte(*st.HeadersJSON), &data); err == nil && len(data) > 0 {
			ua := data["_user_agent"]
			deviceID := data["_device_id"]
			delete(data, "_user_agent")
			delete(data, "_device_id")
			log.Printf("oauth: restored identity from DB for token %d (ua=%q)", st.ID, ua)
			return SpoofIdentity{
				UserAgent: ua,
				DeviceID:  deviceID,
				Headers:   data,
			}
		}
	}

	// Fallback: generate new identity based on backend.
	log.Printf("oauth: no persisted identity for token %d, generating new", st.ID)
	if st.Backend == "mobile_spoof" || st.Backend == "" {
		return GenerateIdentity()
	}
	return GenerateWebIdentity(p.uaPool)
}
