package prefetch

import (
	"context"
	"log"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/reddit"
)

type MediaDownloader interface {
	DownloadAsync(originalURL string)
}

type WindowInfoProvider interface {
	WindowInfo() (resetAt time.Time, capacity int, remaining int)
}

type SettingsProvider interface {
	Get(key string) string
}

type Scheduler struct {
	cfg      config.PrefetchConfig
	pool     WindowInfoProvider
	settings SettingsProvider
	cli      *reddit.Client
	archiver *archive.Service
	media    MediaDownloader

	lastUserReq atomic.Int64 // unix timestamp of last real user request
}

func New(
	cfg config.PrefetchConfig,
	pool WindowInfoProvider,
	settings SettingsProvider,
	redditCli *reddit.Client,
	archiver *archive.Service,
	media MediaDownloader,
) *Scheduler {
	return &Scheduler{
		cfg:      cfg,
		pool:     pool,
		settings: settings,
		cli:      redditCli,
		archiver: archiver,
		media:    media,
	}
}

// NotifyUserRequest is called by HTTP handlers when a real user request consumes an OAuth token.
func (s *Scheduler) NotifyUserRequest() {
	s.lastUserReq.Store(time.Now().Unix())
}

func (s *Scheduler) Start(ctx context.Context) {
	go s.run(ctx)
}

func (s *Scheduler) Stop() {}

func (s *Scheduler) isEnabled() bool {
	if s.settings != nil {
		if v := s.settings.Get("enable_natural_prefetch"); v == "on" {
			return true
		}
	}
	return s.cfg.Enabled
}

func (s *Scheduler) activeSubs() []config.PrefetchSubConfig {
	// Settings-based subs take priority
	if s.settings != nil {
		if v := s.settings.Get("prefetch_subs"); v != "" {
			names := strings.Split(v, "+")
			subs := make([]config.PrefetchSubConfig, len(names))
			for i, n := range names {
				subs[i] = config.PrefetchSubConfig{
					Name:          n,
					Sort:          "hot",
					MaxPages:      1,
					FetchComments: true,
					FetchMedia:    true,
					Priority:      10,
				}
			}
			return subs
		}
	}
	return s.cfg.Subreddits
}

func (s *Scheduler) userRequestedRecently() bool {
	last := s.lastUserReq.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(last, 0)) < 30*time.Second
}

func (s *Scheduler) run(ctx context.Context) {
	for {
		if err := s.runWindow(ctx); err != nil {
			return
		}
	}
}

func (s *Scheduler) runWindow(ctx context.Context) error {
	if !s.isEnabled() {
		return sleep(ctx, 30*time.Second)
	}

	subs := s.activeSubs()
	if len(subs) == 0 {
		return sleep(ctx, 30*time.Second)
	}

	resetAt, capacity, _ := s.pool.WindowInfo()

	if capacity == 0 {
		return sleep(ctx, 30*time.Second)
	}

	windowLeft := time.Until(resetAt)
	if windowLeft <= 0 {
		return sleep(ctx, 5*time.Second)
	}

	// Phase 1: idle for the first half
	halfWindow := windowLeft / 2
	if halfWindow > 0 {
		log.Printf("natural prefetch: idling %.0fs (half window), reset in %.0fs", halfWindow.Seconds(), windowLeft.Seconds())
		if err := sleep(ctx, halfWindow); err != nil {
			return err
		}
	}

	// Re-check
	if !s.isEnabled() {
		return sleep(ctx, time.Until(resetAt))
	}

	_, capacity, remaining := s.pool.WindowInfo()
	if capacity == 0 {
		return sleep(ctx, 10*time.Second)
	}

	// Reserve 10% for real user requests
	reserved := capacity / 10
	usable := remaining - reserved

	threshold := capacity / 2
	if usable < threshold {
		log.Printf("natural prefetch: budget too low (usable %d, remaining %d, reserved %d), skipping", usable, remaining, reserved)
		return sleep(ctx, time.Until(resetAt))
	}

	// Consume 30-50% of total capacity, but never exceed usable
	pct := 30 + rand.Intn(21)
	requestBudget := capacity * pct / 100
	if requestBudget > usable {
		requestBudget = usable
	}
	log.Printf("natural prefetch: planning %d requests (%d%% of %d, usable %d, reserved %d)", requestBudget, pct, capacity, usable, reserved)

	tasks := s.buildTasks(ctx, subs, requestBudget)
	if len(tasks) == 0 {
		return sleep(ctx, time.Until(resetAt))
	}

	// Phase 2: execute with random 1-4s intervals, pause on user activity
	for i, task := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}

		if time.Now().After(resetAt) {
			log.Printf("natural prefetch: window expired after %d/%d tasks", i, len(tasks))
			break
		}

		// Pause if a real user requested recently
		if s.userRequestedRecently() {
			log.Printf("natural prefetch: user active, pausing 30s (at task %d/%d)", i, len(tasks))
			if err := sleep(ctx, 30*time.Second); err != nil {
				return err
			}
			if time.Now().After(resetAt) {
				break
			}
		}

		task()

		if i < len(tasks)-1 {
			delay := time.Duration(1000+rand.Intn(3000)) * time.Millisecond
			if err := sleep(ctx, delay); err != nil {
				return err
			}
		}
	}

	if wait := time.Until(resetAt); wait > 0 {
		return sleep(ctx, wait)
	}
	return nil
}

func (s *Scheduler) buildTasks(ctx context.Context, subs []config.PrefetchSubConfig, budget int) []func() {
	var tasks []func()

	for _, sub := range subs {
		if len(tasks) >= budget {
			break
		}

		subName := sub.Name
		sortBy := sub.Sort
		if sortBy == "" {
			sortBy = "hot"
		}
		fetchComments := sub.FetchComments
		fetchMedia := sub.FetchMedia
		maxPages := sub.MaxPages
		if maxPages <= 0 {
			maxPages = 1
		}

		var firstPagePosts []reddit.Post
		var afterCursor string
		tasks = append(tasks, func() {
			posts, _, after, err := s.cli.FetchSubreddit(ctx, subName, sortBy, "", 25)
			if err != nil {
				log.Printf("natural prefetch: %s listing: %v", subName, err)
				return
			}
			firstPagePosts = posts
			afterCursor = after
			s.archiver.ArchivePosts(posts, subName, "natural_prefetch")
			if fetchMedia && s.media != nil {
				for i := range posts {
					s.downloadPostMedia(&posts[i])
				}
			}
			log.Printf("natural prefetch: %s page 1, %d posts", subName, len(posts))
		})

		if len(tasks) >= budget {
			break
		}

		tasks = append(tasks, func() {
			subInfo, err := s.cli.FetchSubredditAbout(ctx, subName)
			if err != nil {
				log.Printf("natural prefetch: %s about: %v", subName, err)
				return
			}
			s.archiver.ArchiveSubreddit(&subInfo)
		})

		if len(tasks) >= budget {
			break
		}

		if fetchComments {
			for postIdx := 0; postIdx < 25 && len(tasks) < budget; postIdx++ {
				idx := postIdx
				tasks = append(tasks, func() {
					if idx >= len(firstPagePosts) {
						return
					}
					p := firstPagePosts[idx]
					_, comments, err := s.cli.FetchPost(ctx, subName, p.ID, "confidence")
					if err != nil {
						log.Printf("natural prefetch: %s comment %s: %v", subName, p.ID, err)
						return
					}
					permalink := p.Permalink
					if permalink == "" {
						permalink = "/r/" + subName + "/comments/" + p.ID
					}
					s.archiver.ArchiveComments(permalink, comments)
				})
			}
		}

		for page := 1; page < maxPages && len(tasks) < budget; page++ {
			pg := page
			tasks = append(tasks, func() {
				if afterCursor == "" {
					return
				}
				posts, _, after, err := s.cli.FetchSubreddit(ctx, subName, sortBy, afterCursor, 25)
				if err != nil {
					log.Printf("natural prefetch: %s page %d: %v", subName, pg+1, err)
					return
				}
				afterCursor = after
				s.archiver.ArchivePosts(posts, subName, "natural_prefetch")
				if fetchMedia && s.media != nil {
					for i := range posts {
						s.downloadPostMedia(&posts[i])
					}
				}
				log.Printf("natural prefetch: %s page %d, %d posts", subName, pg+1, len(posts))
			})
		}
	}

	if len(tasks) > 1 {
		rand.Shuffle(len(tasks)-1, func(i, j int) {
			tasks[i+1], tasks[j+1] = tasks[j+1], tasks[i+1]
		})
	}

	return tasks
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

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	}
}
