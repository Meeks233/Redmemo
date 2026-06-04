package reddit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/redmemo/redmemo/internal/transport"
)

// httpDoer is the subset of tls_client.HttpClient the Reddit clients depend on.
// Narrowing the field to this interface keeps the spoofed transport in
// production while letting tests inject a plain fhttp client.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

const redditAPIBase = "https://oauth.reddit.com"

var (
	ErrNoTokenAvailable = errors.New("no OAuth token available")
	ErrSuspended        = errors.New("user is suspended")
	ErrQuarantined      = errors.New("subreddit is quarantined")
	ErrPrivate          = errors.New("subreddit is private")
	ErrBanned           = errors.New("subreddit is banned")
	ErrGated            = errors.New("subreddit is gated")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrRateLimited      = errors.New("rate limited")
)

// TokenProvider abstracts the OAuth token source.
// Implemented by oauth.TokenHolder.
type TokenProvider interface {
	Token() *TokenInfo
	OnRequestComplete(tokenID int, resp *http.Response)
	NotifyUnauthorized()
}

// TokenInfo holds the info needed to make an authenticated request.
// Headers carries the full spoof header set, including User-Agent.
type TokenInfo struct {
	ID          int
	AccessToken string
	Headers     map[string]string
}

// Client is a Reddit API client backed by an OAuth token source.
type Client struct {
	tokens     TokenProvider
	httpClient httpDoer
	etags      *etagCache
}

// NewClient creates a new Reddit API client. Redirects are not followed by the
// transport — doRequestDepth handles 3xx hops itself.
func NewClient(tokens TokenProvider) *Client {
	return &Client{
		tokens:     tokens,
		httpClient: transport.NewSpoofedClient(30*time.Second, tls_client.WithNotFollowRedirects()),
		etags:      newETagCache(2000),
	}
}

// FetchSubreddit fetches a subreddit listing. `t` is Reddit's relative
// timeframe (hour|day|week|month|year|all) and is honored only by the
// top/controversial sorts; it is harmlessly ignored by others.
// Returns posts, before cursor, after cursor, error.
// `before` paginates backward (Reddit's `before=t3_xxx`); when both before and
// after are supplied Reddit honours after, so callers normally set one or the
// other depending on which direction the user clicked.
func (c *Client) FetchSubreddit(ctx context.Context, sub, sort, t, after, before string, limit int) ([]Post, string, string, error) {
	if sort == "" {
		sort = "hot"
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}

	path := fmt.Sprintf("/r/%s/%s.json?raw_json=1&include_over_18=on&limit=%d", sub, sort, limit)
	if t != "" {
		path += "&t=" + url.QueryEscape(t)
	}
	if after != "" {
		path += "&after=" + after
	}
	if before != "" {
		path += "&before=" + before
	}

	data, _, err := c.doRequest(ctx, path)
	if err != nil {
		return nil, "", "", err
	}

	return ParseSubredditListing(data)
}

// FetchPost fetches a post and its comments.
func (c *Client) FetchPost(ctx context.Context, sub, id, commentSort string) (Post, []Comment, error) {
	return c.FetchPostLimited(ctx, sub, id, commentSort, 0)
}

// FetchPostLimited fetches a post and the first `limit` top-level comments.
// limit<=0 means no cap (Reddit's default ~200). A small initial limit saves
// quota: Reddit returns the requested comments plus a "more" placeholder that
// the user can expand on demand.
func (c *Client) FetchPostLimited(ctx context.Context, sub, id, commentSort string, limit int) (Post, []Comment, error) {
	if commentSort == "" {
		commentSort = "confidence"
	}
	path := fmt.Sprintf("/r/%s/comments/%s.json?raw_json=1&include_over_18=on&sort=%s", sub, id, commentSort)
	if limit > 0 {
		path += fmt.Sprintf("&limit=%d", limit)
	}

	data, _, err := c.doRequest(ctx, path)
	if err != nil {
		return Post{}, nil, err
	}

	return ParsePostPage(data)
}

// FetchSubredditAbout fetches subreddit metadata.
func (c *Client) FetchSubredditAbout(ctx context.Context, sub string) (Subreddit, error) {
	path := fmt.Sprintf("/r/%s/about.json?raw_json=1&include_over_18=on", sub)

	data, _, err := c.doRequest(ctx, path)
	if err != nil {
		return Subreddit{}, err
	}

	return ParseSubredditAbout(data)
}

// FetchUser fetches user profile and listings.
func (c *Client) FetchUser(ctx context.Context, username, listing, sort, after string) (User, []Post, []Comment, error) {
	// Fetch user about
	aboutData, _, err := c.doRequest(ctx, fmt.Sprintf("/user/%s/about.json?raw_json=1&include_over_18=on", username))
	if err != nil {
		return User{}, nil, nil, err
	}
	user, err := ParseUserAbout(aboutData)
	if err != nil {
		return User{}, nil, nil, err
	}

	// Fetch user listing
	if listing == "" {
		listing = "overview"
	}
	listPath := fmt.Sprintf("/user/%s/%s.json?raw_json=1&include_over_18=on", username, listing)
	if sort != "" {
		listPath += "&sort=" + sort
	}
	if after != "" {
		listPath += "&after=" + after
	}

	listData, _, err := c.doRequest(ctx, listPath)
	if err != nil {
		return user, nil, nil, err
	}

	posts, comments, _, _, _ := ParseUserListing(listData)
	return user, posts, comments, nil
}

// FetchSearch performs a search. Returns posts, subreddits, before cursor,
// after cursor, error — mirroring FetchSubreddit so callers can render both
// Prev and Next pagination links.
func (c *Client) FetchSearch(ctx context.Context, query, sub, sort, t, after, before string, limit int) ([]Post, []Subreddit, string, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	// query must be URL-encoded: multi-word searches ("linux video") contain
	// spaces and other reserved characters that would otherwise produce a
	// malformed request line and fail upstream.
	eq := url.QueryEscape(query)
	var path string
	if sub != "" {
		path = fmt.Sprintf("/r/%s/search.json?raw_json=1&include_over_18=on&limit=%d&q=%s", sub, limit, eq)
	} else {
		path = fmt.Sprintf("/search.json?raw_json=1&include_over_18=on&limit=%d&q=%s", limit, eq)
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

	data, _, err := c.doRequest(ctx, path)
	if err != nil {
		return nil, nil, "", "", err
	}

	posts, subs, beforeCursor, afterCursor, err := ParseSearchResults(data)
	return posts, subs, beforeCursor, afterCursor, err
}

// Probe sends a lightweight request to check rate limit headers.
func (c *Client) Probe(ctx context.Context) (*RateLimitInfo, error) {
	_, resp, err := c.doRequest(ctx, "/api/v1/me.json")
	if err != nil {
		return nil, err
	}

	info := &RateLimitInfo{}
	if s := resp.Header.Get("X-Ratelimit-Remaining"); s != "" {
		info.Remaining, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
	}
	if s := resp.Header.Get("X-Ratelimit-Reset"); s != "" {
		f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		info.Reset = int64(f)
	}
	if s := resp.Header.Get("X-Ratelimit-Used"); s != "" {
		info.Used, _ = strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	}
	return info, nil
}

// Matches redlib: opts in to quarantined and gated subreddits via cookie.
const quarantineCookie = `_options=%7B%22pref_quarantine_optin%22%3A%20true%2C%20%22pref_gated_sr_optin%22%3A%20true%7D`

// maxRedirects caps how many upstream 3xx hops doRequest will follow before
// giving up, so a redirect loop can't recurse until the stack overflows.
const maxRedirects = 5

func (c *Client) doRequest(ctx context.Context, path string) ([]byte, *http.Response, error) {
	return c.doRequestDepth(ctx, path, 0)
}

func (c *Client) doRequestDepth(ctx context.Context, path string, depth int) ([]byte, *http.Response, error) {
	token := c.tokens.Token()
	if token == nil {
		return nil, nil, ErrNoTokenAvailable
	}

	req, err := http.NewRequestWithContext(ctx, "GET", redditAPIBase+path, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Host", "oauth.reddit.com")
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Cookie", quarantineCookie)
	for k, v := range token.Headers {
		req.Header.Set(k, v)
	}

	cachedETag, cachedBody, hasCached := c.etags.Get(path)
	if hasCached {
		req.Header.Set("If-None-Match", cachedETag)
	}

	transport.ApplyHeaderOrder(req)

	resp, err := c.httpClient.Do(req)
	// tls-client does not auto-retry idempotent GETs on stale keep-alive
	// connections the way net/http does. NP listings fire every ~25 minutes;
	// by then Reddit (or upstream NAT) has silently dropped the idle h2 conn,
	// and the first reuse returns io.EOF / connection reset / broken pipe.
	// One retry on these transport errors recovers the round without changing
	// behavior for real failures (the second attempt opens a fresh conn).
	if err != nil && isTransientTransportErr(err) {
		resp, err = c.httpClient.Do(req)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Update token rate limits
	c.tokens.OnRequestComplete(token.ID, resp)

	// 304 Not Modified — return cached body
	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cachedBody, resp, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp, fmt.Errorf("read body: %w", err)
	}

	// Handle redirects
	if resp.StatusCode >= 301 && resp.StatusCode <= 399 {
		location := resp.Header.Get("Location")
		if location != "" {
			if depth >= maxRedirects {
				return nil, resp, fmt.Errorf("too many redirects (>%d) starting at %s", maxRedirects, path)
			}
			newPath := location
			newPath = strings.TrimPrefix(newPath, redditAPIBase)
			newPath = strings.TrimPrefix(newPath, "https://www.reddit.com")
			newPath = strings.TrimPrefix(newPath, "https://oauth.reddit.com")
			if !strings.Contains(newPath, "raw_json=1") {
				if strings.Contains(newPath, "?") {
					newPath += "&raw_json=1"
				} else {
					newPath += "?raw_json=1"
				}
			}
			return c.doRequestDepth(ctx, newPath, depth+1)
		}
	}

	// Handle errors
	if resp.StatusCode == 401 {
		c.tokens.NotifyUnauthorized()
		return nil, resp, ErrUnauthorized
	}

	if resp.StatusCode == 429 || (resp.StatusCode == 403 && resp.Header.Get("Retry-After") != "") {
		return nil, resp, ErrRateLimited
	}

	if len(body) == 0 && resp.StatusCode != 204 {
		return nil, resp, ErrRateLimited
	}

	if err := checkAPIError(body); err != nil {
		return nil, resp, err
	}

	// Cache ETag for future conditional requests
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.etags.Set(path, etag, body)
	}

	return body, resp, nil
}

// isTransientTransportErr reports whether err looks like a dropped keep-alive
// connection — safe to retry once for an idempotent GET. Matches against error
// text because tls-client wraps the underlying net error opaquely.
func isTransientTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected EOF")
}

func checkAPIError(body []byte) error {
	// Quick check for common error patterns without full JSON parse
	s := string(body)
	if strings.Contains(s, `"is_suspended":true`) || strings.Contains(s, `"is_suspended": true`) {
		return ErrSuspended
	}
	if strings.Contains(s, `"reason":"quarantined"`) {
		return ErrQuarantined
	}
	if strings.Contains(s, `"reason":"private"`) {
		return ErrPrivate
	}
	if strings.Contains(s, `"reason":"banned"`) {
		return ErrBanned
	}
	if strings.Contains(s, `"reason":"gated"`) {
		return ErrGated
	}
	if strings.Contains(s, `"message":"Unauthorized"`) {
		return ErrUnauthorized
	}
	return nil
}
