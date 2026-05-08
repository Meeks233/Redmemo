package archive

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

type Service struct {
	postStore    *store.PostStore
	commentStore *store.CommentStore
	subStore     *store.SubredditStore
}

func NewService(ps *store.PostStore, cs *store.CommentStore, ss *store.SubredditStore) *Service {
	return &Service{
		postStore:    ps,
		commentStore: cs,
		subStore:     ss,
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
