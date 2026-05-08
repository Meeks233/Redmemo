package oauth

import (
	"context"
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
)

type ManagedToken struct {
	StoredToken   store.StoredToken
	Identity      SpoofIdentity
	RateRemaining int
	RateResetAt   time.Time
}

const (
	dynamicSpoofBackend = "dynamic_spoof"
	maxDynamicTokens    = 3
)

type Pool struct {
	mu     sync.RWMutex
	tokens []*ManagedToken
	client *Client
	store  *store.TokenStore
	cache  *cache.Cache
	cfg    config.OAuthConfig
	cancel context.CancelFunc
	wg     sync.WaitGroup

	spawnMu sync.Mutex
	bgCtx   context.Context
}

func NewPool(cfg config.OAuthConfig, client *Client, tokenStore *store.TokenStore, c *cache.Cache) *Pool {
	return &Pool{
		client: client,
		store:  tokenStore,
		cache:  c,
		cfg:    cfg,
	}
}

func (p *Pool) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	p.bgCtx = ctx

	// Clean up expired dynamic tokens from DB.
	if n, err := p.store.DeleteExpiredByBackend(dynamicSpoofBackend); err != nil {
		log.Printf("oauth: cleanup expired dynamic tokens: %v", err)
	} else if n > 0 {
		log.Printf("oauth: cleaned up %d expired dynamic tokens", n)
	}

	stored, err := p.store.ListEnabled()
	if err != nil {
		return err
	}

	p.mu.Lock()
	for _, st := range stored {
		mt := &ManagedToken{
			StoredToken: *st,
			Identity:    GenerateIdentity(),
		}
		if st.RateRemaining != nil {
			mt.RateRemaining = *st.RateRemaining
		} else {
			mt.RateRemaining = 99
		}
		if st.RateResetAt != nil {
			mt.RateResetAt = *st.RateResetAt
		}
		p.tokens = append(p.tokens, mt)
	}
	p.mu.Unlock()

	// If no tokens in DB but config has entries, authenticate them now.
	if len(p.tokens) == 0 {
		for _, tcfg := range p.cfg.Tokens {
			result, err := p.client.Authenticate(tcfg)
			if err != nil {
				log.Printf("oauth: initial auth failed for %s: %v", tcfg.ClientID, err)
				continue
			}
			now := time.Now()
			expiresAt := now.Add(time.Duration(result.ExpiresIn) * time.Second)
			remaining := 99
			st := &store.StoredToken{
				ClientID:      tcfg.ClientID,
				ClientSecret:  tcfg.ClientSecret,
				AccessToken:   result.AccessToken,
				ExpiresAt:     &expiresAt,
				RateRemaining: &remaining,
				Backend:       tcfg.Backend,
				Enabled:       true,
				LastUsed:      &now,
			}
			if err := p.store.Upsert(st); err != nil {
				log.Printf("oauth: failed to store token: %v", err)
				continue
			}

			p.mu.Lock()
			p.tokens = append(p.tokens, &ManagedToken{
				StoredToken:   *st,
				Identity:      GenerateIdentity(),
				RateRemaining: 99,
				RateResetAt:   now.Add(10 * time.Minute),
			})
			p.mu.Unlock()
		}
	}

	p.mu.RLock()
	for _, mt := range p.tokens {
		p.wg.Add(1)
		go p.refreshLoop(ctx, mt)
	}
	p.mu.RUnlock()

	return nil
}

func (p *Pool) refreshLoop(ctx context.Context, mt *ManagedToken) {
	defer p.wg.Done()
	for {
		p.mu.RLock()
		expiresAt := mt.StoredToken.ExpiresAt
		p.mu.RUnlock()

		var sleepDur time.Duration
		if expiresAt != nil {
			sleepDur = time.Until(*expiresAt) - 120*time.Second
		} else {
			sleepDur = 22 * time.Minute
		}
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

		tcfg := config.OAuthTokenConfig{
			ClientID:     mt.StoredToken.ClientID,
			ClientSecret: mt.StoredToken.ClientSecret,
			Backend:      mt.StoredToken.Backend,
		}
		if tcfg.Backend == "password" {
			for _, tc := range p.cfg.Tokens {
				if tc.ClientID == tcfg.ClientID && tc.Backend == "password" {
					tcfg.Username = tc.Username
					tcfg.Password = tc.Password
					break
				}
			}
		}
		result, err := p.client.Refresh(tcfg)
		if err != nil {
			log.Printf("oauth: refresh failed for token %d: %v", mt.StoredToken.ID, err)
			continue
		}

		now := time.Now()
		expiresAtNew := now.Add(time.Duration(result.ExpiresIn) * time.Second)
		remaining := 99

		p.mu.Lock()
		mt.StoredToken.AccessToken = result.AccessToken
		mt.StoredToken.ExpiresAt = &expiresAtNew
		mt.StoredToken.RateRemaining = &remaining
		mt.StoredToken.LastUsed = &now
		mt.Identity = GenerateIdentity()
		mt.RateRemaining = 99
		mt.RateResetAt = now.Add(10 * time.Minute)
		p.mu.Unlock()

		if err := p.store.UpdateToken(&mt.StoredToken); err != nil {
			log.Printf("oauth: failed to persist refreshed token %d: %v", mt.StoredToken.ID, err)
		}
	}
}

func (p *Pool) GetBestToken() *ManagedToken {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var best *ManagedToken
	for _, mt := range p.tokens {
		if mt.RateRemaining <= 0 && now.After(mt.RateResetAt) {
			mt.RateRemaining = 99
			mt.RateResetAt = now.Add(10 * time.Minute)
			remaining := 99
			mt.StoredToken.RateRemaining = &remaining
		}
		if mt.RateRemaining <= 0 {
			continue
		}
		if best == nil || mt.RateRemaining > best.RateRemaining {
			best = mt
		}
	}
	return best
}

func (p *Pool) OnRequestComplete(tokenID int, resp *http.Response) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var mt *ManagedToken
	for _, t := range p.tokens {
		if t.StoredToken.ID == tokenID {
			mt = t
			break
		}
	}
	if mt == nil {
		return
	}

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

	if mt.RateRemaining < 10 {
		log.Printf("oauth: token %d low on quota (%d remaining)", tokenID, mt.RateRemaining)
	}
}

// RemainingBudget implements ratelimit.BudgetSource.
func (p *Pool) RemainingBudget(_ context.Context) (int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	total := 0
	for _, mt := range p.tokens {
		remaining := mt.RateRemaining
		if remaining <= 0 && now.After(mt.RateResetAt) {
			remaining = 99
		}
		if remaining > 0 {
			total += remaining
		}
	}
	return total, nil
}

// HasAvailableTokens reports whether any token in the pool has remaining quota.
func (p *Pool) HasAvailableTokens() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	for _, mt := range p.tokens {
		if mt.RateRemaining > 0 {
			return true
		}
		if now.After(mt.RateResetAt) {
			return true
		}
	}
	return false
}

// SpawnTokenIfNeeded creates a dynamic OAuth token in the background when the
// pool has no available tokens and hasn't reached the dynamic token cap.
func (p *Pool) SpawnTokenIfNeeded(ctx context.Context) {
	if p.HasAvailableTokens() {
		return
	}

	if !p.spawnMu.TryLock() {
		return
	}
	defer p.spawnMu.Unlock()

	if p.HasAvailableTokens() {
		return
	}

	if p.dynamicCount() >= maxDynamicTokens {
		log.Printf("oauth: dynamic token cap reached (%d/%d)", p.dynamicCount(), maxDynamicTokens)
		return
	}

	log.Printf("oauth: spawning dynamic token (current dynamic: %d/%d)", p.dynamicCount(), maxDynamicTokens)
	if err := p.spawnOne(ctx); err != nil {
		log.Printf("oauth: spawn dynamic token failed: %v", err)
	}
}

func (p *Pool) dynamicCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, mt := range p.tokens {
		if mt.StoredToken.Backend == dynamicSpoofBackend {
			n++
		}
	}
	return n
}

func (p *Pool) spawnOne(ctx context.Context) error {
	result, err := p.client.Authenticate(config.OAuthTokenConfig{Backend: "mobile_spoof"})
	if err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}

	now := time.Now()
	expiresAt := now.Add(time.Duration(result.ExpiresIn) * time.Second)
	remaining := 99
	st := &store.StoredToken{
		ClientID:      "dynamic",
		AccessToken:   result.AccessToken,
		ExpiresAt:     &expiresAt,
		RateRemaining: &remaining,
		Backend:       dynamicSpoofBackend,
		Enabled:       true,
		LastUsed:      &now,
	}
	if err := p.store.Upsert(st); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}

	mt := &ManagedToken{
		StoredToken:   *st,
		Identity:      GenerateIdentity(),
		RateRemaining: 99,
		RateResetAt:   now.Add(10 * time.Minute),
	}

	p.mu.Lock()
	p.tokens = append(p.tokens, mt)
	p.mu.Unlock()

	bgCtx := p.bgCtx
	if bgCtx == nil {
		bgCtx = ctx
	}
	p.wg.Add(1)
	go p.refreshLoop(bgCtx, mt)

	log.Printf("oauth: dynamic token spawned, expires in %ds", result.ExpiresIn)
	return nil
}

func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}
