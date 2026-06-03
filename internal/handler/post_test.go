package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/oauth"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

// In-memory fakes for the servePost dependency chain. They satisfy the narrow
// interfaces in deps.go and let the removed-fallback path be exercised
// end-to-end without standing up SQLite / Redis / a Reddit-shaped HTTP server.

type fakePostStore struct {
	mu                sync.Mutex
	rows              map[string]*store.StoredPost
	getCalls          int
	markRemovedCalls  []string
	saveHTMLCalls     []string
}

func newFakePostStore() *fakePostStore {
	return &fakePostStore{rows: map[string]*store.StoredPost{}}
}

func (f *fakePostStore) put(p *store.StoredPost) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *p
	f.rows[p.URLPath] = &cp
}

func (f *fakePostStore) Get(urlPath string) (*store.StoredPost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	r, ok := f.rows[urlPath]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (f *fakePostStore) MarkUpstreamRemoved(urlPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markRemovedCalls = append(f.markRemovedCalls, urlPath)
	if r, ok := f.rows[urlPath]; ok {
		r.UpstreamRemoved = true
	}
	return nil
}

func (f *fakePostStore) SaveHTML(urlPath string, html []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveHTMLCalls = append(f.saveHTMLCalls, urlPath)
	return nil
}

// Methods below are part of postStorer but not exercised by servePost; they
// stay as harmless stubs so the fake satisfies the full interface.
func (f *fakePostStore) ArchiveSearch(store.ArchiveSearchOpts) ([]*store.StoredPost, int64, error) {
	return nil, 0, nil
}
func (f *fakePostStore) ArchivedSubsByTop(int) ([]store.ArchivedSub, error)        { return nil, nil }
func (f *fakePostStore) ArchivedSubsAlphabetical() ([]store.ArchivedSub, error)    { return nil, nil }
func (f *fakePostStore) ArchivedSubsByNew(int) ([]store.ArchivedSub, error)        { return nil, nil }
func (f *fakePostStore) DetectNSFWForSubs([]string) (map[string]bool, error)       { return nil, nil }
func (f *fakePostStore) ListBySubreddit(string, int, int, bool) ([]*store.StoredPost, error) {
	return nil, nil
}
func (f *fakePostStore) CountBySubreddit(string, bool) (int64, error)              { return 0, nil }
func (f *fakePostStore) ListHomepage(string, store.ArchiveSearchOpts) ([]*store.StoredPost, error) {
	return nil, nil
}
func (f *fakePostStore) RandomWalk(store.ArchiveSearchOpts, bool, float64, float64, int) ([]*store.StoredPost, float64, bool, error) {
	return nil, 0, false, nil
}
func (f *fakePostStore) Reshuffle() error                                            { return nil }
func (f *fakePostStore) SubredditCounts([]string) (map[string]int, error)            { return nil, nil }
func (f *fakePostStore) Count() (int64, error)                                       { return 0, nil }
func (f *fakePostStore) SubredditCount() (int64, error)                              { return 0, nil }
func (f *fakePostStore) SubredditStats(int, int) ([]store.SubredditStat, error)      { return nil, nil }
func (f *fakePostStore) DistinctSubreddits() ([]string, error)                       { return nil, nil }

type fakeCommentStore struct {
	rows map[string]*store.StoredComments
}

func (f *fakeCommentStore) GetLatest(postURLPath string) (*store.StoredComments, error) {
	if f.rows == nil {
		return nil, nil
	}
	r, ok := f.rows[postURLPath]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

type fakeCache struct{}

func (fakeCache) GetHTML(context.Context, string) ([]byte, error)               { return nil, nil }
func (fakeCache) PutHTML(context.Context, string, []byte, time.Duration) error  { return nil }
func (fakeCache) InvalidateHTMLPrefix(context.Context, string) error            { return nil }
func (fakeCache) InvalidateAllHTML(context.Context) error                       { return nil }
func (fakeCache) Get(context.Context, string) (string, error)                   { return "", nil }
func (fakeCache) Set(context.Context, string, string, time.Duration) error      { return nil }

type fakeRedditClient struct {
	mu         sync.Mutex
	fetchCalls int32
	post       reddit.Post
	comments   []reddit.Comment
	err        error
}

func (f *fakeRedditClient) FetchPost(_ context.Context, _, _, _ string) (reddit.Post, []reddit.Comment, error) {
	atomic.AddInt32(&f.fetchCalls, 1)
	return f.post, f.comments, f.err
}

func (f *fakeRedditClient) FetchSubreddit(context.Context, string, string, string, string, int) ([]reddit.Post, string, string, error) {
	return nil, "", "", nil
}
func (f *fakeRedditClient) FetchSearch(context.Context, string, string, string, string, string, int) ([]reddit.Post, []reddit.Subreddit, string, error) {
	return nil, nil, "", nil
}
func (f *fakeRedditClient) FetchSubredditAbout(context.Context, string) (reddit.Subreddit, error) {
	return reddit.Subreddit{}, nil
}
func (f *fakeRedditClient) FetchUser(context.Context, string, string, string, string) (reddit.User, []reddit.Post, []reddit.Comment, error) {
	return reddit.User{}, nil, nil, nil
}

type fakeTokenSource struct{ available bool }

func (f fakeTokenSource) HasAvailableTokens() bool                  { return f.available }
func (fakeTokenSource) WaitForUserAgent(context.Context) string     { return "" }
func (fakeTokenSource) EarliestReset() (int, int)                   { return 0, 0 }
func (fakeTokenSource) RemainingBudget(context.Context) (int, error) { return 0, nil }
func (fakeTokenSource) TokenStatuses() []oauth.TokenStatusInfo      { return nil }
func (fakeTokenSource) WindowInfo() (time.Time, int, int)           { return time.Time{}, 0, 0 }

type fakeArchiver struct {
	mu              sync.Mutex
	done            sync.WaitGroup
	postCalls       []reddit.Post
	commentCalls    int
}

func (f *fakeArchiver) ArchivePost(post *reddit.Post, _, _ string) {
	f.mu.Lock()
	f.postCalls = append(f.postCalls, *post)
	f.mu.Unlock()
	f.done.Done()
}
func (f *fakeArchiver) ArchiveComments(string, []reddit.Comment) {
	f.mu.Lock()
	f.commentCalls++
	f.mu.Unlock()
	f.done.Done()
}
func (f *fakeArchiver) ArchivePosts([]reddit.Post, string, string)    {}
func (f *fakeArchiver) ArchiveSubreddit(*reddit.Subreddit)            {}
func (f *fakeArchiver) SetControlFromString(string)                   {}

func (f *fakeArchiver) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.postCalls)
}

// newTestHandler builds a Handler wired with in-memory fakes. The renderer is
// real (templ output is the only way to verify the Time Machine badge is on
// the page). hr is left nil so shouldDegrade short-circuits past the cooldown
// check; the token source is "available" by default.
func newTestHandler(t *testing.T) (*Handler, *fakePostStore, *fakeCommentStore, *fakeRedditClient, *fakeArchiver) {
	t.Helper()
	rend, err := render.New(config.RenderConfig{BrandName: "TestBrand", ShowArchiveBadge: true})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	ps := newFakePostStore()
	cs := &fakeCommentStore{rows: map[string]*store.StoredComments{}}
	rc := &fakeRedditClient{}
	arc := &fakeArchiver{}
	h := &Handler{
		cache:          fakeCache{},
		renderer:       rend,
		redditCli:      rc,
		oauthHolder:    fakeTokenSource{available: true},
		postStore:      ps,
		commentStore:   cs,
		archiver:       arc,
		cfg:            &config.Config{Render: config.RenderConfig{BrandName: "TestBrand"}},
		siteDefaults:   map[string]string{},
		upstreamFlight: newSingleFlight(),
	}
	return h, ps, cs, rc, arc
}

// Scenario A: upstream returns Removed=true and a prior archive exists. The
// fallback chain must flip UpstreamRemoved on the row exactly once, never
// save HTML on top of the archive, and the final response must come from the
// archive path with the Time Machine badge rendered.
func TestServePost_RemovedUpstreamWithPriorArchive(t *testing.T) {
	h, ps, _, rc, arc := newTestHandler(t)

	// The renderPostFallback MarkUpstreamRemoved lookup keys by "/r/{sub}/
	// comments/{id}/" (no title segment) — see post.go:209. Seed the prior
	// archive at that same key so the sticky verdict can be flipped on it.
	urlPath := "/r/golang/comments/abc/"
	ps.put(&store.StoredPost{
		URLPath:   urlPath,
		Subreddit: "golang",
		PostID:    "abc",
		Title:     "Original Title",
		// Note: archived JSON pre-dates the Removed field, so it deserializes
		// to false. The Time Machine badge must still appear because the row's
		// UpstreamRemoved=true is OR'd into post.Removed at render time.
		JSONData:  []byte(`{"id":"abc","title":"Original Title","permalink":"` + urlPath + `","community":"golang","author":{"name":"poster"}}`),
		FirstSeen: time.Now().Add(-24 * time.Hour),
	})

	rc.post = reddit.Post{ID: "abc", Permalink: urlPath, Removed: true, Community: "golang"}
	rc.comments = nil
	// renderPostFallback fires a goroutine into ArchivePost + ArchiveComments
	// for any successful fetch — wait for both before asserting.
	arc.done.Add(2)

	req := httptest.NewRequest("GET", "/r/golang/comments/abc", nil)
	w := httptest.NewRecorder()
	h.servePost(w, req, "golang", "abc")

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if src := resp.Header.Get("X-Source"); src != "archive" {
		t.Errorf("X-Source = %q, want archive", src)
	}
	body := w.Body.String()
	if !strings.Contains(body, "time-machine-inline") {
		t.Errorf("body should contain time-machine-inline badge")
	}
	if len(ps.markRemovedCalls) != 1 || ps.markRemovedCalls[0] != urlPath {
		t.Errorf("MarkUpstreamRemoved calls = %v, want [%s]", ps.markRemovedCalls, urlPath)
	}
	if len(ps.saveHTMLCalls) != 0 {
		t.Errorf("SaveHTML must not be called on archive fallback, got %v", ps.saveHTMLCalls)
	}
	if atomic.LoadInt32(&rc.fetchCalls) != 1 {
		t.Errorf("FetchPost calls = %d, want 1", rc.fetchCalls)
	}

	arc.done.Wait()
}

// Scenario B: upstream fails (HR/network/quota error) and there is no prior
// archive — servePost must fall through to serveDegradeMiss without touching
// UpstreamRemoved or persisting anything.
func TestServePost_UpstreamFailureNoArchive(t *testing.T) {
	h, ps, _, rc, arc := newTestHandler(t)

	rc.err = errors.New("upstream unreachable")

	req := httptest.NewRequest("GET", "/r/golang/comments/zzz/missing", nil)
	w := httptest.NewRecorder()
	h.servePost(w, req, "golang", "zzz")

	resp := w.Result()
	// serveDegradeMiss redirects to /fuckreddit for transient reasons (the
	// upstream_disabled site-default isn't set here). 307 confirms we landed
	// on the no-archive-no-upstream terminal branch.
	if resp.StatusCode != 307 {
		t.Fatalf("status = %d, want 307 (serveDegradeMiss redirect)", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/fuckreddit") {
		t.Errorf("Location = %q, want /fuckreddit prefix", loc)
	}
	if len(ps.markRemovedCalls) != 0 {
		t.Errorf("MarkUpstreamRemoved must not be called; got %v", ps.markRemovedCalls)
	}
	if len(ps.saveHTMLCalls) != 0 {
		t.Errorf("SaveHTML must not be called; got %v", ps.saveHTMLCalls)
	}
	if arc.postCount() != 0 {
		t.Errorf("ArchivePost must not be called; got %d", arc.postCount())
	}
}

// Scenario C: a sticky UpstreamRemoved=true row in postStore must short-
// circuit the entire upstream chain — no FetchPost, no MarkUpstreamRemoved —
// and serve the archive directly with the Time Machine badge. The archived
// JSON's Removed field is deliberately false so the assertion proves the OR
// at renderPostFromArchive:264 is the only thing keeping the badge on.
func TestServePost_StickyUpstreamRemovedSkipsUpstream(t *testing.T) {
	h, ps, _, rc, arc := newTestHandler(t)

	urlPath := "/r/golang/comments/sticky/"
	ps.put(&store.StoredPost{
		URLPath:         urlPath,
		Subreddit:       "golang",
		PostID:          "sticky",
		Title:           "Old Good Title",
		JSONData:        []byte(`{"id":"sticky","title":"Old Good Title","permalink":"` + urlPath + `","community":"golang","author":{"name":"poster"},"removed":false}`),
		FirstSeen:       time.Now().Add(-72 * time.Hour),
		UpstreamRemoved: true,
	})

	req := httptest.NewRequest("GET", "/r/golang/comments/sticky", nil)
	w := httptest.NewRecorder()
	h.servePost(w, req, "golang", "sticky")

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if src := resp.Header.Get("X-Source"); src != "archive" {
		t.Errorf("X-Source = %q, want archive", src)
	}
	if atomic.LoadInt32(&rc.fetchCalls) != 0 {
		t.Errorf("FetchPost must not be called for sticky-removed row; got %d", rc.fetchCalls)
	}
	if len(ps.markRemovedCalls) != 0 {
		t.Errorf("MarkUpstreamRemoved must not be called when already sticky; got %v", ps.markRemovedCalls)
	}
	if arc.postCount() != 0 {
		t.Errorf("ArchivePost must not be called when bypassing upstream; got %d", arc.postCount())
	}
	body := w.Body.String()
	if !strings.Contains(body, "time-machine-inline") {
		t.Errorf("body should contain time-machine-inline badge (driven by row UpstreamRemoved OR, JSON Removed=false)")
	}
}
