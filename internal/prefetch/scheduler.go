package prefetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/searchquery"
	"github.com/redmemo/redmemo/internal/store"
)

// l1TokenRetries is how many exponential-backoff attempts L1 makes to let an
// installed-but-transiently-unusable session token recover before abandoning a
// listing round. l1TokenRetryBase is the first wait; each attempt doubles it
// (base, 2*base, 4*base). The retry only re-checks the local token state and
// never spends an upstream request probing.
const (
	l1TokenRetries   = 3
	l1TokenRetryBase = 2 * time.Second
)

// TokenWaiter is implemented by the OAuth holder. NP uses it to block on a
// missing session token instead of issuing an unauthenticated public request:
// emitting two different identities (no-token public vs. about-to-arrive
// session token) from the same IP within seconds is a stealth tell. Optional —
// when nil (e.g. in tests) the scheduler simply skips the wait.
type TokenWaiter interface {
	WaitForToken(ctx context.Context) bool
	// TokenInstalled distinguishes cold start (no token ever installed — block
	// on WaitForToken) from a post-install transient where the token is present
	// but momentarily unusable (rate-limited or refreshing). In the latter case
	// WaitForToken's one-shot signal has already fired, so it returns instantly
	// and blocking is pointless.
	TokenInstalled() bool
	// TokenUsable reports whether a session token is usable *right now* — i.e.
	// not expired, not refreshing, and with rate budget remaining. It is a
	// purely local check (no upstream request), used by the L1 backoff retry to
	// poll whether an installed-but-transiently-unusable token has recovered.
	TokenUsable() bool
}

type MediaDownloader interface {
	DownloadMedia(ctx context.Context, url string) error
	// IsCached reports whether the media at url is already on disk; IsFetching
	// whether an on-demand (foreground) download for it is in flight right now.
	// L2 uses them to skip media the foreground path already has, and to freeze
	// a duplicate task while the foreground path is still fetching it.
	IsCached(url string) bool
	IsFetching(url string) bool
	// ListFailedAudio returns v.redd.it URLs whose audio mux exhausted its
	// retry budget, oldest first. RetryMuxAudio re-attempts one. Together they
	// back the L5 background audio-remux layer.
	ListFailedAudio(limit int) ([]string, error)
	RetryMuxAudio(ctx context.Context, videoURL string) (outcome string, err error)
}

type WindowInfoProvider interface {
	WindowInfo() (resetAt time.Time, capacity int, remaining int)
}

type SettingsProvider interface {
	Get(key string) string
	Set(key, value string) error
}

type SubStatusChecker interface {
	IsAlive(name string) (bool, error)
	MarkLive(name string) error
	RecordFailure(name, reason string) error
	ListAllAlive() ([]string, error)
}

// HRRecorder reports every successful upstream call to the HR rate-limit
// layer so background prefetch contributes to the global counter just like
// foreground HR requests.
type HRRecorder interface {
	RecordUpstream(ctx context.Context)
}

// workItem is a single request submitted by L1/L2 to the NP dispatch queue.
type workItem struct {
	label       string
	fn          func(ctx context.Context)
	done        chan struct{}
	needsBudget bool
}

// cycleState is persisted to DB so the scheduler can resume after container restart.
type cycleState struct {
	NextCycleAt time.Time         `json:"next_cycle_at"`
	Round       int               `json:"round"`
	Cursors     map[string]string `json:"cursors"`
	// Exhausted records subs whose listing returned no further pages. A sub
	// missing from Cursors is NOT necessarily exhausted — it may have failed
	// to fetch — so resumption must consult this set, not cursor presence.
	Exhausted map[string]bool `json:"exhausted,omitempty"`
}

const cycleStateKey = "_prefetch_cycle_state"

type SubIconProvider interface {
	Get(name string) (*store.SubIcon, error)
	Save(icon *store.SubIcon) error
	SaveAbout(name string, aboutJSON []byte) error
	ListExpired() ([]*store.SubIcon, error)
	ListAll() ([]*store.SubIcon, error)
	IconTTL() time.Duration
}

type Scheduler struct {
	cfg         config.PrefetchConfig
	pool        WindowInfoProvider
	tokenWaiter TokenWaiter
	settings    SettingsProvider
	cli         *reddit.Client
	publicCli   *reddit.PublicClient
	archiver  *archive.Service
	media     MediaDownloader
	subStatus SubStatusChecker
	postStore *store.PostStore
	iconStore SubIconProvider
	hr        HRRecorder
	Events    *EventLog

	queue       chan *workItem
	lastUserReq atomic.Int64

	// userActivePause yields the dispatch-loop pause applied when a user
	// request arrived recently. When nil the default randomized 25–40s
	// applies; tests override it for deterministic, fast runs.
	userActivePause func() time.Duration

	// Observable state for debug page
	statusMu    sync.RWMutex
	l1Phase     string
	l1Round     int
	l1MaxRounds int
	l1Subs      []string
	l1Cursors   map[string]string
	l1NextCycle time.Time
	l2Phase     string
	l2Sub       string
	l2Pending   int
	l5Phase     string
	l5Current   string
	l5Pending   int
	npPhase     string
	npCurrent   string
}

const (
	maxRoundsPerCycle = 8
	pageSize          = 25
)

func New(
	cfg config.PrefetchConfig,
	pool WindowInfoProvider,
	settings SettingsProvider,
	redditCli *reddit.Client,
	publicCli *reddit.PublicClient,
	archiver *archive.Service,
	media MediaDownloader,
	subStatus SubStatusChecker,
	postStore *store.PostStore,
	iconStore SubIconProvider,
	hr HRRecorder,
) *Scheduler {
	tw, _ := pool.(TokenWaiter)
	return &Scheduler{
		cfg:         cfg,
		pool:        pool,
		tokenWaiter: tw,
		settings:    settings,
		cli:         redditCli,
		publicCli:   publicCli,
		archiver:  archiver,
		media:     media,
		subStatus: subStatus,
		postStore: postStore,
		iconStore: iconStore,
		hr:        hr,
		Events:    NewEventLog(200),
		queue:     make(chan *workItem, 1),
	}
}

// recordUpstream tolerates a nil HR recorder (HR layer disabled).
func (s *Scheduler) recordUpstream(ctx context.Context) {
	if s.hr != nil {
		s.hr.RecordUpstream(ctx)
	}
}

func (s *Scheduler) NotifyUserRequest() {
	s.lastUserReq.Store(time.Now().Unix())
}

func (s *Scheduler) Start(ctx context.Context) {
	s.Events.Add(LevelInfo, "init", "scheduler started (L1/L2/L5 + NP dispatch)")
	go s.dispatchLoop(ctx)
	go s.producerLoop(ctx)
	go s.iconLoop(ctx)
}

func (s *Scheduler) Stop() {}

// ---------------------------------------------------------------------------
// Cycle state persistence
// ---------------------------------------------------------------------------

func (s *Scheduler) loadCycleState() *cycleState {
	if s.settings == nil {
		return nil
	}
	raw := s.settings.Get(cycleStateKey)
	if raw == "" {
		return nil
	}
	var st cycleState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		log.Printf("prefetch: failed to parse cycle state: %v", err)
		return nil
	}
	return &st
}

func (s *Scheduler) saveCycleState(st *cycleState) {
	if s.settings == nil {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := s.settings.Set(cycleStateKey, string(data)); err != nil {
		log.Printf("prefetch: failed to save cycle state: %v", err)
	}
}

func (s *Scheduler) clearCycleState() {
	if s.settings == nil {
		return
	}
	s.settings.Set(cycleStateKey, "")
}

// ---------------------------------------------------------------------------
// NP Dispatch Loop — the single gateway for all outgoing requests (FIFO)
// ---------------------------------------------------------------------------

// userPauseDuration is the delay applied before dispatching when a user has
// been active recently. It is injectable via s.userActivePause so tests can
// pin it instead of waiting out the production 25–40s randomized delay.
func (s *Scheduler) userPauseDuration() time.Duration {
	if s.userActivePause != nil {
		return s.userActivePause()
	}
	return time.Duration(25+rand.Intn(15)) * time.Second
}

func (s *Scheduler) dispatchLoop(ctx context.Context) {
	for {
		s.setNPStatus("idle", "")
		var item *workItem
		select {
		case item = <-s.queue:
		case <-ctx.Done():
			return
		}

		if item.needsBudget {
			s.setNPStatus("waiting for budget", item.label)
			if err := s.waitForBudget(ctx); err != nil {
				close(item.done)
				return
			}
		}

		if s.userRequestedRecently() {
			pause := s.userPauseDuration()
			s.setNPStatus("paused (user active)", item.label)
			s.Events.Addf(LevelInfo, "NP", "user active, pausing %s before: %s", formatDur(pause), item.label)
			if err := sleep(ctx, pause); err != nil {
				close(item.done)
				return
			}
		}

		s.setNPStatus("dispatching", item.label)
		s.Events.Addf(LevelInfo, "NP", "dispatching: %s", item.label)
		item.fn(ctx)
		close(item.done)

		s.setNPStatus("cooldown", "")
		delay := time.Duration(4000+rand.Intn(4000)) * time.Millisecond
		if err := sleep(ctx, delay); err != nil {
			return
		}
	}
}

func (s *Scheduler) waitForBudget(ctx context.Context) error {
	for {
		resetAt, capacity, remaining := s.pool.WindowInfo()
		if capacity == 0 {
			wait := time.Duration(25+rand.Intn(15)) * time.Second
			s.Events.Addf(LevelSkip, "NP", "no token capacity, waiting %s", formatDur(wait))
			if err := sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}

		reserved := capacity / 10
		if reserved < 5 {
			reserved = 5
		}
		if remaining > reserved {
			return nil
		}

		wait := time.Until(resetAt)
		if wait <= 0 {
			wait = 5 * time.Second
		}
		s.Events.Addf(LevelSkip, "NP", "budget low (remaining=%d, reserved=%d), waiting %s for window reset",
			remaining, reserved, formatDur(wait))
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (s *Scheduler) submit(ctx context.Context, label string, needsBudget bool, fn func(ctx context.Context)) error {
	item := &workItem{
		label:       label,
		fn:          fn,
		done:        make(chan struct{}),
		needsBudget: needsBudget,
	}
	select {
	case s.queue <- item:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-item.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// Producer Loop — L1/L2 generate work items and feed them to the NP queue
// ---------------------------------------------------------------------------

func (s *Scheduler) producerLoop(ctx context.Context) {
	for {
		if err := s.waitUntilEnabled(ctx); err != nil {
			s.Events.Addf(LevelError, "L1", "exiting: %v", err)
			return
		}
		if err := s.runBigCycle(ctx); err != nil {
			s.Events.Addf(LevelError, "L1", "exiting: %v", err)
			return
		}
	}
}

func (s *Scheduler) isEnabled() bool {
	if s.settings == nil {
		return false
	}
	return s.settings.Get("enable_natural_prefetch") == "on"
}

func (s *Scheduler) activeSubs() []string {
	if s.settings == nil {
		return nil
	}
	v := s.settings.Get("prefetch_subs")
	if v == "" {
		return nil
	}
	// prefetch_subs now holds a query in the global unified search grammar; the
	// subs to crawl are the sub: clause's includes (e.g. sub:golang+rust).
	return searchquery.Parse(v).WhiteSubs
}

func (s *Scheduler) userRequestedRecently() bool {
	last := s.lastUserReq.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(last, 0)) < 30*time.Second
}

func (s *Scheduler) waitUntilEnabled(ctx context.Context) error {
	for {
		if s.isEnabled() {
			if subs := s.activeSubs(); len(subs) > 0 {
				s.setL1Status("ready", 0, 0, subs, nil, time.Time{})
				return nil
			}
			v := ""
			if s.settings != nil {
				v = s.settings.Get("prefetch_subs")
			}
			s.Events.Addf(LevelSkip, "L1", "no subs configured (prefetch_subs=%q), sleeping 30s", v)
		} else {
			v := ""
			if s.settings != nil {
				v = s.settings.Get("enable_natural_prefetch")
			}
			s.setL1Status("disabled", 0, 0, nil, nil, time.Time{})
			s.Events.Addf(LevelSkip, "L1", "disabled (enable_natural_prefetch=%q), sleeping 30s", v)
		}
		if err := sleep(ctx, 30*time.Second); err != nil {
			return err
		}
	}
}

// runBigCycle executes one full L1 cycle, resuming from persisted state if available.
func (s *Scheduler) runBigCycle(ctx context.Context) error {
	subs := s.activeSubs()
	if len(subs) == 0 {
		return nil
	}

	// Try to restore state from a previous run
	var startRound int
	cursors := make(map[string]string)
	exhausted := make(map[string]bool)
	var cycleWait time.Duration

	if saved := s.loadCycleState(); saved != nil && !saved.NextCycleAt.IsZero() {
		if wait := time.Until(saved.NextCycleAt); wait > 0 && saved.Round >= maxRoundsPerCycle {
			// Previous cycle completed, still in sleep phase
			s.Events.Addf(LevelInfo, "L1", "resuming inter-cycle sleep -- next cycle in %s", formatDur(wait))
			log.Printf("natural prefetch L1: resuming inter-cycle sleep, next in %s", formatDur(wait))
			return sleep(ctx, wait)
		}
		if saved.Round > 0 && saved.Round < maxRoundsPerCycle {
			startRound = saved.Round
			if saved.Cursors != nil {
				cursors = saved.Cursors
			}
			if saved.Exhausted != nil {
				exhausted = saved.Exhausted
			}
			cycleWait = time.Until(saved.NextCycleAt)
			if cycleWait <= 0 {
				cycleWait = 12*time.Hour + time.Duration(rand.Int63n(int64(12*time.Hour)))
			}
			s.Events.Addf(LevelInfo, "L1", "resuming cycle at round %d/%d for [%s] (restored from DB)",
				startRound+1, maxRoundsPerCycle, strings.Join(subs, ", "))
			log.Printf("natural prefetch L1: resuming at round %d/%d", startRound+1, maxRoundsPerCycle)
		}
	}

	if cycleWait <= 0 {
		cycleWait = 12*time.Hour + time.Duration(rand.Int63n(int64(12*time.Hour)))
	}
	nextCycleAt := time.Now().Add(cycleWait)

	if startRound == 0 {
		s.Events.Addf(LevelInfo, "L1", "big cycle started for [%s] -- next cycle in %s",
			strings.Join(subs, ", "), formatDur(cycleWait))
		log.Printf("natural prefetch L1: big cycle started for [%s], next in %s",
			strings.Join(subs, ", "), formatDur(cycleWait))
	}
	s.setL1Status("running", startRound, maxRoundsPerCycle, subs, cursors, nextCycleAt)

	for round := startRound; round < maxRoundsPerCycle; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		if !s.isEnabled() {
			s.Events.Add(LevelSkip, "L1", "disabled mid-cycle, stopping")
			s.clearCycleState()
			break
		}

		activeSubs := 0
		for _, sub := range subs {
			if err := ctx.Err(); err != nil {
				return err
			}

			// Skip only subs that genuinely ran out of pages. A sub that
			// failed to fetch in an earlier round has no cursor either, but
			// must be retried — not silently dropped for the rest of the cycle.
			if round > 0 && exhausted[sub] {
				continue
			}
			// This sub is still in play this round (whether it succeeds or
			// fails), so the cycle should not end early on its account.
			activeSubs++

			cursor := cursors[sub]
			var posts []reddit.Post
			var after string
			var fetchErr error

			label := fmt.Sprintf("L1 r/%s round %d/%d listing (after=%q)", sub, round+1, maxRoundsPerCycle, cursor)
			err := s.submit(ctx, label, true, func(ctx context.Context) {
				posts, _, after, fetchErr = s.cli.FetchSubreddit(ctx, sub, "hot", cursor, pageSize)
				s.recordUpstream(ctx)
				// Token() returns nil for three distinct reasons; ErrNoTokenAvailable
				// collapses them. Disambiguate before reacting:
				//   - no token ever installed (cold start) → block until the
				//     first token+UA pair lands, then retry on the OAuth path.
				//   - token installed but momentarily unusable (rate-limited or
				//     refreshing) → tokenReady already fired, so WaitForToken
				//     would return instantly and the retry would fail identically.
				//     Don't emit a misleading "blocking" warn or spin a no-op
				//     retry; skip this sub for the round (quota resets within
				//     ~10 min, next round is 15-30 min out).
				// Either way, no publicCli fallback: emitting an unauthenticated
				// request from the same IP that's about to carry the session
				// token is a stealth tell we won't accept.
				if errors.Is(fetchErr, reddit.ErrNoTokenAvailable) && s.tokenWaiter != nil {
					if s.tokenWaiter.TokenInstalled() {
						// Token is installed but momentarily unusable (rate-limited
						// or refreshing). Rather than abandon the round outright,
						// give the local token a few exponential-backoff chances to
						// recover, re-checking only whether a usable session token
						// has reappeared — no upstream request is spent probing. If
						// it comes back, resume this round with a real fetch; if all
						// retries lapse, give up the round as before.
						s.Events.Addf(LevelWarn, "L1", "r/%s round %d: session token temporarily unusable (rate-limited or refreshing), retrying up to %dx with backoff", sub, round+1, l1TokenRetries)
						recovered := false
						for attempt := 0; attempt < l1TokenRetries; attempt++ {
							backoff := l1TokenRetryBase << attempt
							select {
							case <-ctx.Done():
								fetchErr = ctx.Err()
								return
							case <-time.After(backoff):
							}
							if s.tokenWaiter.TokenUsable() {
								s.Events.Addf(LevelInfo, "L1", "r/%s round %d: session token recovered on retry %d/%d, resuming round", sub, round+1, attempt+1, l1TokenRetries)
								recovered = true
								break
							}
							s.Events.Addf(LevelSkip, "L1", "r/%s round %d: token still unusable after retry %d/%d", sub, round+1, attempt+1, l1TokenRetries)
						}
						if !recovered {
							s.Events.Addf(LevelWarn, "L1", "r/%s round %d: session token still unusable after %d retries, skipping this round", sub, round+1, l1TokenRetries)
							if s.subStatus != nil {
								s.subStatus.RecordFailure(sub, "token temporarily unusable")
							}
							return
						}
						posts, _, after, fetchErr = s.cli.FetchSubreddit(ctx, sub, "hot", cursor, pageSize)
						s.recordUpstream(ctx)
					} else {
						s.Events.Addf(LevelWarn, "L1", "r/%s round %d: no session token yet, blocking until token+UA ready", sub, round+1)
						if s.tokenWaiter.WaitForToken(ctx) {
							posts, _, after, fetchErr = s.cli.FetchSubreddit(ctx, sub, "hot", cursor, pageSize)
							s.recordUpstream(ctx)
						}
					}
				}
				if fetchErr != nil {
					s.Events.Addf(LevelError, "L1", "r/%s round %d: fetch failed: %v", sub, round+1, fetchErr)
					if s.subStatus != nil {
						s.subStatus.RecordFailure(sub, fetchErr.Error())
					}
					return
				}
				s.Events.Addf(LevelOK, "L1", "r/%s round %d/%d: %d posts fetched (after=%q)",
					sub, round+1, maxRoundsPerCycle, len(posts), after)
				if s.subStatus != nil {
					s.subStatus.MarkLive(sub)
				}
				s.archiver.ArchivePosts(posts, sub, "natural_prefetch")
			})
			if err != nil {
				return err
			}

			if fetchErr != nil {
				continue
			}

			if after != "" {
				cursors[sub] = after
			} else {
				delete(cursors, sub)
				exhausted[sub] = true
				s.Events.Addf(LevelInfo, "L1", "r/%s: no more pages after round %d", sub, round+1)
			}

			if err := s.submitL2(ctx, sub); err != nil {
				return err
			}
		}

		// L5 trails L2: once this round's listing + media work is queued,
		// drain any videos whose audio mux earlier exhausted its retries.
		if err := s.submitL5(ctx); err != nil {
			return err
		}

		if round > 0 && activeSubs == 0 {
			s.Events.Add(LevelInfo, "L1", "all subs exhausted pages, ending cycle early")
			break
		}

		// Persist state after each round so we can resume on restart
		s.saveCycleState(&cycleState{
			NextCycleAt: nextCycleAt,
			Round:       round + 1,
			Cursors:     cursors,
			Exhausted:   exhausted,
		})
		s.setL1Status("running", round+1, maxRoundsPerCycle, subs, cursors, nextCycleAt)

		if round < maxRoundsPerCycle-1 {
			roundWait := 15*time.Minute + time.Duration(rand.Int63n(int64(15*time.Minute)))
			s.setL1Status("sleeping between rounds", round+1, maxRoundsPerCycle, subs, cursors, nextCycleAt)
			s.Events.Addf(LevelInfo, "L1", "round %d/%d complete -- next round in %s",
				round+1, maxRoundsPerCycle, formatDur(roundWait))
			log.Printf("natural prefetch L1: round %d/%d complete, next in %s",
				round+1, maxRoundsPerCycle, formatDur(roundWait))
			if err := sleep(ctx, roundWait); err != nil {
				return err
			}
		}
	}

	// Mark cycle complete: persist next cycle time so restart sleeps correctly
	s.saveCycleState(&cycleState{
		NextCycleAt: nextCycleAt,
		Round:       maxRoundsPerCycle,
		Cursors:     nil,
	})

	remaining := time.Until(nextCycleAt)
	if remaining <= 0 {
		remaining = 1 * time.Minute
	}
	s.setL1Status("sleeping between cycles", maxRoundsPerCycle, maxRoundsPerCycle, subs, nil, nextCycleAt)
	s.Events.Addf(LevelOK, "L1", "big cycle complete -- sleeping %s until next cycle", formatDur(remaining))
	log.Printf("natural prefetch L1: big cycle complete, sleeping %s", formatDur(remaining))
	return sleep(ctx, remaining)
}

// ---------------------------------------------------------------------------
// L2: Media download — submits CDN download tasks through the NP queue
// ---------------------------------------------------------------------------

// l2GraceWindow is how long L2 waits before re-checking media it froze because
// the on-demand (foreground) path was already fetching it. After the wait, a
// frozen item that became cached is dropped as a cancelled duplicate; one still
// missing is downloaded by L2 itself.
const l2GraceWindow = 35 * time.Second

// frozenPost is an L2 post with media that was being fetched on-demand when L2
// reached it. Its frozen items are re-checked after l2GraceWindow.
type frozenPost struct {
	urlPath string
	postID  string
	items   []mediaItem
	okSoFar bool // whether the post's non-frozen items all downloaded cleanly
}

func (s *Scheduler) submitL2(ctx context.Context, sub string) error {
	if s.postStore == nil || s.media == nil {
		return nil
	}

	pending, err := s.postStore.ListNeedingMedia(sub, pageSize)
	if err != nil {
		s.Events.Addf(LevelError, "L2", "r/%s: query pending media: %v", sub, err)
		return nil
	}

	if len(pending) == 0 {
		s.setL2Status("idle", sub, 0)
		return nil
	}

	s.setL2Status("downloading", sub, len(pending))
	s.Events.Addf(LevelInfo, "L2", "r/%s: %d posts need media -- submitting to NP queue", sub, len(pending))

	completed := 0
	var frozen []frozenPost

	for _, sp := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}

		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err != nil {
			s.Events.Addf(LevelError, "L2", "r/%s post %s: unmarshal failed: %v", sub, sp.PostID, err)
			continue
		}

		mediaItems := ExtractMediaItems(&p)
		if len(mediaItems) == 0 {
			s.postStore.SetMediaDone(sp.URLPath)
			continue
		}

		allOK := true
		var frozenItems []mediaItem
		for _, mi := range mediaItems {
			switch {
			case s.media.IsCached(mi.URL):
				// Already on disk — the on-demand path (or an earlier round)
				// got it. Nothing to do.
			case s.media.IsFetching(mi.URL):
				// The on-demand (HR/foreground) path is fetching it right now.
				// Freeze L2's duplicate task: defer it to the grace-window
				// re-check instead of racing the foreground fetch.
				frozenItems = append(frozenItems, mi)
			default:
				ok, ctxErr := s.l2Download(ctx, sub, sp.PostID, mi)
				if ctxErr != nil {
					return ctxErr
				}
				if !ok {
					allOK = false
				}
			}
		}

		switch {
		case len(frozenItems) > 0:
			frozen = append(frozen, frozenPost{
				urlPath: sp.URLPath,
				postID:  sp.PostID,
				items:   frozenItems,
				okSoFar: allOK,
			})
		case allOK:
			s.postStore.SetMediaDone(sp.URLPath)
			completed++
			s.Events.Addf(LevelOK, "L2", "r/%s post %s: %d media done (%s)",
				sub, sp.PostID, len(mediaItems), mediaKindSummary(mediaItems))
		}
	}

	// Frozen posts: wait out the grace window, then re-check. Media the
	// on-demand path finished in the meantime is a cancelled duplicate; media
	// it did not get, L2 downloads itself.
	if len(frozen) > 0 {
		s.Events.Addf(LevelInfo, "L2", "r/%s: %d posts frozen (media being fetched on-demand) -- rechecking in %s",
			sub, len(frozen), formatDur(l2GraceWindow))
		s.setL2Status("frozen (on-demand active)", sub, len(frozen))
		if err := sleep(ctx, l2GraceWindow); err != nil {
			return err
		}
		for _, fp := range frozen {
			if err := ctx.Err(); err != nil {
				return err
			}
			allOK := fp.okSoFar
			for _, mi := range fp.items {
				if s.media.IsCached(mi.URL) {
					s.Events.Addf(LevelOK, "L2", "r/%s post %s: %s fetched on-demand -- duplicate cancelled",
						sub, fp.postID, mi.Kind)
					continue
				}
				ok, ctxErr := s.l2Download(ctx, sub, fp.postID, mi)
				if ctxErr != nil {
					return ctxErr
				}
				if !ok {
					allOK = false
				}
			}
			if allOK {
				s.postStore.SetMediaDone(fp.urlPath)
				completed++
			}
		}
	}

	if completed > 0 {
		s.Events.Addf(LevelOK, "L2", "r/%s: media complete for %d/%d posts", sub, completed, len(pending))
	}
	s.setL2Status("idle", "", 0)
	return nil
}

// l2Download submits one media-download task through the NP queue. It returns
// whether the download succeeded; a non-nil error means the context was
// cancelled and the caller should unwind.
func (s *Scheduler) l2Download(ctx context.Context, sub, postID string, mi mediaItem) (bool, error) {
	var dlErr error
	label := fmt.Sprintf("L2 r/%s post %s %s", sub, postID, mi.Kind)
	if err := s.submit(ctx, label, false, func(ctx context.Context) {
		dlErr = s.media.DownloadMedia(ctx, mi.URL)
		if dlErr != nil {
			s.Events.Addf(LevelWarn, "L2", "r/%s post %s: %s download failed: %v", sub, postID, mi.Kind, dlErr)
		}
	}); err != nil {
		return false, err
	}
	return dlErr == nil, nil
}

// ---------------------------------------------------------------------------
// L5: Audio remux retry — drains the failed-audio FCFS queue through NP
// ---------------------------------------------------------------------------

const l5BatchSize = 25

// submitL5 re-attempts the audio mux for videos parked as 'failed'. It mirrors
// L2: each retry is a CDN-only request (no OAuth budget) submitted through the
// NP queue, so it trails L2 under the same FIFO dispatch and randomized
// pacing. The failed-audio queue is drained oldest-first (FCFS).
func (s *Scheduler) submitL5(ctx context.Context) error {
	if s.media == nil {
		return nil
	}

	failed, err := s.media.ListFailedAudio(l5BatchSize)
	if err != nil {
		s.Events.Addf(LevelError, "L5", "query failed-audio queue: %v", err)
		return nil
	}

	if len(failed) == 0 {
		s.setL5Status("idle", "", 0)
		return nil
	}

	s.setL5Status("remuxing", "", len(failed))
	s.Events.Addf(LevelInfo, "L5", "%d videos awaiting audio remux -- submitting to NP queue", len(failed))

	recovered := 0
	for _, videoURL := range failed {
		if err := ctx.Err(); err != nil {
			return err
		}

		u := videoURL
		short := shortURL(u)
		var outcome string
		var muxErr error
		s.setL5Status("remuxing", short, len(failed))

		err := s.submit(ctx, fmt.Sprintf("L5 audio-remux %s", short), false, func(ctx context.Context) {
			outcome, muxErr = s.media.RetryMuxAudio(ctx, u)
			switch {
			case muxErr != nil:
				s.Events.Addf(LevelError, "L5", "%s: remux error: %v", short, muxErr)
			case outcome == "recovered":
				s.Events.Addf(LevelOK, "L5", "%s: audio recovered", short)
			case outcome == "skipped":
				s.Events.Addf(LevelInfo, "L5", "%s: skipped (resolved or recently retried)", short)
			default:
				s.Events.Addf(LevelWarn, "L5", "%s: audio still unavailable", short)
			}
		})
		if err != nil {
			return err
		}
		if outcome == "recovered" {
			recovered++
		}
	}

	if recovered > 0 {
		s.Events.Addf(LevelOK, "L5", "audio recovered for %d/%d videos", recovered, len(failed))
	}
	s.setL5Status("idle", "", 0)
	return nil
}

// shortURL trims a CDN URL down to host-relative path (no scheme, no query)
// for compact event-log labels.
func shortURL(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// Shared utilities
// ---------------------------------------------------------------------------

type mediaItem struct {
	URL  string
	Kind string // "image", "video", "gif", "thumbnail", "poster", "gallery"
}

func ExtractMediaItems(p *reddit.Post) []mediaItem {
	var items []mediaItem

	if p.Media.URL != "" {
		kind := "image"
		switch p.PostType {
		case "video":
			kind = "video"
		case "gif":
			kind = "gif"
		}
		items = append(items, mediaItem{URL: reddit.UnformatURL(p.Media.URL), Kind: kind})
	}
	if p.Media.Poster != "" {
		items = append(items, mediaItem{URL: reddit.UnformatURL(p.Media.Poster), Kind: "poster"})
	}
	// The thumbnail is only rendered in listing cards for non-media post types
	// (link/etc.) — see the post_thumbnail block in partials.html, gated on
	// PostType not being self/image/video/gif. For media posts the card shows
	// the full Media.URL, so caching the tiny thumbnail is a wasted upstream
	// fetch; skip it for exactly the types the template skips.
	switch p.PostType {
	case "image", "video", "gif", "self":
	default:
		if p.Thumbnail.URL != "" {
			items = append(items, mediaItem{URL: reddit.UnformatURL(p.Thumbnail.URL), Kind: "thumbnail"})
		}
	}
	for i := range p.Gallery {
		if p.Gallery[i].URL != "" {
			items = append(items, mediaItem{URL: reddit.UnformatURL(p.Gallery[i].URL), Kind: "gallery"})
		}
	}
	return items
}

// ExtractMediaURLs returns just the URLs for backward compatibility / tests.
func ExtractMediaURLs(p *reddit.Post) []string {
	items := ExtractMediaItems(p)
	urls := make([]string, len(items))
	for i, it := range items {
		urls[i] = it.URL
	}
	return urls
}

func mediaKindSummary(items []mediaItem) string {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Kind]++
	}
	var parts []string
	for _, k := range []string{"video", "gif", "image", "poster", "thumbnail", "gallery"} {
		if n, ok := counts[k]; ok {
			if n == 1 {
				parts = append(parts, k)
			} else {
				parts = append(parts, fmt.Sprintf("%d %ss", n, k))
			}
		}
	}
	return strings.Join(parts, " + ")
}

func formatDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, sec)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

// ---------------------------------------------------------------------------
// Observable status for debug page
// ---------------------------------------------------------------------------

type PrefetchStatus struct {
	L1Phase     string
	L1Round     int
	L1MaxRounds int
	L1Subs      []string
	L1Cursors   map[string]string
	L1NextCycle string
	L2Phase     string
	L2Sub       string
	L2Pending   int
	L5Phase     string
	L5Current   string
	L5Pending   int
	NPPhase     string
	NPCurrent   string
	QueueLen    int
	Enabled     bool
	ActiveSubs  []string
}

func (s *Scheduler) Status() PrefetchStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	cursors := make(map[string]string, len(s.l1Cursors))
	for k, v := range s.l1Cursors {
		cursors[k] = v
	}

	var nextCycle string
	if !s.l1NextCycle.IsZero() {
		if d := time.Until(s.l1NextCycle); d > 0 {
			nextCycle = "in " + formatDur(d)
		} else {
			nextCycle = "now"
		}
	}

	subs := make([]string, len(s.l1Subs))
	copy(subs, s.l1Subs)

	return PrefetchStatus{
		L1Phase:     s.l1Phase,
		L1Round:     s.l1Round,
		L1MaxRounds: s.l1MaxRounds,
		L1Subs:      subs,
		L1Cursors:   cursors,
		L1NextCycle: nextCycle,
		L2Phase:     s.l2Phase,
		L2Sub:       s.l2Sub,
		L2Pending:   s.l2Pending,
		L5Phase:     s.l5Phase,
		L5Current:   s.l5Current,
		L5Pending:   s.l5Pending,
		NPPhase:     s.npPhase,
		NPCurrent:   s.npCurrent,
		QueueLen:    len(s.queue),
		Enabled:     s.isEnabled(),
		ActiveSubs:  s.activeSubs(),
	}
}

func (s *Scheduler) setL1Status(phase string, round, maxRounds int, subs []string, cursors map[string]string, nextCycle time.Time) {
	s.statusMu.Lock()
	s.l1Phase = phase
	s.l1Round = round
	s.l1MaxRounds = maxRounds
	s.l1Subs = subs
	if cursors != nil {
		c := make(map[string]string, len(cursors))
		for k, v := range cursors {
			c[k] = v
		}
		s.l1Cursors = c
	}
	s.l1NextCycle = nextCycle
	s.statusMu.Unlock()
}

func (s *Scheduler) setL2Status(phase, sub string, pending int) {
	s.statusMu.Lock()
	s.l2Phase = phase
	s.l2Sub = sub
	s.l2Pending = pending
	s.statusMu.Unlock()
}

func (s *Scheduler) setL5Status(phase, current string, pending int) {
	s.statusMu.Lock()
	s.l5Phase = phase
	s.l5Current = current
	s.l5Pending = pending
	s.statusMu.Unlock()
}

func (s *Scheduler) setNPStatus(phase, current string) {
	s.statusMu.Lock()
	s.npPhase = phase
	s.npCurrent = current
	s.statusMu.Unlock()
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
