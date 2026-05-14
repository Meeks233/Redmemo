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

type Service struct {
	postStore      *store.PostStore
	commentStore   *store.CommentStore
	subStore       *store.SubredditStore
	subStatusStore *store.SubStatusStore

	nsfwMu    sync.Mutex
	nsfwKnown map[string]bool // lowercase sub name → true (already marked NSFW in DB)
}

func NewService(ps *store.PostStore, cs *store.CommentStore, ss *store.SubredditStore) *Service {
	return &Service{
		postStore:    ps,
		commentStore: cs,
		subStore:     ss,
		nsfwKnown:    make(map[string]bool),
	}
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
