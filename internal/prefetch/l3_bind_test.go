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
// archived.
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

// l3TestPost builds a StoredPost whose JSONData round-trips through the
// json.Unmarshal → ExtractMediaItems path. media=true gives it an image URL
// (one media item → media branch); media=false leaves it a bare self post
// (zero media items → no-media branch). numComments populates the raw
// Comments[1] field numCommentsOfJSON reads for the min-comments waterline.
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

// newL2WaveScheduler wires a Scheduler that drives runL2Wave entirely through
// in-memory seams: pendingMediaFn supplies the wave's posts, l3FetchFn records
// any L3 hit (so a test can assert L2 never touches L3), and a real dispatchLoop
// runs with a near-zero cooldown so submit() completes within the test's
// lifetime. postStore/cli/archiver stay nil — the seams and nil-guards keep
// every path off the DB and network.
func newL2WaveScheduler(t *testing.T, depth string, posts []*store.StoredPost) (*Scheduler, *l3Recorder, *mockDownloader) {
	t.Helper()
	rec := &l3Recorder{}
	dl := &mockDownloader{}
	s := &Scheduler{
		settings:         &mockSettings{data: map[string]string{"prefetch_default_depth": depth}},
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

// newL3WaveScheduler wires a Scheduler that drives runL3Wave through in-memory
// seams: l3CandidatesFn supplies the eligible posts (decoupled from any media
// state), l3FetchFn records which posts had their comments fetched. No media
// downloader interaction — L3 is comments only.
func newL3WaveScheduler(t *testing.T, minComments string, candidates []*store.StoredPost) (*Scheduler, *l3Recorder) {
	t.Helper()
	data := map[string]string{}
	if minComments != "" {
		data["prefetch_l3_min_comments"] = minComments
	}
	rec := &l3Recorder{}
	s := &Scheduler{
		settings:         &mockSettings{data: data},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 600},
		Events:           NewEventLog(100),
		queue:            make(chan *workItem, 8),
		dispatchCooldown: func() time.Duration { return time.Millisecond },
		l3CandidatesFn: func(_ /*sub*/, _ /*cycle*/, _ /*prev*/ string, _ /*limit*/, _ /*min*/ int) ([]*store.StoredPost, error) {
			return candidates, nil
		},
		l3FetchFn: rec.fn,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.dispatchLoop(ctx)
	return s, rec
}

// TestRunL2Wave_NeverFetchesL3 pins the post-decoupling contract: L2 is pure
// CDN cache. In every depth that runs media (l2 and l2+l3) it downloads media
// but never touches comments — L3 is a separate, self-standing layer. This
// covers both a media post and a bare text post.
func TestRunL2Wave_NeverFetchesL3(t *testing.T) {
	for _, depth := range []string{"l2", "l2+l3"} {
		t.Run(depth, func(t *testing.T) {
			posts := []*store.StoredPost{
				l3TestPost(t, "media1", true, 50),
				l3TestPost(t, "text1", false, 50),
			}
			s, rec, dl := newL2WaveScheduler(t, depth, posts)

			if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
				t.Fatalf("runL2Wave returned error: %v", err)
			}

			if got := rec.got(); len(got) != 0 {
				t.Errorf("depth=%s: runL2Wave fetched L3 for %v, want none (L2 is media-only)", depth, got)
			}
			if calls := dl.getCalls(); len(calls) != 1 {
				t.Errorf("depth=%s: media downloads = %v, want exactly 1 (the image post only)", depth, calls)
			}
		})
	}
}

// TestRunL2Wave_DownloadsCommentMedia pins that L2 — the NP media layer —
// downloads inline images pasted into a post's archived comment bodies, even
// for a bare text post that carries no structured media of its own. The comment
// images reach L2 via the commentMediaFn seam (production: archiver.CommentMediaURLs).
func TestRunL2Wave_DownloadsCommentMedia(t *testing.T) {
	post := l3TestPost(t, "text1", false, 50) // self post, no structured media
	s, _, dl := newL2WaveScheduler(t, "l2", []*store.StoredPost{post})
	commentImg := "https://preview.redd.it/c1.png?width=640&s=sig"
	s.commentMediaFn = func(urlPath string) []string {
		if urlPath == post.URLPath {
			return []string{commentImg}
		}
		return nil
	}

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	calls := dl.getCalls()
	if len(calls) != 1 || calls[0] != commentImg {
		t.Fatalf("L2 media downloads = %v, want exactly the comment image %q", calls, commentImg)
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
	s, rec, dl := newL2WaveScheduler(t, "none", posts)

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

// TestRunL3Wave_FetchesAllCandidates is the core of the decoupled L3 layer: the
// candidate query (not the media queue) decides what L3 fetches, so both media
// and text posts have their comments archived regardless of any media state.
func TestRunL3Wave_FetchesAllCandidates(t *testing.T) {
	cands := []*store.StoredPost{
		l3TestPost(t, "media1", true, 5),
		l3TestPost(t, "text1", false, 5),
	}
	s, rec := newL3WaveScheduler(t, "", cands)

	if err := s.runL3Wave(context.Background(), "day", "selfhosted", 25, "L3:day:selfhosted:1", 1); err != nil {
		t.Fatalf("runL3Wave returned error: %v", err)
	}

	got := rec.got()
	want := []string{"media1", "text1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("L3 fetched %v, want %v (candidates drive L3, independent of media)", got, want)
	}
}

// TestRunL3Wave_MinCommentsGate confirms the defensive in-wave waterline gates
// candidates: only posts at/over the threshold reach the fetch, the rest are
// frozen out regardless of media presence.
func TestRunL3Wave_MinCommentsGate(t *testing.T) {
	cands := []*store.StoredPost{
		l3TestPost(t, "media_low", true, 3),
		l3TestPost(t, "media_hi", true, 20),
		l3TestPost(t, "text_low", false, 3),
		l3TestPost(t, "text_hi", false, 20),
		l3TestPost(t, "text_exact", false, 10), // == threshold → included
	}
	s, rec := newL3WaveScheduler(t, "10", cands)

	if err := s.runL3Wave(context.Background(), "day", "selfhosted", 25, "L3:day:selfhosted:1", 1); err != nil {
		t.Fatalf("runL3Wave returned error: %v", err)
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

// TestRunL3Wave_NoCandidatesSkips pins that an empty candidate set fetches
// nothing and returns cleanly.
func TestRunL3Wave_NoCandidatesSkips(t *testing.T) {
	s, rec := newL3WaveScheduler(t, "", nil)

	if err := s.runL3Wave(context.Background(), "day", "selfhosted", 25, "L3:day:selfhosted:1", 1); err != nil {
		t.Fatalf("runL3Wave returned error: %v", err)
	}
	if got := rec.got(); len(got) != 0 {
		t.Errorf("empty candidate set fetched %v, want none", got)
	}
}
