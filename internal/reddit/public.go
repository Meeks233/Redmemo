package reddit

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/transport"
)

// publicCookie augments the quarantine/gated opt-in cookie with over18=1.
// www.reddit.com gates NSFW listings on this cookie (the oauth.reddit.com
// host instead honors the include_over_18 query param), so without it NSFW
// subreddits like r/golang are rejected with a 403.
const publicCookie = quarantineCookie + `; over18=1`

type PublicClient struct {
	httpClient httpDoer
	// userAgentFn returns the active OAuth session's bound User-Agent so every
	// public-endpoint request shares one identity with the authenticated API
	// client. It is expected to block during cold start (see
	// TokenHolder.WaitForUserAgent) rather than fall back to any other UA —
	// emitting a second UA from the session IP is a stealth tell.
	userAgentFn func() string

	mu         sync.Mutex
	tokens     int
	maxTokens  int
	lastRefill time.Time
	refillRate time.Duration
}

func NewPublicClient(userAgentFn func() string) *PublicClient {
	return &PublicClient{
		httpClient:  transport.NewSpoofedClient(15 * time.Second),
		userAgentFn: userAgentFn,
		tokens:      8,
		maxTokens:   8,
		lastRefill:  time.Now(),
		refillRate:  8 * time.Second,
	}
}

func (c *PublicClient) tryAcquire() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	newTokens := int(now.Sub(c.lastRefill) / c.refillRate)
	if newTokens > 0 {
		c.tokens += newTokens
		if c.tokens > c.maxTokens {
			c.tokens = c.maxTokens
		}
		c.lastRefill = now
	}

	if c.tokens > 0 {
		c.tokens--
		return true
	}
	return false
}

func (c *PublicClient) fetch(ctx context.Context, path string) ([]byte, error) {
	if !c.tryAcquire() {
		return nil, fmt.Errorf("public api: rate limited locally")
	}

	ua := c.userAgentFn()
	if ua == "" {
		return nil, fmt.Errorf("public api: no session user-agent available")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.reddit.com"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", publicCookie)
	transport.ApplyHeaderOrder(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("public api: 429 rate limited")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("public api: status %d for %s", resp.StatusCode, path)
	}

	return io.ReadAll(resp.Body)
}

func (c *PublicClient) FetchSubredditAbout(ctx context.Context, sub string) (Subreddit, error) {
	path := fmt.Sprintf("/r/%s/about.json?raw_json=1", url.PathEscape(sub))
	data, err := c.fetch(ctx, path)
	if err != nil {
		return Subreddit{}, err
	}
	return ParseSubredditAbout(data)
}
