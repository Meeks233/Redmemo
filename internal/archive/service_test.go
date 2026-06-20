package archive

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

// fakePostRepo is an in-memory postRepo satisfying the archive service's needs.
// It records every Save call so a test can assert the archive layer *did not*
// overwrite a previously-good copy when upstream reports a removed verdict.
type fakePostRepo struct {
	rows          map[string]*store.StoredPost
	saves         []*store.StoredPost
	markedRemoved []string
}

func newFakePostRepo() *fakePostRepo {
	return &fakePostRepo{rows: map[string]*store.StoredPost{}}
}

func (f *fakePostRepo) Save(p *store.StoredPost) error {
	cp := *p
	f.saves = append(f.saves, &cp)
	f.rows[p.URLPath] = &cp
	return nil
}

func (f *fakePostRepo) Get(urlPath string) (*store.StoredPost, error) {
	if r, ok := f.rows[urlPath]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (f *fakePostRepo) MarkUpstreamRemoved(urlPath string) error {
	f.markedRemoved = append(f.markedRemoved, urlPath)
	if r, ok := f.rows[urlPath]; ok {
		r.UpstreamRemoved = true
	}
	return nil
}

func newServiceWithFake(repo *fakePostRepo) *Service {
	return &Service{
		postStore: repo,
		nsfwKnown: map[string]bool{},
	}
}

// fakeCommentRepo is the comment-side analogue of fakePostRepo. It returns a
// caller-supplied prior tree from GetLatest so the removed-bodies merge guard
// in ArchiveComments can be unit-tested in isolation.
type fakeCommentRepo struct {
	prior *store.StoredComments
	saved *store.StoredComments
}

func (f *fakeCommentRepo) GetLatest(string) (*store.StoredComments, error) {
	if f.prior == nil {
		return nil, nil
	}
	cp := *f.prior
	return &cp, nil
}

func (f *fakeCommentRepo) Save(_ string, sc *store.StoredComments) error {
	cp := *sc
	f.saved = &cp
	return nil
}

func newServiceWithCommentRepo(cr *fakeCommentRepo) *Service {
	return &Service{
		postStore:    newFakePostRepo(),
		commentStore: cr,
		nsfwKnown:    map[string]bool{},
	}
}

// A removed-upstream payload must NOT overwrite an existing archive row — the
// whole point of the Time Machine path is to keep the prior good copy
// readable. The verdict is also flipped on the existing row so future fetches
// short-circuit.
func TestArchivePost_RemovedDoesNotOverwriteExisting(t *testing.T) {
	repo := newFakePostRepo()
	repo.rows["/r/sub/comments/abc"] = &store.StoredPost{
		URLPath:  "/r/sub/comments/abc",
		Title:    "Original Title",
		JSONData: []byte(`{"id":"abc","title":"Original Title"}`),
	}
	svc := newServiceWithFake(repo)

	removed := &reddit.Post{
		ID:        "abc",
		Title:     "Original Title",
		Permalink: "/r/sub/comments/abc",
		Removed:   true,
	}
	svc.ArchivePost(removed, "sub", "manual_refresh")

	if len(repo.saves) != 0 {
		t.Fatalf("Save was called %d times; removed post must not overwrite archive", len(repo.saves))
	}
	if len(repo.markedRemoved) != 1 || repo.markedRemoved[0] != "/r/sub/comments/abc" {
		t.Fatalf("MarkUpstreamRemoved calls = %v, want [/r/sub/comments/abc]", repo.markedRemoved)
	}
	if got := string(repo.rows["/r/sub/comments/abc"].JSONData); got != `{"id":"abc","title":"Original Title"}` {
		t.Fatalf("archive JSON was clobbered: %s", got)
	}
}

// A removed payload with no prior archive simply drops the write — there is
// nothing useful in the removed JSON to seed an archive from.
func TestArchivePost_RemovedSkipsWriteWhenNoExisting(t *testing.T) {
	repo := newFakePostRepo()
	svc := newServiceWithFake(repo)

	svc.ArchivePost(&reddit.Post{
		ID:        "abc",
		Permalink: "/r/sub/comments/abc",
		Removed:   true,
	}, "sub", "background")

	if len(repo.saves) != 0 {
		t.Fatalf("Save was called %d times; removed payload should not seed an archive", len(repo.saves))
	}
	if len(repo.markedRemoved) != 0 {
		t.Fatalf("MarkUpstreamRemoved called %d times without an existing row", len(repo.markedRemoved))
	}
}

// hasRemovedComment must traverse into replies — a tombstone buried under an
// alive parent still has to flip the cheap pre-check so the merge path runs.
func TestHasRemovedComment_Nested(t *testing.T) {
	tree := []reddit.Comment{{
		Kind: "t1", ID: "a", Removed: false,
		Replies: []reddit.Comment{{Kind: "t1", ID: "b", Removed: true}},
	}}
	if !hasRemovedComment(tree) {
		t.Fatalf("hasRemovedComment missed a nested Removed=true node")
	}
	clean := []reddit.Comment{{
		Kind: "t1", ID: "a",
		Replies: []reddit.Comment{{Kind: "t1", ID: "b"}},
	}}
	if hasRemovedComment(clean) {
		t.Fatalf("hasRemovedComment reported true on a fully-alive tree")
	}
}

// indexAliveComments must skip tombstones (so we never restore a body from
// another tombstone) but must still descend into their Replies to surface any
// alive grandchildren underneath. "more" nodes are skipped entirely.
func TestIndexAliveComments_OnlyAliveT1AndDescendsThroughRemoved(t *testing.T) {
	tree := []reddit.Comment{
		{Kind: "t1", ID: "alive1", Body: "hello"},
		{Kind: "t1", ID: "deadparent", Removed: true, Body: "[removed]", Replies: []reddit.Comment{
			{Kind: "t1", ID: "alivechild", Body: "still here"},
		}},
		{Kind: "more", ID: "more1"},
	}
	idx := indexAliveComments(tree)
	if _, ok := idx["alive1"]; !ok {
		t.Fatalf("alive root comment missing from index")
	}
	if _, ok := idx["alivechild"]; !ok {
		t.Fatalf("alive grandchild under a removed parent missing from index")
	}
	if _, ok := idx["deadparent"]; ok {
		t.Fatalf("removed parent leaked into the alive index")
	}
	if _, ok := idx["more1"]; ok {
		t.Fatalf("'more' node leaked into the alive index")
	}
}

// mergeRemovedBodies must restore Body+Author on Removed nodes whose ID
// matches a prior alive copy, must KEEP Removed=true (the badge signal), and
// must not touch alive nodes or unmatched tombstones.
func TestMergeRemovedBodies_RestoresBodyKeepsRemovedFlag(t *testing.T) {
	prior := map[string]reddit.Comment{
		"a": {Kind: "t1", ID: "a", Body: "original A", Author: reddit.Author{Name: "alice"}},
		"b": {Kind: "t1", ID: "b", Body: "original B", Author: reddit.Author{Name: "bob"}},
	}
	incoming := []reddit.Comment{
		{Kind: "t1", ID: "a", Removed: true, Body: "[removed]", Author: reddit.Author{Name: "[deleted]"}},
		{Kind: "t1", ID: "untouched", Body: "fresh", Author: reddit.Author{Name: "carol"}},
		{Kind: "t1", ID: "ghost", Removed: true, Body: "[removed]"}, // not in prior
		{Kind: "t1", ID: "parent", Replies: []reddit.Comment{
			{Kind: "t1", ID: "b", Removed: true, Body: "[removed]"},
		}},
	}
	mergeRemovedBodies(incoming, prior)

	if incoming[0].Body != "original A" || incoming[0].Author.Name != "alice" {
		t.Fatalf("removed node 'a' was not restored: body=%q author=%q", incoming[0].Body, incoming[0].Author.Name)
	}
	if !incoming[0].Removed {
		t.Fatalf("Removed flag must stay TRUE after merge — badge would disappear otherwise")
	}
	if incoming[1].Body != "fresh" || incoming[1].Author.Name != "carol" {
		t.Fatalf("non-removed node was mutated by merge: %+v", incoming[1])
	}
	if incoming[2].Body != "[removed]" {
		t.Fatalf("tombstone with no prior copy must stay tombstoned: %q", incoming[2].Body)
	}
	if incoming[3].Replies[0].Body != "original B" || !incoming[3].Replies[0].Removed {
		t.Fatalf("nested removed reply was not restored / lost its Removed flag: %+v", incoming[3].Replies[0])
	}
}

// ArchiveComments end-to-end: prior archive has alive bodies, new fetch comes
// back with [removed] tombstones, and the saved blob must carry the restored
// bodies + Removed=true so the renderer keeps showing the Time Machine badge.
func TestArchiveComments_RemovedTreeRestoresBodiesFromPrior(t *testing.T) {
	priorTree := []reddit.Comment{
		{Kind: "t1", ID: "c1", Body: "kept body", Author: reddit.Author{Name: "alice"}},
	}
	priorBlob, _ := json.Marshal(priorTree)
	cr := &fakeCommentRepo{prior: &store.StoredComments{
		PostURLPath: "/r/sub/comments/abc",
		JSONData:    priorBlob,
	}}
	svc := newServiceWithCommentRepo(cr)

	incoming := []reddit.Comment{
		{Kind: "t1", ID: "c1", Removed: true, Body: "[removed]", Author: reddit.Author{Name: "[deleted]"}},
	}
	svc.ArchiveComments("/r/sub/comments/abc", incoming)

	if cr.saved == nil {
		t.Fatalf("ArchiveComments did not Save anything")
	}
	var got []reddit.Comment
	if err := json.Unmarshal(cr.saved.JSONData, &got); err != nil {
		t.Fatalf("saved JSON does not parse: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("saved tree shape unexpected: %+v", got)
	}
	if string(got[0].Body) != "kept body" {
		t.Fatalf("body was not restored from prior archive: %q", got[0].Body)
	}
	if got[0].Author.Name != "alice" {
		t.Fatalf("author was not restored from prior archive: %q", got[0].Author.Name)
	}
	if !got[0].Removed {
		t.Fatalf("Removed flag was cleared — Time Machine badge would not render")
	}
}

// When the incoming tree is fully alive, ArchiveComments must NOT read the
// prior archive (cheap pre-check via hasRemovedComment skips the merge).
// We assert by saving a payload that round-trips byte-identical to its input
// — proving no merge mutation happened.
func TestArchiveComments_AlivePayloadByPassesMerge(t *testing.T) {
	cr := &fakeCommentRepo{} // prior=nil; if merge ran, it would no-op anyway
	svc := newServiceWithCommentRepo(cr)

	incoming := []reddit.Comment{{Kind: "t1", ID: "c1", Body: "hello"}}
	want, _ := json.Marshal(incoming)
	svc.ArchiveComments("/r/sub/comments/abc", incoming)

	if cr.saved == nil {
		t.Fatalf("ArchiveComments did not Save the alive payload")
	}
	if string(cr.saved.JSONData) != string(want) {
		t.Fatalf("alive payload was mutated:\n got=%s\nwant=%s", cr.saved.JSONData, want)
	}
}

// A normal (non-removed) post still saves, proving the guard isn't blocking
// the healthy path.
func TestArchivePost_AlivePayloadSaves(t *testing.T) {
	repo := newFakePostRepo()
	svc := newServiceWithFake(repo)

	svc.ArchivePost(&reddit.Post{
		ID:        "abc",
		Title:     "Hello",
		Permalink: "/r/sub/comments/abc",
	}, "sub", "manual_refresh")

	if len(repo.saves) != 1 {
		t.Fatalf("Save was called %d times, want 1", len(repo.saves))
	}
	if len(repo.markedRemoved) != 0 {
		t.Fatalf("MarkUpstreamRemoved called %d times for an alive post", len(repo.markedRemoved))
	}
	// On-demand ArchivePost carries no listing position: L3 ordering must not
	// see a fabricated rank for a post that never appeared in a hot listing.
	if repo.saves[0].ListingRank != nil || repo.saves[0].ListingSeenAt != nil {
		t.Fatalf("ArchivePost set a listing position (rank=%v seen=%v), want both nil",
			repo.saves[0].ListingRank, repo.saves[0].ListingSeenAt)
	}
}

// ArchiveListing stamps each post with its 0-based index in the API-returned
// order and one shared snapshot time, so L3 can drain a hot listing
// top-to-bottom (listing_rank ASC) within the freshest snapshot.
func TestArchiveListing_StampsSequentialRanks(t *testing.T) {
	repo := newFakePostRepo()
	svc := newServiceWithFake(repo)

	posts := []reddit.Post{
		{ID: "a", Permalink: "/r/sub/comments/a"},
		{ID: "b", Permalink: "/r/sub/comments/b"},
		{ID: "c", Permalink: "/r/sub/comments/c"},
	}
	svc.ArchiveListing(posts, "sub", "natural_prefetch")

	if len(repo.saves) != 3 {
		t.Fatalf("Save was called %d times, want 3", len(repo.saves))
	}
	var seen *time.Time
	for i, sp := range repo.saves {
		if sp.ListingRank == nil {
			t.Fatalf("post %d: ListingRank is nil, want %d", i, i)
		}
		if *sp.ListingRank != i {
			t.Fatalf("post %d: ListingRank = %d, want %d (API order, 0=top)", i, *sp.ListingRank, i)
		}
		if sp.ListingSeenAt == nil {
			t.Fatalf("post %d: ListingSeenAt is nil, want a shared snapshot", i)
		}
		if seen == nil {
			seen = sp.ListingSeenAt
		} else if !sp.ListingSeenAt.Equal(*seen) {
			t.Fatalf("post %d: ListingSeenAt = %v, want shared %v across the fetch", i, sp.ListingSeenAt, *seen)
		}
	}
}

// TestCommentMediaURLs verifies the L2 harvest source: it pulls inline image
// URLs out of the latest archived comment tree (including nested replies) and
// returns nil when no comments are stored.
func TestCommentMediaURLs(t *testing.T) {
	tree := []reddit.Comment{
		{
			Kind: "t1",
			Body: `<a href="/preview/pre/c1.png?width=640&amp;s=sig">link</a>`,
			Replies: []reddit.Comment{
				{Kind: "t1", Body: `<img src="/img/c2.jpg">`},
			},
		},
	}
	data, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("marshal tree: %v", err)
	}
	svc := newServiceWithCommentRepo(&fakeCommentRepo{
		prior: &store.StoredComments{JSONData: data},
	})

	got := svc.CommentMediaURLs("/r/sub/comments/abc")
	want := []string{
		"https://preview.redd.it/c1.png?width=640&s=sig",
		"https://i.redd.it/c2.jpg",
	}
	if len(got) != len(want) {
		t.Fatalf("CommentMediaURLs() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("CommentMediaURLs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// No archived comments → nil.
	empty := newServiceWithCommentRepo(&fakeCommentRepo{})
	if got := empty.CommentMediaURLs("/r/sub/comments/abc"); got != nil {
		t.Errorf("CommentMediaURLs() with no comments = %v, want nil", got)
	}
}
