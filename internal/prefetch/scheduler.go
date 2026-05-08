package prefetch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/ratelimit"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

// MediaDownloader abstracts the media proxy for async downloads.
// Implemented by media.Proxy.
type MediaDownloader interface {
	DownloadAsync(originalURL string)
}

type Scheduler struct {
	cfg          config.PrefetchConfig
	rateLimiter  *ratelimit.Manager
	redditCli    *reddit.Client
	postStore    *store.PostStore
	commentStore *store.CommentStore
	subStore     *store.SubredditStore
	media        MediaDownloader
	stopCh       chan struct{}
}

func New(
	cfg config.PrefetchConfig,
	rateLimiter *ratelimit.Manager,
	redditCli *reddit.Client,
	postStore *store.PostStore,
	commentStore *store.CommentStore,
	subStore *store.SubredditStore,
	media MediaDownloader,
) *Scheduler {
	return &Scheduler{
		cfg:          cfg,
		rateLimiter:  rateLimiter,
		redditCli:    redditCli,
		postStore:    postStore,
		commentStore: commentStore,
		subStore:     subStore,
		media:        media,
		stopCh:       make(chan struct{}),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	go s.run(ctx)
}

func (s *Scheduler) run(ctx context.Context) {
	if s.cfg.CheckInterval <= 0 {
		s.cfg.CheckInterval = 30 * time.Second
	}
	ticker := time.NewTicker(s.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				log.Printf("prefetch: run error: %v", err)
			}
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		}
	}
}

func (s *Scheduler) Stop() {
	select {
	case s.stopCh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) RunOnce(ctx context.Context) error {
	allowed, budget := s.rateLimiter.CanPrefetch(ctx)
	if !allowed || budget <= 0 {
		return nil
	}

	subs := make([]config.PrefetchSubConfig, len(s.cfg.Subreddits))
	copy(subs, s.cfg.Subreddits)
	sort.Slice(subs, func(i, j int) bool {
		return subs[i].Priority > subs[j].Priority
	})

	used := 0
	for _, sub := range subs {
		if used >= budget {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.prefetchSub(ctx, sub, &used, budget); err != nil {
			log.Printf("prefetch: %s failed: %v", sub.Name, err)
		}
	}

	log.Printf("prefetch: completed, used %d/%d budget", used, budget)
	return nil
}

func (s *Scheduler) prefetchSub(ctx context.Context, sub config.PrefetchSubConfig, used *int, budget int) error {
	if *used >= budget {
		return nil
	}

	sortBy := sub.Sort
	if sortBy == "" {
		sortBy = "hot"
	}

	posts, _, after, err := s.redditCli.FetchSubreddit(ctx, sub.Name, sortBy, "", 25)
	*used++
	s.rateLimiter.IncrementPrefetch()
	if err != nil {
		return fmt.Errorf("fetch listing: %w", err)
	}

	for i := range posts {
		s.savePost(&posts[i], sub.Name, "prefetch")
	}

	if *used < budget {
		subInfo, err := s.redditCli.FetchSubredditAbout(ctx, sub.Name)
		*used++
	s.rateLimiter.IncrementPrefetch()
		if err == nil {
			s.saveSubreddit(&subInfo)
		}
	}

	if sub.FetchComments {
		for i := range posts {
			if *used >= budget {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			s.prefetchComments(ctx, sub.Name, posts[i].ID, posts[i].Permalink, used)
		}
	}

	if sub.FetchMedia && s.media != nil {
		for i := range posts {
			s.downloadPostMedia(&posts[i])
		}
	}

	maxPages := sub.MaxPages
	if maxPages <= 0 {
		maxPages = 1
	}
	for page := 1; page < maxPages && after != "" && *used < budget; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		morePosts, _, nextAfter, err := s.redditCli.FetchSubreddit(ctx, sub.Name, sortBy, after, 25)
		*used++
	s.rateLimiter.IncrementPrefetch()
		if err != nil {
			return fmt.Errorf("fetch page %d: %w", page+1, err)
		}
		for i := range morePosts {
			s.savePost(&morePosts[i], sub.Name, "prefetch")
		}
		if sub.FetchMedia && s.media != nil {
			for i := range morePosts {
				s.downloadPostMedia(&morePosts[i])
			}
		}
		after = nextAfter
	}

	return nil
}

func (s *Scheduler) prefetchComments(ctx context.Context, sub, postID, permalink string, used *int) {
	post, comments, err := s.redditCli.FetchPost(ctx, sub, postID, "confidence")
	*used++
	s.rateLimiter.IncrementPrefetch()
	if err != nil {
		log.Printf("prefetch: comments for %s/%s failed: %v", sub, postID, err)
		return
	}

	_ = post

	commentsJSON, err := json.Marshal(comments)
	if err != nil {
		return
	}

	urlPath := permalink
	if urlPath == "" {
		urlPath = fmt.Sprintf("/r/%s/comments/%s", sub, postID)
	}

	_ = s.commentStore.Save(urlPath, &store.StoredComments{
		PostURLPath:  urlPath,
		JSONData:     commentsJSON,
		CommentCount: countComments(comments),
	})
}

func countComments(comments []reddit.Comment) int {
	count := 0
	for i := range comments {
		if comments[i].Kind == "t1" {
			count++
			count += countComments(comments[i].Replies)
		}
	}
	return count
}

func (s *Scheduler) savePost(p *reddit.Post, subreddit, source string) {
	jsonData, err := json.Marshal(p)
	if err != nil {
		return
	}

	urlPath := p.Permalink
	if urlPath == "" {
		urlPath = fmt.Sprintf("/r/%s/comments/%s", subreddit, p.ID)
	}

	_ = s.postStore.Save(&store.StoredPost{
		URLPath:    urlPath,
		Subreddit:  subreddit,
		PostID:     p.ID,
		Title:      p.Title,
		JSONData:   jsonData,
		Author:     p.Author.Name,
		Score:      parseScore(p.Score),
		CreatedUTC: time.Unix(int64(p.CreatedTS), 0),
		Source:     source,
	})
}

func (s *Scheduler) saveSubreddit(sub *reddit.Subreddit) {
	jsonData, err := json.Marshal(sub)
	if err != nil {
		return
	}

	members := 0
	if sub.Members[0] != "" {
		fmt.Sscanf(sub.Members[1], "%d", &members)
	}

	_ = s.subStore.Save(&store.StoredSubreddit{
		Name:        sub.Name,
		Title:       sub.Title,
		Description: sub.Description,
		IconURL:     sub.Icon,
		Members:     members,
		JSONData:    jsonData,
	})
}

func (s *Scheduler) downloadPostMedia(p *reddit.Post) {
	if p.Media.URL != "" {
		s.media.DownloadAsync(p.Media.URL)
	}
	if p.Thumbnail.URL != "" {
		s.media.DownloadAsync(p.Thumbnail.URL)
	}
	for i := range p.Gallery {
		if p.Gallery[i].URL != "" {
			s.media.DownloadAsync(p.Gallery[i].URL)
		}
	}
}

func parseScore(score [2]string) int {
	var n int
	fmt.Sscanf(score[1], "%d", &n)
	return n
}
