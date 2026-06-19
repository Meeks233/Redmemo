package reddit

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
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

func (c *PublicClient) FetchSubreddit(ctx context.Context, sub, sort, t, after, before string, limit int) ([]Post, string, string, error) {
	if sort == "" {
		sort = "hot"
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	path := fmt.Sprintf("/r/%s/%s.json?raw_json=1&limit=%d", url.PathEscape(sub), url.PathEscape(sort), limit)
	if t != "" {
		path += "&t=" + url.QueryEscape(t)
	}
	if after != "" {
		path += "&after=" + url.QueryEscape(after)
	}
	if before != "" {
		path += "&before=" + url.QueryEscape(before)
	}
	data, err := c.fetch(ctx, path)
	if err != nil {
		return nil, "", "", err
	}
	return ParseSubredditListing(data)
}

func (c *PublicClient) FetchPost(ctx context.Context, sub, id, commentSort string) (Post, []Comment, error) {
	return c.FetchPostLimited(ctx, sub, id, commentSort, 0)
}

func (c *PublicClient) FetchPostLimited(ctx context.Context, sub, id, commentSort string, limit int) (Post, []Comment, error) {
	if commentSort == "" {
		commentSort = "confidence"
	}
	path := fmt.Sprintf("/r/%s/comments/%s.json?raw_json=1&sort=%s", url.PathEscape(sub), url.PathEscape(id), url.QueryEscape(commentSort))
	if limit > 0 {
		path += fmt.Sprintf("&limit=%d", limit)
	}
	data, err := c.fetch(ctx, path)
	if err != nil {
		return Post{}, nil, err
	}
	return ParsePostPage(data)
}

// FetchMoreChildren is Reddit's quota-frugal "load N more children" call.
// We pass the exact child IDs from a "more" stub (up to 100 per call) and
// get back ONLY those expanded comments plus whatever nested replies Reddit
// inlines — one request per click regardless of how many remain hidden,
// vs. the focus-view alternative that re-fetches every visible sibling
// too. /api/morechildren is unauthenticated-friendly so PublicClient uses
// the same path the OAuth Client does.
func (c *PublicClient) FetchMoreChildren(ctx context.Context, sub, postID string, childrenIDs []string, sort string) ([]Comment, error) {
	if sort == "" {
		sort = "confidence"
	}
	if len(childrenIDs) == 0 {
		return nil, nil
	}
	path := fmt.Sprintf("/api/morechildren.json?api_type=json&raw_json=1&link_id=t3_%s&children=%s&sort=%s",
		postID, strings.Join(childrenIDs, ","), sort)
	data, err := c.fetch(ctx, path)
	if err != nil {
		return nil, err
	}
	postLink := fmt.Sprintf("/r/%s/comments/%s/", sub, postID)
	return ParseMoreChildren(data, postLink, "")
}

func (c *PublicClient) FetchSearch(ctx context.Context, query, sub, sort, t, after, before string, limit int) ([]Post, []Subreddit, string, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	// query must be URL-encoded: multi-word searches contain spaces and other
	// reserved characters that would otherwise produce a malformed request.
	eq := url.QueryEscape(query)
	var path string
	if sub != "" {
		path = fmt.Sprintf("/r/%s/search.json?raw_json=1&limit=%d&q=%s", url.PathEscape(sub), limit, eq)
	} else {
		path = fmt.Sprintf("/search.json?raw_json=1&limit=%d&q=%s", limit, eq)
	}
	if sort != "" {
		path += "&sort=" + url.QueryEscape(sort)
	}
	if t != "" {
		path += "&t=" + url.QueryEscape(t)
	}
	if after != "" {
		path += "&after=" + url.QueryEscape(after)
	}
	if before != "" {
		path += "&before=" + url.QueryEscape(before)
	}
	data, err := c.fetch(ctx, path)
	if err != nil {
		return nil, nil, "", "", err
	}
	posts, subs, beforeCursor, afterCursor, err := ParseSearchResults(data)
	return posts, subs, beforeCursor, afterCursor, err
}

func (c *PublicClient) FetchUser(ctx context.Context, username, listing, sort, after string) (User, []Post, []Comment, error) {
	aboutData, err := c.fetch(ctx, fmt.Sprintf("/user/%s/about.json?raw_json=1", url.PathEscape(username)))
	if err != nil {
		return User{}, nil, nil, err
	}
	user, err := ParseUserAbout(aboutData)
	if err != nil {
		return User{}, nil, nil, err
	}

	if listing == "" {
		listing = "overview"
	}
	listPath := fmt.Sprintf("/user/%s/%s.json?raw_json=1", url.PathEscape(username), url.PathEscape(listing))
	if sort != "" {
		listPath += "&sort=" + url.QueryEscape(sort)
	}
	if after != "" {
		listPath += "&after=" + url.QueryEscape(after)
	}
	listData, err := c.fetch(ctx, listPath)
	if err != nil {
		return user, nil, nil, err
	}
	posts, comments, _, _, _ := ParseUserListing(listData)
	return user, posts, comments, nil
}

func (c *PublicClient) FetchSubredditAbout(ctx context.Context, sub string) (Subreddit, error) {
	path := fmt.Sprintf("/r/%s/about.json?raw_json=1", url.PathEscape(sub))
	data, err := c.fetch(ctx, path)
	if err != nil {
		return Subreddit{}, err
	}
	return ParseSubredditAbout(data)
}
