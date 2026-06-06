package archive

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

// postRepo is the narrow slice of PostStore the archive service actually
// reaches for. Defined here as an interface so the removed-overwrite guard can
// be unit-tested with an in-memory fake — *store.PostStore satisfies it.
type postRepo interface {
	Save(*store.StoredPost) error
	Get(string) (*store.StoredPost, error)
	MarkUpstreamRemoved(string) error
}

// commentRepo is the narrow slice of CommentStore the archive service uses;
// declared as an interface so the removed-merge guard can be unit-tested with
// an in-memory fake — *store.CommentStore satisfies it.
type commentRepo interface {
	GetLatest(string) (*store.StoredComments, error)
	Save(string, *store.StoredComments) error
}

type Service struct {
	postStore      postRepo
	commentStore   commentRepo
	subStore       *store.SubredditStore
	subStatusStore *store.SubStatusStore

	nsfwMu    sync.Mutex
	nsfwKnown map[string]bool // lowercase sub name → true (already marked NSFW in DB)

	// control is the live Archive Control filter. Hot-swapped via SetControl on
	// settings save / startup; read lock-free on every ArchivePost.
	control controlPtr
}

func NewService(ps *store.PostStore, cs *store.CommentStore, ss *store.SubredditStore) *Service {
	return &Service{
		postStore:    ps,
		commentStore: cs,
		subStore:     ss,
		nsfwKnown:    make(map[string]bool),
	}
}

// SetControl swaps in a freshly parsed Archive Control filter. Safe to call
// concurrently with ArchivePost — subsequent archive calls see the new rule.
// Passing nil clears the filter (allows everything).
func (s *Service) SetControl(c *Control) {
	s.control.store(c)
}

// SetControlFromString parses raw with ParseControl and installs the result.
// Convenience for the settings save path and startup wiring.
func (s *Service) SetControlFromString(raw string) {
	s.SetControl(ParseControl(raw))
}

// SetSubStatusStore wires the SubStatusStore so the archiver can sticky-mark
// subs as NSFW the first time it sees an over_18 post. Optional; if unset,
// NSFW propagation is skipped.
func (s *Service) SetSubStatusStore(sss *store.SubStatusStore) {
	s.subStatusStore = sss
}

// markNSFWIfNeeded records the sub as NSFW exactly once per process lifetime.
// It avoids a DB write per archived post by checking an in-memory set first,
// then performing a single sticky upsert (the SQL is a no-op if already true).
func (s *Service) markNSFWIfNeeded(sub string) {
	if s.subStatusStore == nil || sub == "" {
		return
	}
	key := strings.ToLower(sub)
	s.nsfwMu.Lock()
	if s.nsfwKnown[key] {
		s.nsfwMu.Unlock()
		return
	}
	s.nsfwKnown[key] = true
	s.nsfwMu.Unlock()

	if err := s.subStatusStore.SetNSFW(sub, true); err != nil {
		log.Printf("archive: mark nsfw %s: %v", sub, err)
		// Roll back so a transient error can be retried on the next post.
		s.nsfwMu.Lock()
		delete(s.nsfwKnown, key)
		s.nsfwMu.Unlock()
	}
}

func (s *Service) ArchivePost(post *reddit.Post, subreddit, source string) {
	jsonData, err := json.Marshal(post)
	if err != nil {
		return
	}

	urlPath := post.Permalink
	if urlPath == "" {
		if subreddit != "" && post.ID != "" {
			urlPath = fmt.Sprintf("/r/%s/comments/%s", subreddit, post.ID)
		} else {
			return
		}
	}

	sub := post.Community
	if sub == "" {
		sub = subreddit
	}

	// Archive Control: the user's whitelist/blacklist short-circuits archival
	// before any DB write. An empty/missing setting permits every sub.
	if ctl := s.control.load(); ctl != nil && !ctl.Allow(sub) {
		return
	}

	// Removed-by-Reddit: if upstream says the post is gone, never overwrite a
	// previously-good archive copy. Flip the sticky upstream_removed verdict on
	// the existing row so future fetches skip this permalink, then return. When
	// there is no prior archive we simply discard — there is nothing useful in
	// the removed payload (Body is "[Removed by Reddit]", media fields are
	// usually wiped).
	if post.Removed {
		if existing, _ := s.postStore.Get(urlPath); existing != nil && !existing.UpstreamRemoved {
			if err := s.postStore.MarkUpstreamRemoved(urlPath); err != nil {
				log.Printf("archive: mark upstream removed %s: %v", urlPath, err)
			}
		}
		return
	}

	if post.NSFW || post.Flags.NSFW {
		s.markNSFWIfNeeded(sub)
	}

	if err := s.postStore.Save(&store.StoredPost{
		URLPath:    urlPath,
		Subreddit:  sub,
		PostID:     post.ID,
		Title:      post.Title,
		JSONData:   jsonData,
		Author:     post.Author.Name,
		Score:      parseScore(post.Score),
		CreatedUTC: time.Unix(int64(post.CreatedTS), 0),
		Source:     source,
	}); err != nil {
		log.Printf("archive: save post %s: %v", urlPath, err)
	}
}

func (s *Service) ArchivePosts(posts []reddit.Post, subreddit, source string) {
	for i := range posts {
		s.ArchivePost(&posts[i], subreddit, source)
	}
}

func (s *Service) ArchiveComments(postURLPath string, comments []reddit.Comment) {
	if len(comments) == 0 {
		return
	}
	if ctl := s.control.load(); ctl != nil {
		if sub := subFromPermalink(postURLPath); sub != "" && !ctl.Allow(sub) {
			return
		}
	}
	// Removed-by-Reddit, comment edition: if the incoming tree contains any
	// "[Removed]" tombstones, splice the previously-archived body/author back
	// in by comment ID before writing. Mirrors the post-side guard at
	// ArchivePost so a re-fetch of a thread whose comments have since been
	// removed never overwrites the good local copy with tombstones. The
	// Removed flag stays set so the renderer keeps showing the Time Machine
	// badge.
	// Load the prior tree once and use it for BOTH the removed-body restore
	// AND the partial-fetch merge. Without the merge, a partial fetch (e.g.
	// /api/morechildren returning 5 replies under one parent) would overwrite
	// the full snapshot with that 5-comment fragment — the next archive read
	// would lose every other thread.
	var priorTree []reddit.Comment
	if s.commentStore != nil {
		if prior, err := s.commentStore.GetLatest(postURLPath); err == nil && prior != nil && len(prior.JSONData) > 0 {
			_ = json.Unmarshal(prior.JSONData, &priorTree)
		}
	}
	if hasRemovedComment(comments) && len(priorTree) > 0 {
		mergeRemovedBodies(comments, indexAliveComments(priorTree))
	}
	if len(priorTree) > 0 {
		comments = MergeCommentTrees(priorTree, comments)
	}
	data, err := json.Marshal(comments)
	if err != nil {
		return
	}
	if err := s.commentStore.Save(postURLPath, &store.StoredComments{
		PostURLPath:  postURLPath,
		JSONData:     data,
		CommentCount: countComments(comments),
	}); err != nil {
		log.Printf("archive: save comments for %s: %v", postURLPath, err)
	}
}

func (s *Service) ArchiveSubreddit(sub *reddit.Subreddit) {
	if ctl := s.control.load(); ctl != nil && !ctl.Allow(sub.Name) {
		return
	}
	jsonData, err := json.Marshal(sub)
	if err != nil {
		return
	}
	var members int
	if sub.Members[1] != "" {
		fmt.Sscanf(sub.Members[1], "%d", &members)
	}
	if err := s.subStore.Save(&store.StoredSubreddit{
		Name:        sub.Name,
		Title:       sub.Title,
		Description: sub.Description,
		IconURL:     sub.Icon,
		Members:     members,
		JSONData:    jsonData,
	}); err != nil {
		log.Printf("archive: save subreddit %s: %v", sub.Name, err)
	}
}

// subFromPermalink extracts the subreddit segment from a "/r/<sub>/..." path.
// Returns "" when the input does not start with "/r/", so callers can treat
// unknown shapes as "no sub" rather than mis-filtering on them.
func subFromPermalink(p string) string {
	const prefix = "/r/"
	if !strings.HasPrefix(p, prefix) {
		return ""
	}
	rest := p[len(prefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// hasRemovedComment reports whether any node in the tree carries Removed=true.
// Cheap pre-check so we only load the prior archived blob when there is
// actually something to splice.
func hasRemovedComment(cs []reddit.Comment) bool {
	for i := range cs {
		if cs[i].Removed {
			return true
		}
		if hasRemovedComment(cs[i].Replies) {
			return true
		}
	}
	return false
}

// indexAliveComments flattens the prior archived tree into an ID → Comment map,
// keeping only nodes whose own body wasn't already a tombstone. We restore
// from the freshest non-removed snapshot, not from another tombstone.
func indexAliveComments(cs []reddit.Comment) map[string]reddit.Comment {
	m := map[string]reddit.Comment{}
	var walk func([]reddit.Comment)
	walk = func(arr []reddit.Comment) {
		for i := range arr {
			if arr[i].Kind == "t1" && arr[i].ID != "" && !arr[i].Removed {
				m[arr[i].ID] = arr[i]
			}
			walk(arr[i].Replies)
		}
	}
	walk(cs)
	return m
}

// mergeRemovedBodies walks the incoming tree and, for every Removed node whose
// ID matches a previously-archived alive copy, restores Body and Author so the
// new row reads like the old one. Removed stays true so the renderer keeps the
// Time Machine badge — the body is preserved, the tombstone signal is not.
func mergeRemovedBodies(cs []reddit.Comment, prior map[string]reddit.Comment) {
	for i := range cs {
		if cs[i].Removed && cs[i].ID != "" {
			if p, ok := prior[cs[i].ID]; ok {
				cs[i].Body = p.Body
				cs[i].Author = p.Author
			}
		}
		if len(cs[i].Replies) > 0 {
			mergeRemovedBodies(cs[i].Replies, prior)
		}
	}
}

func countComments(comments []reddit.Comment) int {
	n := 0
	for i := range comments {
		if comments[i].Kind == "t1" {
			n++
			n += countComments(comments[i].Replies)
		}
	}
	return n
}

func parseScore(score [2]string) int {
	var n int
	fmt.Sscanf(score[1], "%d", &n)
	return n
}
