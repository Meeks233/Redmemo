package prefetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
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

// bucketStatus is the live debug view of one bucket loop. Owned by the
// scheduler's statusMu; bucketLoop updates it once per cycle.
type bucketStatus struct {
	TF          string
	NextCycleAt time.Time
	Period      time.Duration
	Subs        []string
	Cursors     map[string]string
}

// bucketState is persisted per timeframe bucket so the scheduler can resume
// each bucket's cadence independently after container restart. NextCycleAt is
// the wall-clock time the current cycle should *start* (i.e. its first fetch
// is scheduled at some random offset into that cycle's period).
type bucketState struct {
	NextCycleAt time.Time         `json:"next_cycle_at"`
	Cursors     map[string]string `json:"cursors,omitempty"`
	// Exhausted records (sub, sort, tf) cursors whose listing returned no
	// further pages. Cleared at the start of every cycle so the bucket
	// re-walks fresh content on each period rather than skipping forever.
	Exhausted map[string]bool `json:"exhausted,omitempty"`
}

// bucketStateKey returns the per-bucket settings key. Bucket-scoped state was
// introduced when L1 was refactored from a single global cycle to one cycle
// per timeframe bucket (hour/day/week/month/year/all).
func bucketStateKey(tf string) string {
	return "_prefetch_bucket_state_" + tf
}

// legacyCycleStateKey is the pre-bucket scheduler state key. New code never
// writes it, but the producer clears it on startup so a stale entry from the
// monolithic-cycle era doesn't sit in the settings table forever.
const legacyCycleStateKey = "_prefetch_cycle_state"

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
	runStore  *store.PrefetchRunStore
	iconStore SubIconProvider
	hr        HRRecorder
	Events    *EventLog

	queue       chan *workItem
	lastUserReq atomic.Int64

	// userActivePause yields the dispatch-loop pause applied when a user
	// request arrived recently. When nil the default randomized 25–40s
	// applies; tests override it for deterministic, fast runs.
	userActivePause func() time.Duration

	// dispatchCooldown is the inter-call pause the dispatch loop sleeps
	// after every fetch. Defaults to 4–8s randomized when nil; tests pin it
	// to a tiny value so multiple submissions complete within a sub-second
	// window.
	dispatchCooldown func() time.Duration

	// fetchFunc, when non-nil, replaces the s.cli.FetchSubreddit call so
	// bucketLoop tests can exercise the dispatch / cadence path without
	// running a real Reddit client. The hook is honoured by every fetch
	// site (initial and post-token-recovery retry).
	fetchFunc func(ctx context.Context, sub, sort, tf, after string, limit int) ([]reddit.Post, string, string, error)

	// bucketGap and bucketBaseOverride let tests shrink the cadence so a
	// full bucket cycle completes in milliseconds instead of hours.
	// bucketGap is the per-sub floor; bucketBaseOverride, when non-zero,
	// replaces the timeframe-derived base period so the same code path is
	// exercised without waiting out 6h of jitter.
	bucketGap          time.Duration
	bucketBaseOverride time.Duration

	// L4 icon round queue: pre-ordered (post count desc) list of subs still to
	// fetch this round. Built lazily when empty by nextIconBatch. Both the
	// hourly tick and passive /archive triggers drain from the same queue, so
	// rapid triggers consume the round instead of multiplying upstream load.
	iconMu           sync.Mutex
	iconRound        []string
	iconEmptyBuildAt time.Time

	// Observable state for debug page
	statusMu    sync.RWMutex
	l1Phase     string
	l1Round     int
	l1MaxRounds int
	l1Subs      []string
	l1Cursors   map[string]string
	l1NextCycle time.Time
	// l1Buckets surfaces per-bucket schedule + cursors to /debug. Keyed by
	// timeframe (hour/day/.../all). Updated at the top of every bucket
	// cycle; the global L1 view picks the *earliest* NextCycleAt across
	// buckets and the union of all cursors so the debug page reflects the
	// "next thing about to happen" rather than any one bucket in isolation.
	l1Buckets map[string]*bucketStatus
	l2Phase     string
	l2Sub       string
	l2Pending   int
	// l2Cycles is the live wave-schedule view per active L2 cycle, keyed
	// by "tf|sub". Populated when runL2Cycle starts a fresh cycle for an
	// L1 fetch, advanced as each wave fires, and cleared on completion.
	l2Cycles map[string]*l2CycleSnap
	l5Phase     string
	l5Current   string
	l5Pending   int
	// L3 — deep archive (comments). Handler-initiated, on-demand only; the
	// scheduler tracks it for /debug visibility but never schedules it itself.
	l3Phase    string
	l3Current  string
	l3LastAt   time.Time
	l3Count    int
	// l3BindRecent is a small FIFO of the most recent bind-mode L3 fetches
	// (newest first). Surfaced on /debug so the operator can see exactly
	// which posts the binding pipeline has just archived.
	l3BindRecent []l3BindRecord
	// L4 — icon cache loop. Updated by iconLoop/runIconBatch so /debug shows
	// the live round queue, current sub, and next tick eta even between
	// hourly batches.
	l4Phase       string
	l4Current     string
	l4QueueLen    int
	l4NextTickAt  time.Time
	npPhase     string
	npCurrent   string
}

// Bucket identifiers — one per Reddit listing-API timeframe.
const (
	bucketHour  = "hour"
	bucketDay   = "day"
	bucketWeek  = "week"
	bucketMonth = "month"
	bucketYear  = "year"
	bucketAll   = "all"
)

// bucketOrder is the canonical iteration order of buckets — finest to
// coarsest. Used for deterministic event-log ordering and PrefetchStatus
// aggregation; ordering has no effect on the actual cadence which is
// per-bucket independent.
var bucketOrder = []string{bucketHour, bucketDay, bucketWeek, bucketMonth, bucketYear, bucketAll}

const (
	// jitterFrac is the ±fractional spread applied to every cadence period
	// (bucket cycle length and intra-cycle sub gap). Kept tight at 5% so the
	// cadence stays close to the user's intended timeframe semantics; still
	// enough wall-clock entropy that an observer can't pin the period to a
	// fixed value over a few cycles.
	jitterFrac = 0.05
	// minBucketGap is the floor for the intra-cycle pre-fetch sleep. Without
	// it, two subs in a tiny bucket could race the NP dispatcher's own
	// inter-call cooldown and look like a coordinated burst even after our
	// own randomization. 30s comfortably exceeds the dispatcher's 4-8s.
	minBucketGap = 30 * time.Second
	// minCyclePeriod is the absolute floor for one bucket cycle even after
	// downward jitter and a defensive min-gap multiplier. It guards against
	// pathological clock skew or misconfigured base periods producing a
	// zero/negative period (which would otherwise burn CPU in a tight loop).
	minCyclePeriod = time.Minute
	// l2WaveCap is the absolute ceiling for one L2 wave's per-sub fetch — the
	// dynamic 1/5-of-postCount chunk size is clamped against it so a runaway
	// L1 round (say, after a long downtime) can't ask one wave to drain
	// thousands of pending posts in a single dispatch burst.
	l2WaveCap = 100
	// L1 fetches a randomized closed-interval batch in [l1ListingMin,
	// l1ListingMax] posts per round. Reddit caps listing limit at 100, and
	// reading a fuller page per call is more natural than the previous
	// fixed 25 — it lets one round of L1 archive a larger swath without
	// changing the cadence or any downstream layer.
	l1ListingMin = 75
	l1ListingMax = 100
)

// l1RoundLimit returns the listing-API limit for one L1 round: a uniform
// pick across the closed interval [l1ListingMin, l1ListingMax].
func l1RoundLimit() int {
	return l1ListingMin + rand.Intn(l1ListingMax-l1ListingMin+1)
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
	postStore *store.PostStore,
	runStore *store.PrefetchRunStore,
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
		runStore:  runStore,
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
	s.Events.Add(LevelInfo, "init", "scheduler started (L1/L2/L5 + NP dispatch, per-timeframe buckets)")
	s.clearLegacyCycleState()
	s.reclaimPendingRuns(ctx)
	go s.dispatchLoop(ctx)
	go s.coordinatorLoop(ctx)
	go s.iconLoop(ctx)
}

// reclaimPendingRuns is the design contract for the L-layer ledger: the
// container is ephemeral, the prefetch_runs table is the source of truth.
// Anything still 'pending' at startup gets a fresh goroutine that sleeps
// until its scheduled_at and then drives runL2Wave / runL3Wave; anything
// stuck 'running' (mid-fetch when the process died) is failed out so the
// ledger doesn't lie. The L1 bucket loop itself recovers via the existing
// bucketState persistence — this only covers L2/L3 wave rows that the
// scheduling goroutine owned in memory.
func (s *Scheduler) reclaimPendingRuns(ctx context.Context) {
	if s.runStore == nil {
		return
	}
	if n, err := s.runStore.FailStaleRunning(); err != nil {
		s.Events.Addf(LevelWarn, "init", "reclaim: fail stale running rows: %v", err)
	} else if n > 0 {
		s.Events.Addf(LevelWarn, "init", "reclaim: %d row(s) left in 'running' by previous process -> marked fail", n)
	}
	rows, err := s.runStore.ListWavesForActiveCycles()
	if err != nil {
		s.Events.Addf(LevelError, "init", "reclaim: list active cycle waves: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	// Group by cycle_id; rebuild the in-memory L2 cycle snapshot so /debug
	// shows the wave schedule the previous process planned. L3 has no live
	// cycle snapshot (only a recent-bind ring), so its pending rows just need
	// goroutines without snapshot work.
	type group struct {
		layer    string
		tf, sub  string
		cycleID  string
		runs     []store.PrefetchRun
	}
	groups := map[string]*group{}
	order := []string{}
	for _, r := range rows {
		if r.Layer != "L2" && r.Layer != "L3" {
			continue
		}
		key := r.Layer + "|" + r.CycleID.String
		g, ok := groups[key]
		if !ok {
			g = &group{layer: r.Layer, tf: r.Bucket.String, sub: r.Subreddit.String, cycleID: r.CycleID.String}
			groups[key] = g
			order = append(order, key)
		}
		g.runs = append(g.runs, r)
	}

	totalPending := 0
	for _, key := range order {
		g := groups[key]
		if g.layer == "L2" {
			s.rebuildL2CycleSnapshot(g.tf, g.sub, g.cycleID, g.runs)
		}
		for _, r := range g.runs {
			if r.Status != "pending" {
				continue
			}
			totalPending++
			go s.resumePendingWave(ctx, r)
		}
	}
	if totalPending > 0 {
		s.Events.Addf(LevelInfo, "init", "reclaim: %d pending L2/L3 wave(s) across %d cycle(s) recovered from prefetch_runs",
			totalPending, len(order))
	}
}

// rebuildL2CycleSnapshot reconstructs the live l2Cycles entry that runL2Cycle
// would have built had it not been killed. cycleStart is derived from the
// trailing Unix timestamp in cycle_id (runOneSubFetch's format), period and
// chunk from any wave's payload, and the 5 wave offsets from each row's
// scheduled_at relative to cycleStart. currentWave is the number of waves
// already past pending (ok/fail/skipped/running before crash, now failed).
func (s *Scheduler) rebuildL2CycleSnapshot(tf, sub, cycleID string, runs []store.PrefetchRun) {
	if len(runs) == 0 || cycleID == "" {
		return
	}
	cycleStart := parseCycleStart(cycleID)
	var first struct {
		PostCount int `json:"post_count"`
		PeriodSec int `json:"period_sec"`
	}
	_ = json.Unmarshal(runs[0].Payload, &first)
	period := time.Duration(first.PeriodSec) * time.Second

	type waveRow struct {
		off   time.Duration
		chunk int
	}
	pairs := make([]waveRow, 0, len(runs))
	currentWave := 0
	for _, r := range runs {
		var meta struct {
			Chunk int `json:"chunk"`
		}
		_ = json.Unmarshal(r.Payload, &meta)
		if !r.ScheduledAt.IsZero() {
			pairs = append(pairs, waveRow{off: r.ScheduledAt.Sub(cycleStart), chunk: meta.Chunk})
		}
		if r.Status != "pending" {
			if w := int(r.SubInterval.Int32); w > currentWave {
				currentWave = w
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].off < pairs[j].off })
	offsets := make([]time.Duration, len(pairs))
	chunks := make([]int, len(pairs))
	for i, p := range pairs {
		offsets[i] = p.off
		chunks[i] = p.chunk
	}

	depth := s.resolveSubDepth(sub)
	s.statusMu.Lock()
	if s.l2Cycles == nil {
		s.l2Cycles = make(map[string]*l2CycleSnap)
	}
	s.l2Cycles[l2CycleKey(tf, sub)] = &l2CycleSnap{
		tf:          tf,
		sub:         sub,
		postCount:   first.PostCount,
		waveChunks:  chunks,
		bindMode:    depthHasL3(depth),
		cycleStart:  cycleStart,
		period:      period,
		waveOffsets: offsets,
		currentWave: currentWave,
		cycleID:     cycleID,
	}
	s.statusMu.Unlock()
}

// parseCycleStart extracts the Unix timestamp suffix from a cycle_id of the
// form "<tf>:<sub>:<unix>" (runOneSubFetch's format). Returns zero time on
// any parse failure — callers tolerate that (the snapshot just shows a stale
// "started X ago" line).
func parseCycleStart(cycleID string) time.Time {
	i := strings.LastIndexByte(cycleID, ':')
	if i < 0 || i == len(cycleID)-1 {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(cycleID[i+1:], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// resumePendingWave sleeps until the persisted scheduled_at and then drives
// the appropriate wave function. If the scheduled fire time is already in the
// past (container was offline across the wave's window) the wave is marked
// 'skipped' and never fires — L2/L3 waves never look back. Status transitions
// mirror runL2Cycle / runL3Cycle so the ledger looks identical regardless of
// whether the wave was driven by its original cycle goroutine or revived
// after a restart.
func (s *Scheduler) resumePendingWave(ctx context.Context, r store.PrefetchRun) {
	tf := r.Bucket.String
	sub := r.Subreddit.String
	subInterval := int(r.SubInterval.Int32)
	if wait := time.Until(r.ScheduledAt); wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return
		}
	} else {
		_ = s.runStore.MarkFinished(r.ID, "skipped", "wave window missed during downtime")
		s.Events.Addf(LevelWarn, r.Layer, "reclaim r/%s wave %d/%d: window passed at %s — skipped",
			sub, subInterval, l2WavesPerCycle, r.ScheduledAt.Format(time.RFC3339))
		if r.Layer == "L2" {
			s.maybeDropL2Cycle(tf, sub, subInterval)
		}
		return
	}
	var meta struct {
		Chunk int `json:"chunk"`
	}
	_ = json.Unmarshal(r.Payload, &meta)
	chunk := meta.Chunk
	if chunk < 1 {
		chunk = 1
	}
	cycleID := r.CycleID.String

	// TryMarkRunning fails the row if status is no longer 'pending' — most
	// commonly because SupersedePending demoted it after a fresh L1 cycle for
	// the same (tf, sub) opened a newer wave set. Exit cleanly in that case.
	ok, err := s.runStore.TryMarkRunning(r.ID)
	if err != nil {
		s.Events.Addf(LevelWarn, r.Layer, "reclaim r/%s wave %d: mark running: %v", sub, subInterval, err)
	} else if !ok {
		s.Events.Addf(LevelInfo, r.Layer, "reclaim r/%s wave %d/%d: superseded by newer cycle — skipped",
			sub, subInterval, l2WavesPerCycle)
		if r.Layer == "L2" {
			s.maybeDropL2Cycle(tf, sub, subInterval)
		}
		return
	}
	s.Events.Addf(LevelInfo, r.Layer, "reclaim r/%s wave %d/%d firing (chunk=%d, cycle=%s)",
		sub, subInterval, l2WavesPerCycle, chunk, cycleID)

	if r.Layer == "L2" {
		s.advanceL2Wave(tf, sub, subInterval)
	}

	var runErr error
	switch r.Layer {
	case "L2":
		// Re-check depth at fire time: a settings flip away from L2 between
		// schedule and reclaim must not force a stale download pass.
		depth := s.resolveSubDepth(sub)
		if !depthHasL2(depth) {
			_ = s.runStore.MarkFinished(r.ID, "skipped", "depth no longer covers L2")
			s.maybeDropL2Cycle(tf, sub, subInterval)
			return
		}
		runErr = s.runL2Wave(ctx, tf, sub, chunk, cycleID, subInterval)
	case "L3":
		runErr = s.runL3Wave(ctx, tf, sub, chunk, cycleID, subInterval)
	}
	if runErr != nil {
		_ = s.runStore.MarkFinished(r.ID, "fail", runErr.Error())
	} else {
		_ = s.runStore.MarkFinished(r.ID, "ok", "")
	}
	if r.Layer == "L2" {
		s.maybeDropL2Cycle(tf, sub, subInterval)
	}
}

// maybeDropL2Cycle clears the in-memory snapshot once the last wave has
// fired — outside the original runL2Cycle goroutine we have to do this by
// hand instead of via its defer.
func (s *Scheduler) maybeDropL2Cycle(tf, sub string, justFired int) {
	if justFired >= l2WavesPerCycle {
		s.dropL2Cycle(tf, sub)
	}
}

func (s *Scheduler) Stop() {}

// prefetchPhaseOffset returns the maximum random in-cycle offset for the next
// L1 fetch, derived from the user-configurable "Start after % of window
// elapsed" setting (prefetch_threshold, 1..99). The setting names a percentage
// of the cycle base period; each cycle's fetch is rolled uniformly inside
// [0, percent% * base], giving the bucket-loop a fresh phase per cycle while
// keeping the overall cadence anchored on the persisted NextCycleAt.
//
// Falls back to 5% (the pre-setting hardcoded value, matching S35) when the
// setting is missing or unparseable; an unwired settings provider also yields
// the safe 5% default.
func (s *Scheduler) prefetchPhaseOffset(base time.Duration) time.Duration {
	pct := 5
	if s.settings != nil {
		if raw := strings.TrimSpace(s.settings.Get("prefetch_threshold")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 99 {
				pct = n
			}
		}
	}
	spread := time.Duration(int64(base) * int64(pct) / 100)
	if spread <= 0 {
		spread = time.Second
	}
	return spread
}

// ---------------------------------------------------------------------------
// Cycle state persistence
// ---------------------------------------------------------------------------

func (s *Scheduler) loadBucketState(tf string) *bucketState {
	if s.settings == nil {
		return nil
	}
	raw := s.settings.Get(bucketStateKey(tf))
	if raw == "" {
		return nil
	}
	var st bucketState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		log.Printf("prefetch: failed to parse bucket=%s state: %v", tf, err)
		return nil
	}
	return &st
}

func (s *Scheduler) saveBucketState(tf string, st *bucketState) {
	if s.settings == nil {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := s.settings.Set(bucketStateKey(tf), string(data)); err != nil {
		log.Printf("prefetch: failed to save bucket=%s state: %v", tf, err)
	}
}

func (s *Scheduler) clearLegacyCycleState() {
	if s.settings == nil {
		return
	}
	if s.settings.Get(legacyCycleStateKey) != "" {
		s.settings.Set(legacyCycleStateKey, "")
	}
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
		var delay time.Duration
		if s.dispatchCooldown != nil {
			delay = s.dispatchCooldown()
		} else {
			delay = time.Duration(4000+rand.Intn(4000)) * time.Millisecond
		}
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

// coordinatorLoop owns the lifetime of all per-bucket producer goroutines.
// It waits for NP to be enabled with at least one sub, then groups subs by
// their resolved timeframe bucket and launches one bucketLoop per non-empty
// bucket. When the relevant settings change (subs / per-sub modes / global
// sort / global timeframe) it tears down the running bucket loops and rebuilds
// from scratch — cheaper than tracking partial deltas, and the bucketLoop
// preserves cursor continuity by reloading per-bucket state from settings on
// restart.
func (s *Scheduler) coordinatorLoop(ctx context.Context) {
	var bucketsCancel context.CancelFunc
	var bucketsWG sync.WaitGroup
	var lastSig string
	var lastSubs []string

	stopBuckets := func() {
		if bucketsCancel != nil {
			bucketsCancel()
			bucketsWG.Wait()
			bucketsCancel = nil
			s.dropBucketStatus()
		}
	}
	defer stopBuckets()

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if !s.isEnabled() {
			if bucketsCancel != nil {
				s.Events.Add(LevelSkip, "L1", "disabled or no subs, stopping bucket loops")
				stopBuckets()
				lastSig = ""
				lastSubs = nil
			}
			toggle, subs := "", ""
			if s.settings != nil {
				toggle = s.settings.Get("enable_natural_prefetch")
				subs = s.settings.Get("prefetch_subs")
			}
			s.setL1Status("disabled", 0, 0, nil, nil, time.Time{})
			s.Events.Addf(LevelSkip, "L1", "disabled (enable_natural_prefetch=%q, prefetch_subs=%q), sleeping 30s", toggle, subs)
			if err := sleep(ctx, 30*time.Second); err != nil {
				return
			}
			continue
		}

		sig := s.configSignature()
		if sig != lastSig {
			stopBuckets()
			subs := s.activeSubs()
			groups := s.groupSubsByBucket(subs)
			bctx, bcancel := context.WithCancel(ctx)
			bucketsCancel = bcancel
			started := 0
			for _, tf := range bucketOrder {
				members := groups[tf]
				if len(members) == 0 {
					continue
				}
				bucketsWG.Add(1)
				started++
				bucketSubs := append([]string(nil), members...)
				bucketTF := tf
				go func() {
					defer bucketsWG.Done()
					s.bucketLoop(bctx, bucketTF, bucketSubs)
				}()
			}
			s.Events.Addf(LevelInfo, "L1", "coordinator launched %d bucket loop(s) for [%s]",
				started, strings.Join(subs, ", "))
			s.setL1Status("running", 0, 0, subs, nil, time.Time{})
			lastSig = sig
			lastSubs = subs
		}
		_ = lastSubs // retained for future reload diagnostics

		if err := sleep(ctx, 30*time.Second); err != nil {
			return
		}
	}
}

// isEnabled reports whether NP should run. An empty crawl list (prefetch_subs
// blank, whether from the settings UI or REDMEMO_DEFAULT_PREFETCH_SUBS=) is
// treated the same as the toggle being off — without subs there is nothing for
// the layer to do, so we surface "disabled" instead of looping on "no subs
// configured".
func (s *Scheduler) isEnabled() bool {
	if s.settings == nil {
		return false
	}
	if s.settings.Get("enable_natural_prefetch") != "on" {
		return false
	}
	return len(s.activeSubs()) > 0
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

// subMode holds the resolved per-sub listing API parameters NP will use this
// round. Mirrors redlib's `/r/{sub}/{sort}.json?t=...` request grammar.
type subMode struct {
	Sort      string // hot|new|top|rising|controversial
	Timeframe string // hour|day|week|month|year|all (only honored by top/controversial)
}

// resolveSubParams returns the raw (Sort, Timeframe) parsed from settings —
// the Timeframe field is retained even when Sort doesn't honor it upstream,
// so the bucket scheduler can read the user's intended cadence regardless of
// which sort they picked. resolveSubMode is the API-facing wrapper that drops
// the timeframe for sort=hot/new/rising before it reaches the listing call.
func (s *Scheduler) resolveSubParams(sub string) subMode {
	mode := subMode{Sort: "hot", Timeframe: "day"}
	if s.settings != nil {
		if v := s.settings.Get("prefetch_sort"); v != "" {
			mode.Sort = v
		}
		if v := s.settings.Get("prefetch_timeframe"); v != "" {
			mode.Timeframe = v
		}
		if raw := s.settings.Get("prefetch_sub_modes"); raw != "" {
			target := strings.ToLower(sub)
			for _, clause := range splitTopLevelClauses(raw) {
				clause = strings.TrimSpace(clause)
				eq := strings.IndexByte(clause, '=')
				if eq < 0 {
					continue
				}
				if strings.ToLower(strings.TrimSpace(clause[:eq])) != target {
					continue
				}
				body := strings.TrimSpace(clause[eq+1:])
				for _, kv := range strings.Split(body, "&") {
					colon := strings.IndexByte(kv, ':')
					if colon < 0 {
						continue
					}
					key := strings.ToLower(strings.TrimSpace(kv[:colon]))
					val := strings.ToLower(strings.TrimSpace(kv[colon+1:]))
					switch key {
					case "sort":
						mode.Sort = val
					case "time", "t", "timeframe":
						mode.Timeframe = val
					}
				}
				break
			}
		}
	}
	return mode
}

// resolveSubMode returns the listing-API mode for sub: per-sub override from
// prefetch_sub_modes wins per-field (sort and time are overridden independently),
// then the global prefetch_sort + prefetch_timeframe, then ("hot", "day") as
// the hard-coded fallback. For sort=hot/new/rising the timeframe is dropped
// before it reaches the listing call (Reddit ignores it harmlessly, but a
// non-empty value would leak into cursorKey and the event log).
func (s *Scheduler) resolveSubMode(sub string) subMode {
	mode := s.resolveSubParams(sub)
	if mode.Sort != "top" && mode.Sort != "controversial" {
		mode.Timeframe = ""
	}
	return mode
}

// resolveSubBucket returns the timeframe bucket this sub belongs to. It uses
// the *raw* timeframe (the user's intended cadence) even when the sort
// wouldn't honor it for the API call, so a "sort=hot, time=week" sub still
// fires on a weekly cadence rather than collapsing into the default day
// bucket. Unknown timeframes fall back to "day".
func (s *Scheduler) resolveSubBucket(sub string) string {
	return normalizeBucket(s.resolveSubParams(sub).Timeframe)
}

func normalizeBucket(tf string) string {
	switch strings.ToLower(strings.TrimSpace(tf)) {
	case bucketHour, bucketDay, bucketWeek, bucketMonth, bucketYear, bucketAll:
		return strings.ToLower(strings.TrimSpace(tf))
	}
	return bucketDay
}

// bucketBasePeriod returns the fixed nominal cadence for each timeframe. The
// values match the user-facing intuition: an "hour" sub gets a 6h cycle so a
// listing snapshot lines up with a few full hour-windows; "all" gets one
// pass per year. Cycle wall-clock is then jittered ±jitterFrac to break any
// fixed-period detection.
func bucketBasePeriod(tf string) time.Duration {
	switch normalizeBucket(tf) {
	case bucketHour:
		return 6 * time.Hour
	case bucketDay:
		return 12 * time.Hour
	case bucketWeek:
		return 48 * time.Hour
	case bucketMonth:
		return 15 * 24 * time.Hour
	case bucketYear:
		return 180 * 24 * time.Hour
	case bucketAll:
		return 365 * 24 * time.Hour
	}
	return 12 * time.Hour
}

// groupSubsByBucket partitions the active sub list by timeframe bucket,
// preserving each bucket's encounter order so a stable shuffle within a
// bucket starts from a deterministic baseline.
func (s *Scheduler) groupSubsByBucket(subs []string) map[string][]string {
	groups := make(map[string][]string, len(bucketOrder))
	for _, sub := range subs {
		tf := s.resolveSubBucket(sub)
		groups[tf] = append(groups[tf], sub)
	}
	return groups
}

// configSignature is the coordinator's cheap dirty-bit: any change to subs,
// per-sub modes or the global sort/timeframe re-derives the per-bucket sub
// lists and re-launches the bucket loops. It's a settings hash rather than
// a deep diff because all three settings are short user-entered strings.
func (s *Scheduler) configSignature() string {
	if s.settings == nil {
		return ""
	}
	return s.settings.Get("prefetch_subs") + "|" +
		s.settings.Get("prefetch_sort") + "|" +
		s.settings.Get("prefetch_timeframe") + "|" +
		s.settings.Get("prefetch_sub_modes") + "|" +
		s.settings.Get("prefetch_default_depth")
}

// tfSuffix renders a timeframe as a compact event-log label suffix, or "" when
// no timeframe is set (e.g. sort=hot, where t is meaningless).
func tfSuffix(t string) string {
	if t == "" {
		return ""
	}
	return "/" + t
}

// cursorKey scopes the listing cursor to (sub, sort, timeframe) so a mid-cycle
// mode swap doesn't reuse a cursor that belongs to a different listing — the
// `after` token returned by /r/sub/hot is not valid against /r/sub/new.
func cursorKey(sub string, m subMode) string {
	if m.Timeframe != "" {
		return sub + "|" + m.Sort + "|" + m.Timeframe
	}
	return sub + "|" + m.Sort
}

func (s *Scheduler) userRequestedRecently() bool {
	last := s.lastUserReq.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(last, 0)) < 30*time.Second
}

// jitterPercent returns d adjusted by a uniform random factor in
// [1-frac, 1+frac]. Clamped to never go non-positive — a zero/negative sleep
// would defeat its purpose as a pacing barrier.
func jitterPercent(d time.Duration, frac float64) time.Duration {
	if d <= 0 {
		return 0
	}
	if frac <= 0 {
		return d
	}
	off := (rand.Float64()*2 - 1) * frac
	out := time.Duration(float64(d) * (1 + off))
	if out <= 0 {
		return d / 2
	}
	return out
}

// computeCyclePeriod returns the jittered length of one bucket cycle. Floors
// at minCyclePeriod and at gap*len(subs) so a tiny base period with many subs
// can't collapse into a burst-prone sub-second cycle. gap is the per-sub
// floor (production: minBucketGap; tests may shrink it). When base ≤ 0 the
// timeframe-derived default is used.
func computeCyclePeriod(tf string, nSubs int, gap, base time.Duration) time.Duration {
	overridden := base > 0
	if base <= 0 {
		base = bucketBasePeriod(tf)
	}
	period := jitterPercent(base, jitterFrac)
	if gap <= 0 {
		gap = minBucketGap
	}
	floor := time.Duration(nSubs) * gap
	// Skip the absolute minCyclePeriod floor when the caller has explicitly
	// passed a tiny base period — that path is reserved for tests, which
	// would otherwise wait out a full minute per cycle.
	if !overridden && floor < minCyclePeriod {
		floor = minCyclePeriod
	}
	if period < floor {
		period = floor
	}
	return period
}

// effectiveGap returns the per-sub minimum gap used by the bucket loop. Tests
// may shrink it via bucketGap; production uses minBucketGap.
func (s *Scheduler) effectiveGap() time.Duration {
	if s.bucketGap > 0 {
		return s.bucketGap
	}
	return minBucketGap
}

// bucketLoop runs the dispatch cadence for one timeframe bucket. The
// persisted bucketState.NextCycleAt is the wall-clock time of the *next
// fetch round* for this bucket. Each round fetches every sub in the bucket
// once (spread across one jittered period), then the schedule advances by
// exactly one period from the previous anchor.
//
// The schedule anchor is set ONCE — either restored from the persisted
// value (any container restart honours it verbatim) or, if no prior state
// exists, rolled with a small random offset and persisted up front. The
// loop never recomputes NextCycleAt = now + period on cycle entry; doing
// so let every container rebuild push the next fetch a full period into
// the future, which on a 12h day-bucket with frequent rebuilds meant the
// fetch never happened.
func (s *Scheduler) bucketLoop(ctx context.Context, tf string, subs []string) {
	if len(subs) == 0 {
		return
	}

	cursors := make(map[string]string)
	saved := s.loadBucketState(tf)
	if saved != nil && saved.Cursors != nil {
		cursors = saved.Cursors
	}

	base := bucketBasePeriod(tf)
	if s.bucketBaseOverride > 0 {
		base = s.bucketBaseOverride
	}
	s.Events.Addf(LevelInfo, "L1", "bucket=%s started for [%s] -- base period %s, %d sub(s)",
		tf, strings.Join(subs, ", "), formatDur(base), len(subs))

	// nextFetchAt is the scheduled time of the next fetch round.
	//   - hasSaved → honour the persisted value exactly. Container rebuilds
	//     read this and resume; never overwrite it here.
	//   - !hasSaved → fresh bucket. Roll a small randomized offset so a
	//     fleet of buckets does not burst at the same wall-clock instant,
	//     then persist it so a restart inside that offset still honours the
	//     originally-rolled time.
	var nextFetchAt time.Time
	hasSaved := saved != nil && !saved.NextCycleAt.IsZero()
	if hasSaved {
		nextFetchAt = saved.NextCycleAt
		if w := time.Until(nextFetchAt); w > 0 {
			s.Events.Addf(LevelInfo, "L1", "bucket=%s: honouring persisted schedule, next fetch in %s",
				tf, formatDur(w))
		} else {
			s.Events.Addf(LevelInfo, "L1", "bucket=%s: persisted schedule overdue by %s, firing immediately",
				tf, formatDur(-w))
		}
	} else {
		spread := s.prefetchPhaseOffset(base)
		// When the gap floor has been shrunk for tests, cap the initial
		// offset at a few gaps so the loop reaches its first fetch quickly.
		if s.bucketGap > 0 && s.bucketGap*4 < spread {
			spread = s.bucketGap * 4
		}
		nextFetchAt = time.Now().Add(time.Duration(rand.Int63n(int64(spread) + 1)))
		s.saveBucketState(tf, &bucketState{
			NextCycleAt: nextFetchAt,
			Cursors:     cursors,
		})
		s.Events.Addf(LevelInfo, "L1", "bucket=%s: no prior state, first fetch in %s",
			tf, formatDur(time.Until(nextFetchAt)))
	}
	s.recordBucketStatus(tf, nextFetchAt, base, subs, cursors)

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if !s.isEnabled() {
			s.Events.Addf(LevelSkip, "L1", "bucket=%s: disabled mid-cycle, exiting", tf)
			return
		}

		// Wait until the scheduled fetch time. Overdue → fall through
		// immediately; the user is owed a fetch.
		if w := time.Until(nextFetchAt); w > 0 {
			if err := sleep(ctx, w); err != nil {
				return
			}
		}

		gap := s.effectiveGap()
		period := computeCyclePeriod(tf, len(subs), gap, s.bucketBaseOverride)

		// Shuffle order each cycle so observers can't infer a stable sub
		// rotation. Reseed locally with the global rand source.
		ordered := append([]string(nil), subs...)
		rand.Shuffle(len(ordered), func(i, j int) { ordered[i], ordered[j] = ordered[j], ordered[i] })

		s.Events.Addf(LevelInfo, "L1", "bucket=%s cycle started -- period %s, order [%s]",
			tf, formatDur(period), strings.Join(ordered, ", "))
		s.setL1Status(fmt.Sprintf("bucket=%s running", tf), 0, 0, subs, cursors, nextFetchAt)

		// Reset per-cycle exhaustion: each cycle re-walks the listing from
		// the head if the previous cursor ran out. New "hot" / "top" content
		// appears between cycles, so a one-time exhaustion isn't permanent.
		exhausted := make(map[string]bool)
		n := len(ordered)

		for i, sub := range ordered {
			if err := ctx.Err(); err != nil {
				return
			}

			// Fetch first, then sleep before the next sub. For a
			// single-sub bucket this fires the sub at the scheduled
			// nextFetchAt and skips the per-sub sleep entirely — earlier
			// versions slept the full period BEFORE the only sub, which
			// (combined with container restarts overwriting NextCycleAt)
			// silently pushed the fetch indefinitely into the future.
			postCount, cycleID := s.runOneSubFetch(ctx, tf, sub, cursors, exhausted)

			// L2/L3 work is strictly bound to the most recent L1 cycle
			// for this (tf, sub). Every L1 fetch — success, fail, empty,
			// or depth=none — supersedes any prior pending L2/L3 waves
			// here, BEFORE runL2Cycle's own short-circuits (postCount=0,
			// depth=none, depth=l3 delegation) might return without ever
			// calling supersede themselves. cycleID is always non-empty
			// (runOneSubFetch stamps it before the fetch), so prior-cycle
			// rows always lose the cycle_id comparison.
			if s.runStore != nil {
				if n, err := s.runStore.SupersedePending("L2", tf, sub, cycleID, "superseded by newer L1 cycle"); err == nil && n > 0 {
					s.Events.Addf(LevelInfo, "L2", "r/%s: discarded %d stale wave(s) from previous cycle", sub, n)
				}
				if n, err := s.runStore.SupersedePending("L3", tf, sub, cycleID, "superseded by newer L1 cycle"); err == nil && n > 0 {
					s.Events.Addf(LevelInfo, "L3", "r/%s: discarded %d stale wave(s) from previous cycle", sub, n)
				}
			}

			// L2 no longer drains 25 items inline. Spawn a per-fetch
			// goroutine that splits the bucket period into 5 non-uniform
			// random sub-intervals; each wave drains roughly 1/5 of the
			// posts this L1 fetch surfaced. Goroutine inherits the bucket
			// context so a settings reload / shutdown cancels in-flight
			// waves too.
			go s.runL2Cycle(ctx, tf, sub, postCount, period, cycleID)

			// Persist after every sub so a restart picks up advanced
			// cursors. NextCycleAt advances only when the cycle finishes;
			// mid-cycle we keep writing the *current* anchor so a restart
			// during the cycle does not push the schedule forward.
			s.saveBucketState(tf, &bucketState{
				NextCycleAt: nextFetchAt,
				Cursors:     cursors,
				Exhausted:   exhausted,
			})

			if i < n-1 {
				// Space the remaining subs evenly across what's left of
				// the period. Floored at the gap so two subs in a tiny
				// bucket can't race the dispatcher even after unlucky
				// jitter.
				subGap := jitterPercent(period/time.Duration(n), jitterFrac)
				if subGap < gap {
					subGap = gap
				}
				if err := sleep(ctx, subGap); err != nil {
					return
				}
			}
		}

		// L5 trails L2 just as before: one drain pass per bucket cycle.
		if err := s.submitL5(ctx); err != nil {
			return
		}

		// Advance the anchor by exactly one period from the previous
		// anchor — preserving cadence across restarts. If we fell so far
		// behind that the next anchor is already in the past, snap to
		// now+period so we don't burn cycles back-to-back chasing a stale
		// schedule.
		prev := nextFetchAt
		nextFetchAt = nextFetchAt.Add(period)
		if time.Until(nextFetchAt) < 0 {
			nextFetchAt = time.Now().Add(period)
		}
		s.saveBucketState(tf, &bucketState{
			NextCycleAt: nextFetchAt,
			Cursors:     cursors,
			Exhausted:   exhausted,
		})
		s.recordBucketStatus(tf, nextFetchAt, period, ordered, cursors)
		s.Events.Addf(LevelInfo, "L1", "bucket=%s cycle complete (anchor %s → %s) -- next fetch in %s",
			tf, prev.Format(time.RFC3339), nextFetchAt.Format(time.RFC3339),
			formatDur(time.Until(nextFetchAt)))
	}
}

// runOneSubFetch performs one L1 listing fetch for a single sub, with the
// same token-recovery semantics the legacy big-cycle loop had. Fetch and
// archive outcomes mutate the supplied cursors/exhausted maps in place; the
// call is otherwise self-contained and never returns an error (all failure
// paths either record a sub-status warning or skip silently — the bucket
// loop continues to the next sub regardless).
//
// Returns the number of posts archived from this fetch and the cycle id
// the caller should pass to L2 wave scheduling so every wave row in
// prefetch_runs links back to its originating L1 fetch.
func (s *Scheduler) runOneSubFetch(ctx context.Context, tf, sub string, cursors map[string]string, exhausted map[string]bool) (int, string) {
	mode := s.resolveSubMode(sub)
	ck := cursorKey(sub, mode)
	cycleID := fmt.Sprintf("%s:%s:%d", tf, sub, time.Now().Unix())

	// A cursor that already exhausted this listing earlier in this same
	// cycle is skipped — re-issuing the same cursor would return an empty
	// page and burn a request. The exhausted map is per-cycle (cleared by
	// the caller), so the next cycle still re-walks from the head.
	if exhausted[ck] {
		return 0, cycleID
	}

	cursor := cursors[ck]
	var posts []reddit.Post
	var after string
	var fetchErr error
	limit := l1RoundLimit()
	label := fmt.Sprintf("L1 bucket=%s r/%s [%s%s] listing limit=%d (after=%q)",
		tf, sub, mode.Sort, tfSuffix(mode.Timeframe), limit, cursor)

	if err := s.submit(ctx, label, true, func(ctx context.Context) {
		if s.fetchFunc != nil {
			posts, _, after, fetchErr = s.fetchFunc(ctx, sub, mode.Sort, mode.Timeframe, cursor, limit)
		} else {
			posts, _, after, fetchErr = s.cli.FetchSubreddit(ctx, sub, mode.Sort, mode.Timeframe, cursor, "", limit)
		}
		s.recordUpstream(ctx)
		// Token() returning nil collapses three distinct conditions into
		// ErrNoTokenAvailable. Disambiguate before reacting: cold start
		// (block until first token), installed-but-unusable (short
		// backoff retry checking only local state), or genuinely missing
		// (give up this sub for the cycle). No publicCli fallback —
		// emitting an unauthenticated request from the same IP that's
		// about to carry the session token is a stealth tell.
		if errors.Is(fetchErr, reddit.ErrNoTokenAvailable) && s.tokenWaiter != nil {
			if s.tokenWaiter.TokenInstalled() {
				s.Events.Addf(LevelWarn, "L1", "bucket=%s r/%s: session token temporarily unusable, retrying up to %dx with backoff", tf, sub, l1TokenRetries)
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
						s.Events.Addf(LevelInfo, "L1", "bucket=%s r/%s: session token recovered on retry %d/%d", tf, sub, attempt+1, l1TokenRetries)
						recovered = true
						break
					}
					s.Events.Addf(LevelSkip, "L1", "bucket=%s r/%s: token still unusable after retry %d/%d", tf, sub, attempt+1, l1TokenRetries)
				}
				if !recovered {
					s.Events.Addf(LevelWarn, "L1", "bucket=%s r/%s: session token still unusable after %d retries, skipping", tf, sub, l1TokenRetries)
					if s.subStatus != nil {
						s.subStatus.RecordFailure(sub, "token temporarily unusable")
					}
					return
				}
				posts, _, after, fetchErr = s.cli.FetchSubreddit(ctx, sub, mode.Sort, mode.Timeframe, cursor, "", limit)
				s.recordUpstream(ctx)
			} else {
				s.Events.Addf(LevelWarn, "L1", "bucket=%s r/%s: no session token yet, blocking until token+UA ready", tf, sub)
				if s.tokenWaiter.WaitForToken(ctx) {
					posts, _, after, fetchErr = s.cli.FetchSubreddit(ctx, sub, mode.Sort, mode.Timeframe, cursor, "", limit)
					s.recordUpstream(ctx)
				}
			}
		}
		if fetchErr != nil {
			s.Events.Addf(LevelError, "L1", "bucket=%s r/%s: fetch failed: %v", tf, sub, fetchErr)
			if s.subStatus != nil {
				s.subStatus.RecordFailure(sub, fetchErr.Error())
			}
			return
		}
		s.Events.Addf(LevelOK, "L1", "bucket=%s r/%s: %d posts fetched (after=%q)",
			tf, sub, len(posts), after)
		if s.subStatus != nil {
			s.subStatus.MarkLive(sub)
		}
		if s.archiver != nil {
			s.archiver.ArchivePosts(posts, sub, "natural_prefetch")
		}
	}); err != nil {
		return 0, cycleID
	}

	if fetchErr != nil {
		if s.runStore != nil {
			payload, _ := json.Marshal(map[string]any{
				"sort": mode.Sort, "tf": mode.Timeframe, "limit": limit,
			})
			_ = s.runStore.Record("L1", tf, sub, "", cycleID, "fail", fetchErr.Error(), payload)
		}
		return 0, cycleID
	}

	if after != "" {
		cursors[ck] = after
	} else {
		delete(cursors, ck)
		exhausted[ck] = true
		s.Events.Addf(LevelInfo, "L1", "bucket=%s r/%s [%s%s]: no more pages this cycle", tf, sub, mode.Sort, tfSuffix(mode.Timeframe))
	}

	if s.runStore != nil {
		payload, _ := json.Marshal(map[string]any{
			"sort": mode.Sort, "tf": mode.Timeframe, "limit": limit,
			"fetched": len(posts), "after": after,
		})
		_ = s.runStore.Record("L1", tf, sub, "", cycleID, "ok", "", payload)
	}
	return len(posts), cycleID
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
	urlPath     string
	postID      string
	items       []mediaItem
	okSoFar     bool // whether the post's non-frozen items all downloaded cleanly
	numComments int  // captured at unmarshal so bind L3 can re-apply min-comments
}

// numCommentsOf returns the comment count Reddit reported at L1 fetch time.
// Comments[1] is the raw decimal string ("8"); a parse failure yields 0 so a
// missing/garbage field collapses to "below any positive threshold" rather
// than silently bypassing the guard.
func numCommentsOf(p *reddit.Post) int {
	if p == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(p.Comments[1]))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// l2WavesPerCycle is the fixed number of non-uniform sub-intervals the L2
// scheduler chops one L1 cycle period into. Each wave drains ~1/l2WavesPerCycle
// of the posts the parent L1 fetch surfaced.
const l2WavesPerCycle = 5

// waveMinGapFrac is the per-wave inter-gap floor as a fraction of the bucket
// period. With l2WavesPerCycle=5 we have 4 inter-wave gaps; reserving 10% of
// the cycle per gap leaves 60% of the period for the non-uniform random
// portion, which is still enough entropy to break a fixed-quintile pattern
// while guaranteeing no two waves bunch up inside the same 10% window.
const waveMinGapFrac = 0.10

// planWaves rolls the per-cycle stealth plan: l2WavesPerCycle time offsets
// across `period` (with a guaranteed ≥10%-of-period gap between consecutive
// waves to avoid bunching, plus a non-uniform random portion on top) and
// per-wave chunk sizes drawn from a non-uniform partition of postCount. Both
// the firing tempo *and* the per-wave request volume vary every cycle so an
// observer cannot pin either to a fixed quintile.
func planWaves(postCount int, period time.Duration) (chunks []int, offsets []time.Duration) {
	minGap := time.Duration(float64(period) * waveMinGapFrac)
	if minGap < 0 {
		minGap = 0
	}
	reserved := time.Duration(l2WavesPerCycle-1) * minGap
	randomSpan := period - reserved
	if randomSpan < 0 {
		randomSpan = 0
	}
	offsets = make([]time.Duration, l2WavesPerCycle)
	for i := range offsets {
		offsets[i] = time.Duration(rand.Float64() * float64(randomSpan))
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	// Shift each sorted offset by i*minGap so consecutive waves are always
	// ≥minGap apart, while keeping their relative spacing non-uniform.
	for i := range offsets {
		offsets[i] += time.Duration(i) * minGap
	}
	chunks = splitNonUniform(postCount, l2WavesPerCycle)
	return chunks, offsets
}

// splitNonUniform partitions postCount across `waves` bins with a non-uniform
// random distribution. iid uniform weights (floored at 0.1 so a near-zero
// draw can't crush its wave) are normalized; each wave gets at least 1 post
// when postCount ≥ waves, the rest is divided proportionally, and the result
// is shuffled so the residual slot isn't always the same wave index. With
// postCount ≤ waves, postCount waves get 1 each (still shuffled) and the
// remainder gets 0.
func splitNonUniform(postCount, waves int) []int {
	out := make([]int, waves)
	if waves <= 0 || postCount <= 0 {
		return out
	}
	if postCount <= waves {
		for i := 0; i < postCount; i++ {
			out[i] = 1
		}
		rand.Shuffle(waves, func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	weights := make([]float64, waves)
	sum := 0.0
	for i := range weights {
		weights[i] = rand.Float64() + 0.1
		sum += weights[i]
	}
	remaining := postCount - waves
	assigned := 0
	for i := 0; i < waves-1; i++ {
		share := int(weights[i] / sum * float64(remaining))
		out[i] = 1 + share
		if out[i] > l2WaveCap {
			out[i] = l2WaveCap
		}
		assigned += out[i]
	}
	last := postCount - assigned
	if last < 1 {
		last = 1
	}
	if last > l2WaveCap {
		last = l2WaveCap
	}
	out[waves-1] = last
	rand.Shuffle(waves, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// waveRunner is the per-wave fetch primitive both L2 and L3 implement.
type waveRunner func(ctx context.Context, tf, sub string, chunk int, cycleID string, subInterval int) error

// driveWaves schedules and fires the wave plan for one cycle. Shared by L2
// and L3 — extracted so standalone L3 doesn't duplicate L2's
// schedule/sleep/dispatch loop. payloadExtras is merged into each wave row's
// JSON payload (e.g. {"standalone": true} for L3 cycles). onBegin fires just
// before each wave's runWave (used by L2 to advance its live snapshot
// pointer); pass nil for L3.
func (s *Scheduler) driveWaves(
	ctx context.Context,
	layer, tf, sub, cycleID string,
	chunks []int,
	offsets []time.Duration,
	cycleStart time.Time,
	period time.Duration,
	postCount int,
	payloadExtras map[string]any,
	onBegin func(wave int),
	runWave waveRunner,
) {
	runIDs := make([]int64, len(offsets))
	for i, off := range offsets {
		if s.runStore != nil {
			payload := map[string]any{
				"chunk": chunks[i], "post_count": postCount, "wave": i + 1,
				"period_sec": int(period.Seconds()),
			}
			for k, v := range payloadExtras {
				payload[k] = v
			}
			data, _ := json.Marshal(payload)
			id, err := s.runStore.Schedule(layer, tf, sub, "", cycleID, i+1, cycleStart.Add(off), data)
			if err == nil {
				runIDs[i] = id
			}
		}
	}

	s.Events.Addf(LevelInfo, layer, "r/%s: scheduled %d waves chunks=%v over %s (post_count=%d)",
		sub, len(offsets), chunks, formatDur(period), postCount)

	for i, off := range offsets {
		target := cycleStart.Add(off)
		if w := time.Until(target); w > 0 {
			if err := sleep(ctx, w); err != nil {
				return
			}
		}
		if err := ctx.Err(); err != nil {
			return
		}
		if runIDs[i] != 0 && s.runStore != nil {
			ok, _ := s.runStore.TryMarkRunning(runIDs[i])
			if !ok {
				s.Events.Addf(LevelInfo, layer, "r/%s wave %d/%d: superseded by newer cycle — skipped",
					sub, i+1, len(offsets))
				return
			}
		}
		if onBegin != nil {
			onBegin(i + 1)
		}
		err := runWave(ctx, tf, sub, chunks[i], cycleID, i+1)
		if s.runStore != nil && runIDs[i] != 0 {
			if err != nil {
				_ = s.runStore.MarkFinished(runIDs[i], "fail", err.Error())
			} else {
				_ = s.runStore.MarkFinished(runIDs[i], "ok", "")
			}
		}
		if err != nil {
			return
		}
	}
}

// runL2Cycle schedules and drives one L1-cycle's worth of L2 waves via the
// shared driveWaves driver. The wave plan (offsets + per-wave chunks) is
// non-uniform on both axes so neither tempo nor request volume looks like a
// fixed quintile slice; see planWaves.
//
// postCount == 0 still emits a single skipped record so the unified ledger
// reflects "L1 found nothing" rather than going silent.
func (s *Scheduler) runL2Cycle(ctx context.Context, tf, sub string, postCount int, period time.Duration, cycleID string) {
	if s.postStore == nil || s.media == nil {
		return
	}
	depth := s.resolveSubDepth(sub)
	if depth == "none" {
		if s.runStore != nil {
			payload, _ := json.Marshal(map[string]any{"post_count": postCount, "reason": "depth=none"})
			_ = s.runStore.Record("L2", tf, sub, "", cycleID, "skipped", "", payload)
		}
		return
	}
	if depth == "l3" {
		s.runL3Cycle(ctx, tf, sub, postCount, period, cycleID)
		return
	}
	if postCount <= 0 {
		if s.runStore != nil {
			payload, _ := json.Marshal(map[string]any{"post_count": 0})
			_ = s.runStore.Record("L2", tf, sub, "", cycleID, "skipped", "", payload)
		}
		return
	}

	chunks, offsets := planWaves(postCount, period)
	cycleStart := time.Now()
	s.recordL2CycleStart(tf, sub, postCount, chunks, depthHasL3(depth), cycleStart, period, offsets, cycleID)
	defer s.dropL2Cycle(tf, sub)
	s.driveWaves(ctx, "L2", tf, sub, cycleID, chunks, offsets, cycleStart, period, postCount, nil,
		func(wave int) { s.advanceL2Wave(tf, sub, wave) },
		s.runL2Wave)
}

// runL2Wave drains up to `limit` pending-media posts for sub through the NP
// dispatcher, downloading every media URL each post needs. When depth is
// "l2+l3" (bind mode) every successfully-mediafied post immediately spawns a
// budget-gated L3 comment fetch so the outbound traffic pattern resembles a
// real human reading one post end-to-end before moving on instead of a
// media-only crawler. Standalone L3 (depth="l3") is handled by runL3Cycle and
// never reaches this function. Returns the first ctx error if any.
func (s *Scheduler) runL2Wave(ctx context.Context, tf, sub string, limit int, cycleID string, subInterval int) error {
	if s.postStore == nil || s.media == nil {
		return nil
	}
	depth := s.resolveSubDepth(sub)
	if !depthHasL2(depth) {
		// Reachable only on a mid-cycle settings flip away from L2; standalone
		// L3 and "none" are routed away in runL2Cycle.
		s.setL2Status("idle", "", 0)
		return nil
	}
	bindMode := depth == "l2+l3"

	pending, err := s.postStore.ListNeedingMedia(sub, limit)
	if err != nil {
		s.Events.Addf(LevelError, "L2", "r/%s: query pending media: %v", sub, err)
		return nil
	}

	if len(pending) == 0 {
		s.setL2Status("idle", sub, 0)
		return nil
	}

	s.setL2Status("downloading", sub, len(pending))
	s.Events.Addf(LevelInfo, "L2", "r/%s wave %d/%d: %d posts need media (bind_l3=%v) -- submitting to NP queue",
		sub, subInterval, l2WavesPerCycle, len(pending), bindMode)

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
				urlPath:     sp.URLPath,
				postID:      sp.PostID,
				items:       frozenItems,
				okSoFar:     allOK,
				numComments: numCommentsOf(&p),
			})
		case allOK:
			s.postStore.SetMediaDone(sp.URLPath)
			completed++
			s.Events.Addf(LevelOK, "L2", "r/%s post %s: %d media done (%s)",
				sub, sp.PostID, len(mediaItems), mediaKindSummary(mediaItems))
			s.recordL2Post(tf, sub, sp.PostID, cycleID, subInterval, len(mediaItems), "ok", "")
			if bindMode && s.l3MeetsThreshold(numCommentsOf(&p), sub, sp.PostID) {
				s.fetchL3Bound(ctx, tf, sub, sp.PostID, sp.URLPath, cycleID)
			}
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
				s.recordL2Post(tf, sub, fp.postID, cycleID, subInterval, len(fp.items), "ok", "")
				if bindMode && s.l3MeetsThreshold(fp.numComments, sub, fp.postID) {
					s.fetchL3Bound(ctx, tf, sub, fp.postID, fp.urlPath, cycleID)
				}
			}
		}
	}

	if completed > 0 {
		s.Events.Addf(LevelOK, "L2", "r/%s: media complete for %d/%d posts", sub, completed, len(pending))
	}
	s.setL2Status("idle", "", 0)
	return nil
}

// runL3Cycle is the standalone L3 path (depth="l3"): comments only, no media,
// no bind. Reuses the L2 wave planner + driver instead of duplicating the
// schedule/sleep/dispatch loop — only the per-wave fetch primitive differs.
//
// Because L3-only never marks posts media-done, the pending queue does not
// drain; each wave re-walks the same recent slice and skips posts whose
// comments are already archived. That is the documented tradeoff for
// comments-only mode — flip to l2+l3 to also drain the queue via bind.
func (s *Scheduler) runL3Cycle(ctx context.Context, tf, sub string, postCount int, period time.Duration, cycleID string) {
	if s.postStore == nil {
		return
	}
	if postCount <= 0 {
		if s.runStore != nil {
			payload, _ := json.Marshal(map[string]any{"post_count": 0})
			_ = s.runStore.Record("L3", tf, sub, "", cycleID, "skipped", "", payload)
		}
		return
	}
	chunks, offsets := planWaves(postCount, period)
	cycleStart := time.Now()
	s.driveWaves(ctx, "L3", tf, sub, cycleID, chunks, offsets, cycleStart, period, postCount,
		map[string]any{"standalone": true}, nil, s.runL3Wave)
}

// runL3Wave fetches comments for up to `limit` posts of sub via the same NP
// dispatcher + budget gate L1 uses. Standalone — no bind, no media path.
//
// Dedup is cycle-id based, not time-based: ListL3Candidates excludes any post
// whose most recent successful L3 fetch landed in the current L1 cycle or in
// the L1 cycle just before it. The result is a fixed 1-cycle freeze: a post
// archived during cycle N stays out of cycle N+1's candidate set, and
// reappears at cycle N+2 (only if L1 surfaces it again — L3 remains a slave of
// L1). The min-comments waterline is applied at the same SQL layer; posts
// below threshold are filtered out and never enter the dispatch queue.
func (s *Scheduler) runL3Wave(ctx context.Context, tf, sub string, limit int, cycleID string, subInterval int) error {
	if s.postStore == nil {
		return nil
	}
	prevCycle := ""
	if s.runStore != nil {
		var err error
		prevCycle, err = s.runStore.PreviousL1CycleID(sub, cycleID)
		if err != nil {
			s.Events.Addf(LevelWarn, "L3", "r/%s: prev L1 cycle lookup: %v", sub, err)
		}
	}
	minComments := s.resolveL3MinComments()
	pending, err := s.postStore.ListL3Candidates(sub, cycleID, prevCycle, limit, minComments)
	if err != nil {
		s.Events.Addf(LevelError, "L3", "r/%s: query candidates: %v", sub, err)
		return nil
	}
	if len(pending) == 0 {
		s.Events.Addf(LevelInfo, "L3", "r/%s wave %d/%d: 0 eligible posts (freeze + min_comments=%d) — skipped",
			sub, subInterval, l2WavesPerCycle, minComments)
		return nil
	}
	s.Events.Addf(LevelInfo, "L3", "r/%s wave %d/%d: %d posts -- fetching comments (standalone, prev_cycle=%q, min_comments=%d)",
		sub, subInterval, l2WavesPerCycle, len(pending), prevCycle, minComments)
	for _, sp := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.fetchL3Standalone(ctx, tf, sub, sp.PostID, sp.URLPath, cycleID)
	}
	return nil
}

// l3MeetsThreshold returns true when numComments clears the L3 min-comments
// waterline. A miss is logged + recorded as 'skipped' in prefetch_runs so the
// debug ledger shows the post was *considered but frozen*, not silently dropped.
func (s *Scheduler) l3MeetsThreshold(numComments int, sub, postID string) bool {
	min := s.resolveL3MinComments()
	if min <= 0 || numComments >= min {
		return true
	}
	s.Events.Addf(LevelSkip, "L3", "r/%s post %s: frozen — %d comments < threshold %d",
		sub, postID, numComments, min)
	if s.runStore != nil {
		payload, _ := json.Marshal(map[string]any{
			"reason":       "min_comments",
			"num_comments": numComments,
			"min_comments": min,
		})
		_ = s.runStore.Record("L3", "", sub, postID, "", "skipped", "", payload)
	}
	return false
}

// resolveL3MinComments reads the operator-set minimum comment count below
// which a post is frozen out of standalone L3. Returns 0 (disable filter) when
// the setting is missing or invalid; the settings save path and main.go's env
// validator both refuse negative values, so an invalid value reaching this
// helper can only come from manual DB tampering.
func (s *Scheduler) resolveL3MinComments() int {
	if s.settings == nil {
		return 0
	}
	v := strings.TrimSpace(s.settings.Get("prefetch_l3_min_comments"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// resolveSubDepth returns the canonical NP depth ("none"|"l2"|"l3"|"l2+l3")
// for sub. Per-sub depth: override in prefetch_sub_modes wins; otherwise the
// global prefetch_default_depth applies; absent that, "l2+l3" (full visit-like
// flow) is the hard-coded fallback so a fresh install behaves like the legacy
// "bind mode on" default. Unknown values collapse to "l2+l3" rather than
// silently dropping all media+comments traffic.
func (s *Scheduler) resolveSubDepth(sub string) string {
	const fallback = "l2+l3"
	if s.settings == nil {
		return fallback
	}
	target := strings.ToLower(strings.TrimSpace(sub))
	if raw := s.settings.Get("prefetch_sub_modes"); raw != "" && target != "" {
		for _, clause := range splitTopLevelClauses(raw) {
			clause = strings.TrimSpace(clause)
			eq := strings.IndexByte(clause, '=')
			if eq < 0 {
				continue
			}
			if strings.ToLower(strings.TrimSpace(clause[:eq])) != target {
				continue
			}
			body := strings.TrimSpace(clause[eq+1:])
			for _, kv := range strings.Split(body, "&") {
				colon := strings.IndexByte(kv, ':')
				if colon < 0 {
					continue
				}
				key := strings.ToLower(strings.TrimSpace(kv[:colon]))
				val := strings.ToLower(strings.TrimSpace(kv[colon+1:]))
				if key == "depth" || key == "d" {
					if c, ok := canonDepth(val); ok {
						return c
					}
				}
			}
			break
		}
	}
	if v := strings.TrimSpace(s.settings.Get("prefetch_default_depth")); v != "" {
		if c, ok := canonDepth(v); ok {
			return c
		}
	}
	return fallback
}

// splitTopLevelClauses mirrors handler.splitTopLevelClauses — splits on '+'
// except when the '+' is the middle of a depth value (`depth:l2+l3`). The
// scheduler must use the same context-aware split as the form-save path, or
// `golang=depth:l2+l3` becomes `golang=depth:l2` plus a bare `l3` and the depth
// override silently drops to "l2".
func splitTopLevelClauses(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != '+' {
			continue
		}
		if isDepthValueJoin(raw, start, i) {
			continue
		}
		out = append(out, raw[start:i])
		start = i + 1
	}
	out = append(out, raw[start:])
	return out
}

func isDepthValueJoin(raw string, segStart, plus int) bool {
	j := plus - 1
	for j >= segStart && raw[j] != '=' && raw[j] != '&' {
		j--
	}
	if j < segStart {
		return false
	}
	kv := strings.ToLower(raw[j+1 : plus])
	switch kv {
	case "depth:l2", "depth:l3", "d:l2", "d:l3":
	default:
		return false
	}
	k := plus + 1
	for k < len(raw) && raw[k] != '&' && raw[k] != '+' {
		k++
	}
	tail := strings.ToLower(raw[plus+1 : k])
	return tail == "l2" || tail == "l3"
}

// canonDepth mirrors handler.CanonicalizeDepth without the package import to
// avoid a cycle (handler imports prefetch indirectly via render/types).
func canonDepth(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	v = strings.ReplaceAll(v, " ", "")
	switch v {
	case "none", "off":
		return "none", true
	case "l1":
		return "none", true
	case "l2":
		return "l2", true
	case "l3":
		return "l3", true
	case "l2+l3", "l3+l2":
		return "l2+l3", true
	}
	return "", false
}

func depthHasL2(d string) bool { return d == "l2" || d == "l2+l3" }
func depthHasL3(d string) bool { return d == "l3" || d == "l2+l3" }

// globalBindDefaultEnabled reports whether the global prefetch_default_depth
// covers L3 — used only by the /debug status view to label the legacy
// "bind mode" panel. Per-sub overrides may still flip individual subs in
// either direction; this is just the user-visible default.
func (s *Scheduler) globalBindDefaultEnabled() bool {
	if s.settings == nil {
		return true
	}
	v := strings.TrimSpace(s.settings.Get("prefetch_default_depth"))
	if v == "" {
		return true // fallback default is l2+l3
	}
	c, ok := canonDepth(v)
	if !ok {
		return true
	}
	return depthHasL3(c)
}

// recordL2Post writes one prefetch_runs row per L2 post completion so the
// unified ledger has a row-per-post granularity in addition to the wave-level
// schedule rows. Nil-safe for tests.
func (s *Scheduler) recordL2Post(tf, sub, postID, cycleID string, subInterval, mediaCount int, status, errStr string) {
	if s.runStore == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"wave":        subInterval,
		"media_count": mediaCount,
	})
	// We log post-level rows with no sub_interval column (the wave row already
	// captured that) so a later query can SUM media_count grouped by cycle_id
	// without double-counting wave rows.
	_ = s.runStore.Record("L2", tf, sub, postID, cycleID, status, errStr, payload)
}

// fetchL3Bound is the single-post L3 fetch primitive used by both the bind
// pipeline (depth="l2+l3", invoked from runL2Wave after each successful media
// download) and the standalone L3 cycle (depth="l3", invoked directly from
// runL3Wave). It synchronously fetches a post's comments through the NP
// dispatcher using the same budget gate L1 uses (`needsBudget=true`). This
// keeps combined traffic from exceeding the 10-minute OAuth window —
// waitForBudget pauses the dispatch loop the moment remaining quota crosses
// the reserved threshold, so a busy cycle naturally stretches across
// multiple windows instead of burning the entire budget in one burst.
//
// Failures are logged but never propagated: an L3 miss must not block the
// surrounding wave's remaining posts.
func (s *Scheduler) fetchL3Bound(ctx context.Context, tf, sub, postID, urlPath, cycleID string) {
	s.fetchL3Single(ctx, tf, sub, postID, urlPath, cycleID, true)
}

// fetchL3Standalone is the depth="l3" entry — same primitive as bind, but the
// ledger row records bind=false so the L3 cycle stays distinguishable from
// L2-driven bind fetches in the unified prefetch_runs view.
func (s *Scheduler) fetchL3Standalone(ctx context.Context, tf, sub, postID, urlPath, cycleID string) {
	s.fetchL3Single(ctx, tf, sub, postID, urlPath, cycleID, false)
}

func (s *Scheduler) fetchL3Single(ctx context.Context, tf, sub, postID, urlPath, cycleID string, bound bool) {
	if s.cli == nil {
		return
	}
	commentSort := "confidence"
	if s.settings != nil {
		if v := strings.TrimSpace(s.settings.Get("comment_sort")); v != "" {
			commentSort = v
		}
	}

	mode := "bind"
	if !bound {
		mode = "solo"
	}
	var fetched int
	var fetchErr error
	label := fmt.Sprintf("L3 %s r/%s post %s", mode, sub, postID)
	if err := s.submit(ctx, label, true, func(ctx context.Context) {
		_, comments, err := s.cli.FetchPost(ctx, sub, postID, commentSort)
		s.recordUpstream(ctx)
		if err != nil {
			fetchErr = err
			s.Events.Addf(LevelWarn, "L3", "%s r/%s post %s: fetch failed: %v", mode, sub, postID, err)
			return
		}
		if s.archiver != nil && urlPath != "" {
			s.archiver.ArchiveComments(urlPath, comments)
		}
		fetched = len(comments)
		s.Events.Addf(LevelOK, "L3", "%s r/%s post %s: archived %d comments", mode, sub, postID, fetched)
	}); err != nil {
		return
	}

	status := "ok"
	errStr := ""
	if fetchErr != nil {
		status = "fail"
		errStr = fetchErr.Error()
	}
	if s.runStore != nil {
		payload, _ := json.Marshal(map[string]any{
			"bind":     bound,
			"comments": fetched,
		})
		_ = s.runStore.Record("L3", tf, sub, postID, cycleID, status, errStr, payload)
	}

	// Surface L3 in the same /debug panel handler-initiated L3 uses. The recent
	// ring buffer logs both bind and standalone fetches so the operator can
	// see exactly what the comment pipeline has been touching.
	s.statusMu.Lock()
	s.l3Phase = "active"
	s.l3Current = fmt.Sprintf("%s r/%s/%s (%d cmts)", mode, sub, postID, fetched)
	s.l3LastAt = time.Now()
	s.l3Count++
	s.statusMu.Unlock()
	s.recordL3Bind(sub, postID, fetched, status)
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

// debugAbsTimeThreshold is the minimum |duration| (relative to now) at which
// the /debug status panels supplement a "in 2h30m" / "5m ago" relative string
// with a second-line absolute timestamp. Anything closer than this is already
// scannable as a relative offset.
const debugAbsTimeThreshold = time.Hour

// absTimeIfLarge returns the wall-clock timestamp formatted for /debug when
// |now - t| ≥ debugAbsTimeThreshold, else "". Mirrors the log table's split
// date/clock writing — caller renders the returned string as a second line.
func absTimeIfLarge(t time.Time, delta time.Duration) string {
	if t.IsZero() {
		return ""
	}
	if delta < 0 {
		delta = -delta
	}
	if delta < debugAbsTimeThreshold {
		return ""
	}
	return t.Format("2006-01-02 15:04:05 MST")
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
	L1NextCycle    string
	L1NextCycleAbs string
	// L1Buckets is the per-bucket schedule snapshot for the debug page. One
	// entry per active bucket, ordered finest-to-coarsest (hour → all).
	L1Buckets    []PrefetchBucketStatus
	L2Phase      string
	L2Sub        string
	L2Pending    int
	L2BindMode   bool
	L2Cycles     []PrefetchL2Cycle
	L5Phase      string
	L5Current    string
	L5Pending    int
	L3Phase      string
	L3Current    string
	L3LastAt     string
	L3LastAtAbs  string
	L3Count      int
	L3BindMode   bool
	L3Recent     []PrefetchL3Bind
	L4Phase      string
	L4Current    string
	L4QueueLen   int
	L4NextTick   string
	L4NextTickAbs string
	NPPhase     string
	NPCurrent   string
	QueueLen    int
	Enabled     bool
	ActiveSubs  []string
}

type PrefetchBucketStatus struct {
	TF           string
	Period       string
	Subs         []string
	Cursors      map[string]string
	NextCycle    string
	NextCycleAbs string
}

// PrefetchL2Cycle is the per-(tf,sub) live L2 cycle view. WaveOffsets are the
// offsets-from-cycle-start (formatted) for each of the l2WavesPerCycle waves;
// WaveIntervals is the gap between consecutive waves with each wave's chunk
// size appended ("2h19m11s ×16") so the operator sees the non-uniform timing
// and non-uniform per-wave volume together on one line.
// CurrentWave is 1-based: 0 = waiting on wave 1, l2WavesPerCycle = all fired.
type PrefetchL2Cycle struct {
	TF            string
	Sub           string
	PostCount     int
	WaveCount     int
	CurrentWave   int
	BindMode      bool
	StartedAgo    string
	StartedAtAbs  string
	Period        string
	WaveOffsets   []string
	WaveIntervals []string
	CycleID       string
}

// PrefetchL3Bind is one historical bind-mode L3 record surfaced on /debug.
type PrefetchL3Bind struct {
	Sub      string
	PostID   string
	Comments int
	Ago      string
	AtAbs    string
	Status   string
}

// l2CycleSnap is the in-memory snapshot maintained by runL2Cycle and read
// by the status assembler. Mutated under statusMu.
type l2CycleSnap struct {
	tf          string
	sub         string
	postCount   int
	waveChunks  []int
	bindMode    bool
	cycleStart  time.Time
	period      time.Duration
	waveOffsets []time.Duration
	currentWave int
	cycleID     string
}

// l3BindRecord is one entry in the bind-mode L3 ring buffer.
type l3BindRecord struct {
	sub      string
	postID   string
	comments int
	at       time.Time
	status   string
}

// l3BindRecentCap is the ring-buffer depth for surfacing recent bind-mode
// L3 fetches on /debug. Kept small so the panel stays scannable.
const l3BindRecentCap = 8

func (s *Scheduler) Status() PrefetchStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()

	// Build the aggregate L1 view from the per-bucket snapshots when
	// available; fall back to the older single-cycle fields otherwise.
	cursors := make(map[string]string)
	var earliest time.Time
	var buckets []PrefetchBucketStatus
	for _, tf := range bucketOrder {
		b, ok := s.l1Buckets[tf]
		if !ok {
			continue
		}
		for k, v := range b.Cursors {
			cursors[k] = v
		}
		if earliest.IsZero() || b.NextCycleAt.Before(earliest) {
			earliest = b.NextCycleAt
		}
		var nc, ncAbs string
		if !b.NextCycleAt.IsZero() {
			d := time.Until(b.NextCycleAt)
			if d > 0 {
				nc = "in " + formatDur(d)
			} else {
				nc = "now"
			}
			ncAbs = absTimeIfLarge(b.NextCycleAt, d)
		}
		buckets = append(buckets, PrefetchBucketStatus{
			TF:           b.TF,
			Period:       formatDur(b.Period),
			Subs:         append([]string(nil), b.Subs...),
			Cursors:      copyMap(b.Cursors),
			NextCycle:    nc,
			NextCycleAbs: ncAbs,
		})
	}
	if len(cursors) == 0 {
		for k, v := range s.l1Cursors {
			cursors[k] = v
		}
	}
	if earliest.IsZero() {
		earliest = s.l1NextCycle
	}

	var nextCycle, nextCycleAbs string
	if !earliest.IsZero() {
		d := time.Until(earliest)
		if d > 0 {
			nextCycle = "in " + formatDur(d)
		} else {
			nextCycle = "now"
		}
		nextCycleAbs = absTimeIfLarge(earliest, d)
	}

	subs := make([]string, len(s.l1Subs))
	copy(subs, s.l1Subs)

	var l3LastAt, l3LastAtAbs string
	if !s.l3LastAt.IsZero() {
		d := time.Since(s.l3LastAt)
		if d > 0 {
			l3LastAt = formatDur(d) + " ago"
		}
		l3LastAtAbs = absTimeIfLarge(s.l3LastAt, d)
	}
	var l4NextTick, l4NextTickAbs string
	if !s.l4NextTickAt.IsZero() {
		d := time.Until(s.l4NextTickAt)
		if d > 0 {
			l4NextTick = "in " + formatDur(d)
		} else {
			l4NextTick = "now"
		}
		l4NextTickAbs = absTimeIfLarge(s.l4NextTickAt, d)
	}

	return PrefetchStatus{
		L1Phase:     s.l1Phase,
		L1Round:     s.l1Round,
		L1MaxRounds: s.l1MaxRounds,
		L1Subs:      subs,
		L1Cursors:   cursors,
		L1NextCycle:    nextCycle,
		L1NextCycleAbs: nextCycleAbs,
		L1Buckets:      buckets,
		L2Phase:     s.l2Phase,
		L2Sub:       s.l2Sub,
		L2Pending:   s.l2Pending,
		L2BindMode:  s.globalBindDefaultEnabled(),
		L2Cycles:    s.snapshotL2Cycles(),
		L5Phase:     s.l5Phase,
		L5Current:   s.l5Current,
		L5Pending:   s.l5Pending,
		L3Phase:     s.l3Phase,
		L3Current:   s.l3Current,
		L3LastAt:    l3LastAt,
		L3LastAtAbs: l3LastAtAbs,
		L3Count:     s.l3Count,
		L3BindMode:  s.globalBindDefaultEnabled(),
		L3Recent:    s.snapshotL3Recent(),
		L4Phase:     s.l4Phase,
		L4Current:   s.l4Current,
		L4QueueLen:  s.l4QueueLen,
		L4NextTick:    l4NextTick,
		L4NextTickAbs: l4NextTickAbs,
		NPPhase:     s.npPhase,
		NPCurrent:   s.npCurrent,
		QueueLen:    len(s.queue),
		Enabled:     s.isEnabled(),
		ActiveSubs:  s.activeSubs(),
	}
}

// recordBucketStatus updates the live per-bucket schedule view for /debug.
// Snapshotted under the same lock as the rest of the status struct so the
// debug renderer never sees a torn view.
func (s *Scheduler) recordBucketStatus(tf string, nextCycleAt time.Time, period time.Duration, subs []string, cursors map[string]string) {
	cs := make(map[string]string, len(cursors))
	for k, v := range cursors {
		cs[k] = v
	}
	ss := append([]string(nil), subs...)
	s.statusMu.Lock()
	if s.l1Buckets == nil {
		s.l1Buckets = make(map[string]*bucketStatus, len(bucketOrder))
	}
	s.l1Buckets[tf] = &bucketStatus{
		TF:          tf,
		NextCycleAt: nextCycleAt,
		Period:      period,
		Subs:        ss,
		Cursors:     cs,
	}
	s.statusMu.Unlock()
}

// dropBucketStatus clears the bucket from the live view when the coordinator
// retires its loop (settings change / disable). Keeps stale buckets from
// claiming to be "running" after they were torn down.
func (s *Scheduler) dropBucketStatus() {
	s.statusMu.Lock()
	s.l1Buckets = nil
	s.statusMu.Unlock()
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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

// recordL2CycleStart registers a freshly-launched L2 cycle's wave schedule so
// the /debug panel can display the precise non-uniform interval distribution
// (and per-wave chunk size) rolled for this cycle and the current wave pointer.
func (s *Scheduler) recordL2CycleStart(tf, sub string, postCount int, chunks []int, bindMode bool, cycleStart time.Time, period time.Duration, offsets []time.Duration, cycleID string) {
	off := append([]time.Duration(nil), offsets...)
	ch := append([]int(nil), chunks...)
	s.statusMu.Lock()
	if s.l2Cycles == nil {
		s.l2Cycles = make(map[string]*l2CycleSnap)
	}
	s.l2Cycles[l2CycleKey(tf, sub)] = &l2CycleSnap{
		tf:          tf,
		sub:         sub,
		postCount:   postCount,
		waveChunks:  ch,
		bindMode:    bindMode,
		cycleStart:  cycleStart,
		period:      period,
		waveOffsets: off,
		currentWave: 0,
		cycleID:     cycleID,
	}
	s.statusMu.Unlock()
}

// advanceL2Wave bumps the live wave pointer once a wave begins firing.
func (s *Scheduler) advanceL2Wave(tf, sub string, wave int) {
	s.statusMu.Lock()
	if c, ok := s.l2Cycles[l2CycleKey(tf, sub)]; ok {
		c.currentWave = wave
	}
	s.statusMu.Unlock()
}

// dropL2Cycle removes the cycle entry once all waves have fired (or the cycle
// aborted) so /debug only shows in-flight cycles.
func (s *Scheduler) dropL2Cycle(tf, sub string) {
	s.statusMu.Lock()
	if s.l2Cycles != nil {
		delete(s.l2Cycles, l2CycleKey(tf, sub))
	}
	s.statusMu.Unlock()
}

func l2CycleKey(tf, sub string) string { return tf + "|" + sub }

// snapshotL2Cycles materializes the live L2 cycle map into a stable, sorted
// slice for the /debug panel. Caller already holds statusMu (RLock or Lock).
func (s *Scheduler) snapshotL2Cycles() []PrefetchL2Cycle {
	if len(s.l2Cycles) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.l2Cycles))
	for k := range s.l2Cycles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]PrefetchL2Cycle, 0, len(keys))
	for _, k := range keys {
		c := s.l2Cycles[k]
		offsets := make([]string, len(c.waveOffsets))
		intervals := make([]string, len(c.waveOffsets))
		var prev time.Duration
		for i, off := range c.waveOffsets {
			offsets[i] = "+" + formatDur(off)
			gap := off - prev
			if gap < 0 {
				gap = 0
			}
			label := formatDur(gap)
			if i < len(c.waveChunks) {
				label = fmt.Sprintf("%s ×%d", label, c.waveChunks[i])
			}
			intervals[i] = label
			prev = off
		}
		var startedAgo, startedAtAbs string
		if !c.cycleStart.IsZero() {
			d := time.Since(c.cycleStart)
			startedAgo = formatDur(d) + " ago"
			startedAtAbs = absTimeIfLarge(c.cycleStart, d)
		}
		out = append(out, PrefetchL2Cycle{
			TF:            c.tf,
			Sub:           c.sub,
			PostCount:     c.postCount,
			WaveCount:     len(c.waveOffsets),
			CurrentWave:   c.currentWave,
			BindMode:      c.bindMode,
			StartedAgo:    startedAgo,
			StartedAtAbs:  startedAtAbs,
			Period:        formatDur(c.period),
			WaveOffsets:   offsets,
			WaveIntervals: intervals,
			CycleID:       c.cycleID,
		})
	}
	return out
}

// recordL3Bind appends a bind-mode L3 entry to the FIFO ring buffer surfaced
// on /debug. Newest first; oldest entries fall off when the cap is reached.
func (s *Scheduler) recordL3Bind(sub, postID string, comments int, status string) {
	rec := l3BindRecord{
		sub:      sub,
		postID:   postID,
		comments: comments,
		at:       time.Now(),
		status:   status,
	}
	s.statusMu.Lock()
	s.l3BindRecent = append([]l3BindRecord{rec}, s.l3BindRecent...)
	if len(s.l3BindRecent) > l3BindRecentCap {
		s.l3BindRecent = s.l3BindRecent[:l3BindRecentCap]
	}
	s.statusMu.Unlock()
}

// snapshotL3Recent copies the recent-bind ring buffer into a view-safe slice.
// Caller already holds statusMu (RLock or Lock).
func (s *Scheduler) snapshotL3Recent() []PrefetchL3Bind {
	if len(s.l3BindRecent) == 0 {
		return nil
	}
	out := make([]PrefetchL3Bind, len(s.l3BindRecent))
	for i, r := range s.l3BindRecent {
		var ago, atAbs string
		if !r.at.IsZero() {
			d := time.Since(r.at)
			ago = formatDur(d) + " ago"
			atAbs = absTimeIfLarge(r.at, d)
		}
		out[i] = PrefetchL3Bind{
			Sub:      r.sub,
			PostID:   r.postID,
			Comments: r.comments,
			Ago:      ago,
			AtAbs:    atAbs,
			Status:   r.status,
		}
	}
	return out
}

func (s *Scheduler) setL5Status(phase, current string, pending int) {
	s.statusMu.Lock()
	s.l5Phase = phase
	s.l5Current = current
	s.l5Pending = pending
	s.statusMu.Unlock()
}

// RecordL3Fetch surfaces an on-demand comment fetch (deep-archive layer) on
// the /debug page. Handler-side: called after a successful comment retrieval
// regardless of whether it was an initial post view, a manual refresh, or a
// "load more" expansion. The scheduler never initiates L3 itself — this is
// pure passive bookkeeping for visibility.
func (s *Scheduler) RecordL3Fetch(sub, postID string, commentCount int) {
	target := fmt.Sprintf("r/%s/%s (%d cmts)", sub, postID, commentCount)
	s.statusMu.Lock()
	s.l3Phase = "active"
	s.l3Current = target
	s.l3LastAt = time.Now()
	s.l3Count++
	s.statusMu.Unlock()
	if s.Events != nil {
		s.Events.Addf(LevelOK, "L3", "%s: archived %d comments on demand", target, commentCount)
	}
	if s.runStore != nil {
		payload, _ := json.Marshal(map[string]any{
			"bind":     false,
			"comments": commentCount,
		})
		_ = s.runStore.Record("L3", "", sub, postID, "", "ok", "", payload)
	}
	// Drop "active" back to idle in the background so the panel doesn't read
	// "active" forever for a single user click.
	go func() {
		time.Sleep(2 * time.Second)
		s.statusMu.Lock()
		if s.l3Current == target {
			s.l3Phase = "idle"
		}
		s.statusMu.Unlock()
	}()
}

func (s *Scheduler) setL4Status(phase, current string, queueLen int) {
	s.statusMu.Lock()
	s.l4Phase = phase
	s.l4Current = current
	s.l4QueueLen = queueLen
	s.statusMu.Unlock()
}

func (s *Scheduler) setL4NextTick(t time.Time) {
	s.statusMu.Lock()
	s.l4NextTickAt = t
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
