package reddit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/transport"
	"github.com/redmemo/redmemo/internal/useragent"
)

type PublicClient struct {
	httpClient *http.Client
	uaPool     *useragent.Pool

	mu         sync.Mutex
	tokens     int
	maxTokens  int
	lastRefill time.Time
	refillRate time.Duration
}

func NewPublicClient(uaPool *useragent.Pool) *PublicClient {
	return &PublicClient{
		httpClient: transport.NewSpoofedClient(15 * time.Second),
		uaPool:     uaPool,
		tokens:     8,
		maxTokens:  8,
		lastRefill: time.Now(),
		refillRate: 8 * time.Second,
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

	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.reddit.com"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.uaPool.Get())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", `_options={%22pref_quarantine_optin%22:true}`)

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

func (c *PublicClient) FetchSubreddit(ctx context.Context, sub, sort, after string, limit int) ([]Post, string, string, error) {
	if sort == "" {
		sort = "hot"
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	path := fmt.Sprintf("/r/%s/%s.json?raw_json=1&limit=%d", sub, sort, limit)
	if after != "" {
		path += "&after=" + after
	}
	data, err := c.fetch(ctx, path)
	if err != nil {
		return nil, "", "", err
	}
	return ParseSubredditListing(data)
}

func (c *PublicClient) FetchPost(ctx context.Context, sub, id, commentSort string) (Post, []Comment, error) {
	if commentSort == "" {
		commentSort = "confidence"
	}
	path := fmt.Sprintf("/r/%s/comments/%s.json?raw_json=1&sort=%s", sub, id, commentSort)
	data, err := c.fetch(ctx, path)
	if err != nil {
		return Post{}, nil, err
	}
	return ParsePostPage(data)
}

func (c *PublicClient) FetchSearch(ctx context.Context, query, sub, sort, t, after string, restrictSR bool, limit int) ([]Post, []Subreddit, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	var path string
	if sub != "" {
		path = fmt.Sprintf("/r/%s/search.json?raw_json=1&limit=%d&q=%s", sub, limit, query)
		if restrictSR {
			path += "&restrict_sr=on"
		}
	} else {
		path = fmt.Sprintf("/search.json?raw_json=1&limit=%d&q=%s", limit, query)
	}
	if sort != "" {
		path += "&sort=" + sort
	}
	if t != "" {
		path += "&t=" + t
	}
	if after != "" {
		path += "&after=" + after
	}
	data, err := c.fetch(ctx, path)
	if err != nil {
		return nil, nil, "", err
	}
	posts, subs, _, afterCursor, err := ParseSearchResults(data)
	return posts, subs, afterCursor, err
}

func (c *PublicClient) FetchUser(ctx context.Context, username, listing, sort, after string) (User, []Post, []Comment, error) {
	aboutData, err := c.fetch(ctx, fmt.Sprintf("/user/%s/about.json?raw_json=1", username))
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
	listPath := fmt.Sprintf("/user/%s/%s.json?raw_json=1", username, listing)
	if sort != "" {
		listPath += "&sort=" + sort
	}
	if after != "" {
		listPath += "&after=" + after
	}
	listData, err := c.fetch(ctx, listPath)
	if err != nil {
		return user, nil, nil, err
	}
	posts, comments, _, _, _ := ParseUserListing(listData)
	return user, posts, comments, nil
}

func (c *PublicClient) FetchSubredditAbout(ctx context.Context, sub string) (Subreddit, error) {
	path := fmt.Sprintf("/r/%s/about.json?raw_json=1", sub)
	data, err := c.fetch(ctx, path)
	if err != nil {
		return Subreddit{}, err
	}
	return ParseSubredditAbout(data)
}
