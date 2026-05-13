package prefetch

import (
	"context"
	"log"
	"math/rand"
	"strconv"
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

type SubStatusChecker interface {
	IsAlive(name string) (bool, error)
	MarkLive(name string) error
	RecordFailure(name, reason string) error
}

type Scheduler struct {
	cfg       config.PrefetchConfig
	pool      WindowInfoProvider
	settings  SettingsProvider
	cli       *reddit.Client
	publicCli *reddit.PublicClient
	archiver  *archive.Service
	media     MediaDownloader
	subStatus SubStatusChecker

	lastUserReq atomic.Int64 // unix timestamp of last real user request
}

func New(
	cfg config.PrefetchConfig,
	pool WindowInfoProvider,
	settings SettingsProvider,
	redditCli *reddit.Client,
	publicCli *reddit.PublicClient,
	archiver *archive.Service,
	media MediaDownloader,
	subStatus SubStatusChecker,
) *Scheduler {
	return &Scheduler{
		cfg:       cfg,
		pool:      pool,
		settings:  settings,
		cli:       redditCli,
		publicCli: publicCli,
		archiver:  archiver,
		media:     media,
		subStatus: subStatus,
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
	if s.settings == nil {
		return false
	}
	return s.settings.Get("enable_natural_prefetch") == "on"
}

func (s *Scheduler) activeSubs() []config.PrefetchSubConfig {
	if s.settings == nil {
		return nil
	}
	v := s.settings.Get("prefetch_subs")
	if v == "" {
		return nil
	}
	names := strings.Split(v, "+")
	subs := make([]config.PrefetchSubConfig, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		// User explicitly added this sub — never skip it
		subs = append(subs, config.PrefetchSubConfig{
			Name:          n,
			Sort:          "hot",
			MaxPages:      1,
			FetchComments: true,
			FetchMedia:    true,
			Priority:      10,
		})
	}
	return subs
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

	threshold := 50
	if s.settings != nil {
		if v := s.settings.Get("prefetch_threshold"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 99 {
				threshold = n
			}
		}
	}

	const windowTotal = 10 * time.Minute
	startAt := resetAt.Add(-windowTotal * time.Duration(100-threshold) / 100)
	if wait := time.Until(startAt); wait > 0 {
		log.Printf("natural prefetch: idling %.0fs (start at %d%% of window), reset in %.0fs", wait.Seconds(), threshold, windowLeft.Seconds())
		if err := sleep(ctx, wait); err != nil {
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
	if reserved < 5 {
		reserved = 5
	}
	usable := remaining - reserved

	if usable < 5 {
		log.Printf("natural prefetch: budget too low (usable %d, remaining %d, reserved %d), skipping", usable, remaining, reserved)
		return sleep(ctx, time.Until(resetAt))
	}

	requestBudget := usable
	if requestBudget > capacity/2 {
		requestBudget = capacity/2 + rand.Intn(capacity/10+1)
	}
	log.Printf("natural prefetch: planning %d requests (usable %d, remaining %d, reserved %d, capacity %d)", requestBudget, usable, remaining, reserved, capacity)

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
				log.Printf("natural prefetch: %s oauth failed: %v, trying public", subName, err)
				if s.publicCli != nil {
					posts, _, after, err = s.publicCli.FetchSubreddit(ctx, subName, sortBy, "", 25)
				}
				if err != nil {
					log.Printf("natural prefetch: %s listing failed: %v", subName, err)
					if s.subStatus != nil {
						s.subStatus.RecordFailure(subName, err.Error())
					}
					return
				}
			}
			if s.subStatus != nil {
				s.subStatus.MarkLive(subName)
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
			if err != nil && s.publicCli != nil {
				subInfo, err = s.publicCli.FetchSubredditAbout(ctx, subName)
			}
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
					if err != nil && s.publicCli != nil {
						_, comments, err = s.publicCli.FetchPost(ctx, subName, p.ID, "confidence")
					}
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
				if err != nil && s.publicCli != nil {
					posts, _, after, err = s.publicCli.FetchSubreddit(ctx, subName, sortBy, afterCursor, 25)
				}
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
