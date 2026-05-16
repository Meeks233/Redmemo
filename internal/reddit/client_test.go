package reddit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- mock TokenProvider ---

type mockPool struct {
	mu              sync.Mutex
	token           *TokenInfo
	completeCalls   []int
	unauthorizedCnt int
}

func (m *mockPool) GetBestToken() *TokenInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token
}

func (m *mockPool) OnRequestComplete(tokenID int, _ *http.Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeCalls = append(m.completeCalls, tokenID)
}

func (m *mockPool) NotifyUnauthorized() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unauthorizedCnt++
}

func (m *mockPool) completed() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.completeCalls...)
}

func (m *mockPool) unauthorized() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unauthorizedCnt
}

func defaultToken() *TokenInfo {
	return &TokenInfo{
		ID:          7,
		AccessToken: "test-access-token",
		UserAgent:   "test-ua/1.0",
		Headers:     map[string]string{"X-Test-Extra": "yes"},
	}
}

// rewriteTransport routes the client's https://oauth.reddit.com requests to
// the local httptest server while preserving path + query.
type rewriteTransport struct {
	scheme string
	host   string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.scheme
	req.URL.Host = t.host
	return http.DefaultTransport.RoundTrip(req)
}

// newTestClient builds a Client wired to a test server. The Client struct is
// constructed directly (in-package) so no real OAuth pool or network is used.
func newTestClient(t *testing.T, pool TokenProvider, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	httpClient := &http.Client{
		Transport: &rewriteTransport{scheme: u.Scheme, host: u.Host},
		// Mirror NewClient: redirects are followed manually by doRequest.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		pool:       pool,
		httpClient: httpClient,
		etags:      newETagCache(2000),
	}
}

// --- checkAPIError ---

func TestCheckAPIError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"clean", `{"data":{"children":[]}}`, nil},
		{"suspended compact", `{"is_suspended":true}`, ErrSuspended},
		{"suspended spaced", `{"is_suspended": true}`, ErrSuspended},
		{"quarantined", `{"reason":"quarantined"}`, ErrQuarantined},
		{"private", `{"reason":"private"}`, ErrPrivate},
		{"banned", `{"reason":"banned"}`, ErrBanned},
		{"gated", `{"reason":"gated"}`, ErrGated},
		{"unauthorized", `{"message":"Unauthorized","error":401}`, ErrUnauthorized},
		{"unrelated reason", `{"reason":"deleted"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkAPIError([]byte(tc.body)); got != tc.want {
				t.Errorf("checkAPIError(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// --- doRequest: token handling ---

func TestDoRequest_NoToken(t *testing.T) {
	pool := &mockPool{token: nil}
	// Handler must never be reached.
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not be hit when no token is available")
		w.WriteHeader(http.StatusOK)
	}))

	_, _, err := c.doRequest(context.Background(), "/r/test/hot.json")
	if err != ErrNoTokenAvailable {
		t.Fatalf("err = %v, want ErrNoTokenAvailable", err)
	}
	if len(pool.completed()) != 0 {
		t.Errorf("OnRequestComplete called %d times, want 0", len(pool.completed()))
	}
}

func TestDoRequest_SetsAuthHeadersAndReportsCompletion(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	var gotAuth, gotUA, gotCookie, gotExtra string
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotCookie = r.Header.Get("Cookie")
		gotExtra = r.Header.Get("X-Test-Extra")
		w.Write([]byte(`{"ok":1}`))
	}))

	body, _, err := c.doRequest(context.Background(), "/api/v1/me.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != `{"ok":1}` {
		t.Errorf("body = %q", body)
	}
	if gotAuth != "Bearer test-access-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotUA != "test-ua/1.0" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotCookie != quarantineCookie {
		t.Errorf("Cookie = %q, want quarantine opt-in cookie", gotCookie)
	}
	if gotExtra != "yes" {
		t.Errorf("custom token header X-Test-Extra = %q, want yes", gotExtra)
	}
	if calls := pool.completed(); len(calls) != 1 || calls[0] != 7 {
		t.Errorf("OnRequestComplete calls = %v, want [7]", calls)
	}
}

// --- doRequest: error status mapping ---

func TestDoRequest_Unauthorized401(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Unauthorized"}`))
	}))

	_, _, err := c.doRequest(context.Background(), "/api/v1/me.json")
	if err != ErrUnauthorized {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if pool.unauthorized() != 1 {
		t.Errorf("NotifyUnauthorized called %d times, want 1", pool.unauthorized())
	}
}

func TestDoRequest_RateLimited429(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"Too Many Requests"}`))
	}))

	_, _, err := c.doRequest(context.Background(), "/r/test/hot.json")
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestDoRequest_RateLimited403WithRetryAfter(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"blocked"}`))
	}))

	_, _, err := c.doRequest(context.Background(), "/r/test/hot.json")
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited for 403+Retry-After", err)
	}
}

func TestDoRequest_EmptyBodyTreatedAsRateLimited(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 but no body
	}))

	_, _, err := c.doRequest(context.Background(), "/r/test/hot.json")
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited for empty 200 body", err)
	}
}

func TestDoRequest_APIErrorBody(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"reason":"private","kind":"t5"}`))
	}))

	_, _, err := c.doRequest(context.Background(), "/r/secret/about.json")
	if err != ErrPrivate {
		t.Fatalf("err = %v, want ErrPrivate", err)
	}
}

// --- doRequest: ETag conditional requests ---

func TestDoRequest_ETagConditional(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	const etag = `"snapshot-v1"`
	const payload = `{"kind":"Listing","data":{"children":[]}}`

	var hits int
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(payload))
	}))

	// First request: full 200, body cached against the ETag.
	body1, _, err := c.doRequest(context.Background(), "/r/test/hot.json")
	if err != nil {
		t.Fatalf("first request error: %v", err)
	}
	if string(body1) != payload {
		t.Errorf("first body = %q", body1)
	}

	// Second request to the same path: client sends If-None-Match, server
	// answers 304, client must return the cached body.
	body2, _, err := c.doRequest(context.Background(), "/r/test/hot.json")
	if err != nil {
		t.Fatalf("second request error: %v", err)
	}
	if string(body2) != payload {
		t.Errorf("304 path body = %q, want cached payload", body2)
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2", hits)
	}
}

// --- doRequest: redirect following ---

func TestDoRequest_FollowsRedirect(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	const payload = `{"kind":"Listing","data":{"children":[]}}`

	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/r/oldname/hot.json":
			http.Redirect(w, r, "/r/newname/hot.json", http.StatusMovedPermanently)
		case r.URL.Path == "/r/newname/hot.json":
			if r.URL.Query().Get("raw_json") != "1" {
				t.Errorf("redirect target missing raw_json=1: %s", r.URL.RawQuery)
			}
			w.Write([]byte(payload))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))

	body, _, err := c.doRequest(context.Background(), "/r/oldname/hot.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != payload {
		t.Errorf("body after redirect = %q", body)
	}
	// One OnRequestComplete per hop (redirect + final).
	if calls := pool.completed(); len(calls) != 2 {
		t.Errorf("OnRequestComplete calls = %v, want 2 hops", calls)
	}
}

// --- high-level fetch helpers ---

func TestFetchSubreddit_Success(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	listing := `{"kind":"Listing","data":{"before":null,"after":"t3_next","children":[
		{"kind":"t3","data":{"id":"p1","title":"Hello","subreddit":"golang","is_self":true,"score":5,"created_utc":1700000000}}
	]}}`
	var gotPath string
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Write([]byte(listing))
	}))

	posts, before, after, err := c.FetchSubreddit(context.Background(), "golang", "new", "", 25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(posts) != 1 || posts[0].ID != "p1" {
		t.Fatalf("posts = %+v", posts)
	}
	if before != "" || after != "t3_next" {
		t.Errorf("cursors before=%q after=%q", before, after)
	}
	if gotPath != "/r/golang/new.json?raw_json=1&include_over_18=on&limit=25" {
		t.Errorf("request path = %q", gotPath)
	}
}

func TestFetchSubreddit_PropagatesError(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"slow down"}`))
	}))

	_, _, _, err := c.FetchSubreddit(context.Background(), "golang", "new", "", 25)
	if err != ErrRateLimited {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// --- boundary tests ---

func TestDoRequest_ContextCancelled(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the request is even sent

	_, _, err := c.doRequest(ctx, "/r/x/hot.json")
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestDoRequest_MultiHopRedirect(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	const payload = `{"kind":"Listing","data":{"children":[]}}`
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop0.json":
			http.Redirect(w, r, "/hop1.json", http.StatusFound)
		case "/hop1.json":
			http.Redirect(w, r, "/hop2.json", http.StatusFound)
		case "/hop2.json":
			w.Write([]byte(payload))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))

	body, _, err := c.doRequest(context.Background(), "/hop0.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != payload {
		t.Errorf("body after 3-hop redirect = %q", body)
	}
	// One completion callback per hop (hop0, hop1, hop2).
	if calls := pool.completed(); len(calls) != 3 {
		t.Errorf("OnRequestComplete calls = %v, want 3 hops", calls)
	}
}

// TestDoRequest_RedirectLoopIsBounded guards against the redirect-loop stack
// overflow: a server that endlessly redirects to itself must yield an error,
// not unbounded recursion.
func TestDoRequest_RedirectLoopIsBounded(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	var hits int
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/loop.json", http.StatusFound)
	}))

	_, _, err := c.doRequest(context.Background(), "/loop.json")
	if err == nil {
		t.Fatal("expected an error for an unbounded redirect loop")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("err = %v, want a 'too many redirects' error", err)
	}
	// 1 initial request + maxRedirects follows, then it must stop.
	if hits != maxRedirects+1 {
		t.Errorf("server hit %d times, want %d (bounded follow count)", hits, maxRedirects+1)
	}
}

// TestClient_ConcurrentFetch hammers the client (and its sync.Map-backed ETag
// cache) from many goroutines. Run with -race to catch data races in the
// cache and token-completion paths. All traffic stays on the local test
// server — no requests reach Reddit.
func TestClient_ConcurrentFetch(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	const etag = `"snapshot"`
	const listing = `{"kind":"Listing","data":{"before":null,"after":null,"children":[]}}`
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write([]byte(listing))
	}))

	const goroutines = 48
	const iters = 150
	subs := []string{"golang", "rust", "python", "elixir"}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				sub := subs[(g+i)%len(subs)]
				if _, _, _, err := c.FetchSubreddit(context.Background(), sub, "hot", "", 25); err != nil {
					t.Errorf("FetchSubreddit(%s): %v", sub, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if n := len(pool.completed()); n != goroutines*iters {
		t.Errorf("OnRequestComplete calls = %d, want %d", n, goroutines*iters)
	}
}

func TestProbe_ParsesRateLimitHeaders(t *testing.T) {
	pool := &mockPool{token: defaultToken()}
	c := newTestClient(t, pool, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "297.0")
		w.Header().Set("X-Ratelimit-Reset", "412.0")
		w.Header().Set("X-Ratelimit-Used", "303")
		w.Write([]byte(`{"name":"someuser"}`))
	}))

	info, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Remaining != 297.0 {
		t.Errorf("Remaining = %v, want 297", info.Remaining)
	}
	if info.Reset != 412 {
		t.Errorf("Reset = %d, want 412 (float truncated)", info.Reset)
	}
	if info.Used != 303 {
		t.Errorf("Used = %d, want 303", info.Used)
	}
}
