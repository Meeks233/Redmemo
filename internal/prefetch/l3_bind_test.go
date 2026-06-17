package prefetch

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

var errFakeDownload = errors.New("fake download failure")

// l3Recorder captures every (sub, postID) pair that reaches the L3 fetch
// pipeline via the l3FetchFn seam. It replaces the live Reddit fetch + archive
// write so a wave test can assert exactly which posts had their comments
// archived — independent of whether the post carried downloadable media.
type l3Recorder struct {
	mu    sync.Mutex
	posts []string
}

func (r *l3Recorder) fn(_ context.Context, _ /*sub*/, postID, _ /*urlPath*/ string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, postID)
	return 7, nil
}

func (r *l3Recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.posts...)
	sort.Strings(out)
	return out
}

// l3TestPost builds a StoredPost whose JSONData round-trips through
// runL2Wave's json.Unmarshal → ExtractMediaItems path. media=true gives it an
// image URL (one media item → media branch); media=false leaves it a bare
// self post (zero media items → no-media branch). numComments populates the
// raw Comments[1] field numCommentsOf reads for the min-comments waterline.
func l3TestPost(t *testing.T, id string, media bool, numComments int) *store.StoredPost {
	t.Helper()
	urlPath := "/r/selfhosted/comments/" + id + "/title/"
	p := reddit.Post{
		ID:        id,
		Permalink: urlPath,
		Comments:  [2]string{strconv.Itoa(numComments), strconv.Itoa(numComments)},
	}
	if media {
		p.PostType = "image"
		p.Media = reddit.Media{URL: "https://i.redd.it/" + id + ".jpg"}
	} else {
		p.PostType = "self"
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal test post %s: %v", id, err)
	}
	return &store.StoredPost{URLPath: urlPath, PostID: id, Subreddit: "selfhosted", JSONData: data}
}

// newL3WaveScheduler wires a Scheduler that drives runL2Wave entirely through
// in-memory seams: pendingMediaFn supplies the wave's posts, l3FetchFn records
// L3 hits, and a real dispatchLoop runs with a near-zero cooldown so submit()
// completes within the test's lifetime. postStore/cli/archiver stay nil — the
// seams and nil-guards keep every path off the DB and network.
func newL3WaveScheduler(t *testing.T, depth, minComments string, posts []*store.StoredPost) (*Scheduler, *l3Recorder, *mockDownloader) {
	t.Helper()
	data := map[string]string{"prefetch_default_depth": depth}
	if minComments != "" {
		data["prefetch_l3_min_comments"] = minComments
	}
	rec := &l3Recorder{}
	dl := &mockDownloader{}
	s := &Scheduler{
		settings:         &mockSettings{data: data},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 600},
		media:            dl,
		Events:           NewEventLog(100),
		queue:            make(chan *workItem, 8),
		dispatchCooldown: func() time.Duration { return time.Millisecond },
		pendingMediaFn: func(_ string, _ int) ([]*store.StoredPost, error) {
			return posts, nil
		},
		l3FetchFn: rec.fn,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.dispatchLoop(ctx)
	return s, rec, dl
}

// TestRunL2Wave_BindFetchesCommentsForTextPosts is the regression test for the
// L3-binding bug: in bind mode (depth=l2+l3) the comment fetch used to fire
// ONLY for posts that completed a media download, so every text/self post —
// the majority on discussion subs — silently never had comments archived.
// After the fix both the media post and the bare text post must reach L3.
func TestRunL2Wave_BindFetchesCommentsForTextPosts(t *testing.T) {
	posts := []*store.StoredPost{
		l3TestPost(t, "media1", true, 5),
		l3TestPost(t, "text1", false, 5),
	}
	s, rec, dl := newL3WaveScheduler(t, "l2+l3", "", posts)

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	got := rec.got()
	want := []string{"media1", "text1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("L3 fetched posts = %v, want %v (text posts must NOT be excluded in bind mode)", got, want)
	}
	// The media post still downloads its one image; the text post downloads nothing.
	if calls := dl.getCalls(); len(calls) != 1 {
		t.Errorf("media downloads = %v, want exactly 1 (the image post only)", calls)
	}
}

// TestRunL2Wave_NonBindNeverFetchesL3 pins the depth=l2 contract: media is
// archived but comments are never touched, for media AND text posts alike.
func TestRunL2Wave_NonBindNeverFetchesL3(t *testing.T) {
	posts := []*store.StoredPost{
		l3TestPost(t, "media1", true, 50),
		l3TestPost(t, "text1", false, 50),
	}
	s, rec, dl := newL3WaveScheduler(t, "l2", "", posts)

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	if got := rec.got(); len(got) != 0 {
		t.Errorf("depth=l2 fetched L3 for %v, want none", got)
	}
	if calls := dl.getCalls(); len(calls) != 1 {
		t.Errorf("media downloads = %v, want exactly 1", calls)
	}
}

// TestRunL2Wave_MinCommentsThresholdGatesBothTypes confirms the min-comments
// waterline filters media and text posts identically: only posts at/over the
// threshold reach L3, the rest are frozen out regardless of media presence.
func TestRunL2Wave_MinCommentsThresholdGatesBothTypes(t *testing.T) {
	posts := []*store.StoredPost{
		l3TestPost(t, "media_low", true, 3),
		l3TestPost(t, "media_hi", true, 20),
		l3TestPost(t, "text_low", false, 3),
		l3TestPost(t, "text_hi", false, 20),
		l3TestPost(t, "text_exact", false, 10), // == threshold → included
	}
	s, rec, _ := newL3WaveScheduler(t, "l2+l3", "10", posts)

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	got := rec.got()
	want := []string{"media_hi", "text_exact", "text_hi"}
	if len(got) != len(want) {
		t.Fatalf("L3 fetched %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("L3 fetched %v, want %v (threshold must gate text and media alike)", got, want)
			break
		}
	}
}

// TestRunL2Wave_DepthNoneSkipsEverything pins that a sub resolved to depth=none
// downloads no media and fetches no comments even if runL2Wave is invoked
// directly (a mid-cycle settings flip is the only way to reach it).
func TestRunL2Wave_DepthNoneSkipsEverything(t *testing.T) {
	posts := []*store.StoredPost{
		l3TestPost(t, "media1", true, 50),
		l3TestPost(t, "text1", false, 50),
	}
	s, rec, dl := newL3WaveScheduler(t, "none", "", posts)

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	if got := rec.got(); len(got) != 0 {
		t.Errorf("depth=none fetched L3 for %v, want none", got)
	}
	if calls := dl.getCalls(); len(calls) != 0 {
		t.Errorf("depth=none downloaded %v, want none", calls)
	}
}

// TestRunL2Wave_FailedMediaDownloadSkipsL3 confirms the bind L3 stays tied to a
// SUCCESSFUL media archive for media posts: a post whose download fails is not
// marked done and must not have its comments fetched (it will be retried in a
// later wave). Text posts are unaffected since they never download.
func TestRunL2Wave_FailedMediaDownloadSkipsL3(t *testing.T) {
	posts := []*store.StoredPost{l3TestPost(t, "media1", true, 50)}
	s, rec, dl := newL3WaveScheduler(t, "l2+l3", "", posts)
	dl.err = errFakeDownload

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	if got := rec.got(); len(got) != 0 {
		t.Errorf("failed media post fetched L3 for %v, want none (download failed → no bind)", got)
	}
	if calls := dl.getCalls(); len(calls) != 1 {
		t.Errorf("expected exactly 1 (failed) download attempt, got %v", calls)
	}
}
