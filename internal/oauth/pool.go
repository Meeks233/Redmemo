package oauth

import (
	"context"
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

type Pool struct {
	mu     sync.RWMutex
	tokens []*ManagedToken
	client *Client
	store  *store.TokenStore
	cache  *cache.Cache
	cfg    config.OAuthConfig
	cancel context.CancelFunc
	wg     sync.WaitGroup
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
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best *ManagedToken
	for _, mt := range p.tokens {
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

	total := 0
	for _, mt := range p.tokens {
		if mt.RateRemaining > 0 {
			total += mt.RateRemaining
		}
	}
	return total, nil
}

func (p *Pool) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}
