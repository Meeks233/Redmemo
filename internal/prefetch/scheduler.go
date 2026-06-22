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
	archiver    *archive.Service
	media       MediaDownloader
	subStatus   SubStatusChecker
	postStore   *store.PostStore
	runStore    *store.PrefetchRunStore
	iconStore   SubIconProvider
	hr          HRRecorder
	Events      *EventLog

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

	// pendingMediaFn, when non-nil, replaces s.postStore.ListNeedingMedia so
	// runL2Wave's media/bind-L3 branching can be exercised without a DB.
	// Tests feed a fixed slice of pending posts (mix of media + text) to
	// assert which posts trigger a bound L3 fetch.
	pendingMediaFn func(sub string, limit int) ([]*store.StoredPost, error)

	// commentMediaFn, when non-nil, replaces archiver.CommentMediaURLs so
	// runL2Wave's comment-image harvest can be exercised without an archive
	// service / comment store. Tests return a fixed URL slice per post path.
	commentMediaFn func(urlPath string) []string

	// l3FetchFn, when non-nil, replaces the cli.FetchPost + archiver.ArchiveComments
	// network+persist step inside fetchL3Single. It returns the archived comment
	// count. Tests record (sub, postID) to assert exactly which posts the L3
	// pipeline fetched — independent of whether the post carried media.
	l3FetchFn func(ctx context.Context, sub, postID, urlPath string) (int, error)

	// l3CandidatesFn, when non-nil, replaces s.postStore.ListL3Candidates so the
	// standalone L3 wave can be exercised without a DB. Tests feed a fixed slice
	// of eligible posts to assert which ones the L3 pipeline fetches under the
	// min-comments gate, independent of any media state.
	l3CandidatesFn func(sub, cycleID, prevCycleID string, limit, minComments int) ([]*store.StoredPost, error)

	// postCountFn, when non-nil, replaces s.postStore.CountBySubreddit so the
	// brand-new-sub reconcile gate (resumeOrRegenerate) can be exercised without
	// a DB. Tests return a fixed archived-post count per sub.
	postCountFn func(sub string) (int, error)

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
	l2Phase   string
	l2Sub     string
	l2Pending int
	// l2Cycles is the live wave-schedule view per active L2 cycle, keyed
	// by "tf|sub". Populated when runL2Cycle starts a fresh cycle for an
	// L1 fetch, advanced as each wave fires, and cleared on completion.
	l2Cycles map[string]*l2CycleSnap
	// l3Cycles mirrors l2Cycles for the self-standing L3 comment layer so
	// /debug surfaces the scheduled L3 wave plan (offsets, per-wave chunks,
	// current wave) the moment a cycle is scheduled — even before the first
	// wave fires. Without it an enabled L3 shows only Phase "—" between its
	// 12-hourly L1-triggered cycles and looks dead.
	l3Cycles  map[string]*l2CycleSnap
	l5Phase   string
	l5Current string
	l5Pending int
	// L3 — deep archive (comments). Handler-initiated, on-demand only; the
	// scheduler tracks it for /debug visibility but never schedules it itself.
	l3Phase   string
	l3Current string
	l3LastAt  time.Time
	l3Count   int
	// l3BindRecent is a small FIFO of the most recent bind-mode L3 fetches
	// (newest first). Surfaced on /debug so the operator can see exactly
	// which posts the binding pipeline has just archived.
	l3BindRecent []l3BindRecord
	// L4 — icon cache loop. Updated by iconLoop/runIconBatch so /debug shows
	// the live round queue, current sub, and next tick eta even between
	// hourly batches.
	l4Phase      string
	l4Current    string
	l4QueueLen   int
	l4NextTickAt time.Time
	npPhase      string
	npCurrent    string
	// Reclaim status: visible on /debug while driveReclaimedCycle is active.
	// reclaimL{2,3}Sub records which sub the banner is for so Status() can
	// re-check its CURRENT depth at render time and suppress a stale "recovering"
	// banner if the layer was disabled after the driver parked on a future wave.
	reclaimL2Phase string
	reclaimL2Info  string
	reclaimL2Sub   string
	reclaimL3Phase string
	reclaimL3Info  string
	reclaimL3Sub   string

	// drivers counts the live wave-driving goroutines per "layer|tf|sub" so
	// reconcileLoop can tell whether a layer is actively firing (a healthy live
	// or recovery cycle) or has gone quiet (paused by a mid-cycle disable, or
	// never started). Guarded by its own driverMu so a slow status read on
	// statusMu never blocks a driver enter/exit.
	drivers  map[string]int
	driverMu sync.Mutex
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
		archiver:    archiver,
		media:       media,
		subStatus:   subStatus,
		postStore:   postStore,
		runStore:    runStore,
		iconStore:   iconStore,
		hr:          hr,
		Events:      NewEventLog(200),
		queue:       make(chan *workItem, 1),
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
	go s.reconcileLoop(ctx)
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
		layer   string
		tf, sub string
		cycleID string
		runs    []store.PrefetchRun
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

		// Skip groups whose layer the sub no longer runs (depth changed since
		// the waves were scheduled). Retire any still-pending rows and do NOT
		// rebuild the L2 cycle snapshot — otherwise /debug would show a phantom
		// L2 cycle + "L2 recovering" for an L3-only sub. driveReclaimedCycle has
		// the same guard, but bailing here also avoids the wasted snapshot work
		// and cleans the ledger up front.
		if !depthCoversLayer(g.layer, s.resolveSubDepth(g.sub)) {
			retired := 0
			for _, r := range g.runs {
				if r.Status == "pending" {
					if err := s.runStore.MarkFinished(r.ID, "skipped", "depth no longer covers "+g.layer); err == nil {
						retired++
					}
				}
			}
			if retired > 0 {
				s.Events.Addf(LevelInfo, "init", "reclaim: r/%s depth no longer covers %s — retired %d orphaned pending wave(s), not revived",
					g.sub, g.layer, retired)
			}
			continue
		}

		// Hangover cleanup: discard a persisted L3 cycle whose plan is malformed
		// (see l3PlanHangover) instead of recovering it. The old fixed-5-wave
		// planner — or a conflicting/partial DB write — leaves a plan that no
		// longer matches planL3Waves; replaying it would re-show the wrong wave
		// count every restart. Retire its pending rows so the next L1 / reconcile
		// rolls a fresh, correctly-dispersed cycle.
		if g.layer == "L3" {
			l1Count, _ := s.runStore.LastCyclePostCount(g.tf, g.sub)
			if reason, bad := l3PlanHangover(g.runs, l1Count); bad {
				retired := 0
				for _, r := range g.runs {
					if r.Status == "pending" {
						if err := s.runStore.MarkFinished(r.ID, "skipped", "discarded malformed L3 cycle: "+reason); err == nil {
							retired++
						}
					}
				}
				s.Events.Addf(LevelWarn, "init", "reclaim: r/%s discarded malformed L3 cycle %s (%s) — retired %d pending wave(s), not revived",
					g.sub, g.cycleID, reason, retired)
				continue
			}
		}

		s.rebuildL2CycleSnapshot(g.layer, g.tf, g.sub, g.cycleID, g.runs)
		var pending []store.PrefetchRun
		for _, r := range g.runs {
			if r.Status == "pending" {
				pending = append(pending, r)
			}
		}
		if len(pending) == 0 {
			continue
		}
		sort.Slice(pending, func(i, j int) bool {
			return pending[i].SubInterval.Int32 < pending[j].SubInterval.Int32
		})
		totalPending += len(pending)
		go s.driveReclaimedCycle(ctx, g.layer, g.tf, g.sub, pending)
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
func (s *Scheduler) rebuildL2CycleSnapshot(layer, tf, sub, cycleID string, runs []store.PrefetchRun) {
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
	dst := &s.l2Cycles
	if layer == "L3" {
		dst = &s.l3Cycles
	}
	if *dst == nil {
		*dst = make(map[string]*l2CycleSnap)
	}
	(*dst)[l2CycleKey(tf, sub)] = &l2CycleSnap{
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
// driveReclaimedCycle drives a group of reclaimed waves for one (layer, cycle)
// sequentially. Past-due waves fire immediately but one at a time — never in
// parallel — so the NP queue sees a normal serial cadence rather than a burst
// of concurrent submissions. If any wave is superseded by a newer L1 cycle,
// the remaining waves in this group are skipped (they share the same cycle_id
// and would all be superseded too).
func (s *Scheduler) driveReclaimedCycle(ctx context.Context, layer, tf, sub string, waves []store.PrefetchRun) {
	s.driverEnter(layer, tf, sub)
	defer s.driverExit(layer, tf, sub)
	// Orphan guard: if the sub's depth was changed away from this layer since
	// these waves were scheduled (e.g. flipped to depth=l3, leaving stale L2
	// waves behind), retire them instead of advertising a phantom "<layer>
	// recovering" on /debug. We must NOT call setReclaimStatus here — that's the
	// field /debug renders, and the whole point is that an L3-only sub shows no
	// L2 recovery.
	if !depthCoversLayer(layer, s.resolveSubDepth(sub)) {
		if s.runStore != nil {
			for _, w := range waves {
				_ = s.runStore.MarkFinished(w.ID, "skipped", "depth no longer covers "+layer)
			}
		}
		s.Events.Addf(LevelInfo, layer, "reclaim r/%s: depth no longer covers %s — %d orphaned wave(s) retired, not recovered",
			sub, layer, len(waves))
		return
	}

	total := len(waves)
	overdue := 0
	for _, w := range waves {
		if time.Until(w.ScheduledAt) <= 0 {
			overdue++
		}
	}
	s.Events.Addf(LevelInfo, layer, "reclaim r/%s: driving %d wave(s) sequentially (%d overdue, %d future)",
		sub, total, overdue, total-overdue)

	superseded := false
	for i, r := range waves {
		if err := ctx.Err(); err != nil {
			return
		}
		// Aggressive live disable: if the operator switched this layer off
		// mid-recovery, stop driving at once and clear the "recovering" banner —
		// but leave the remaining waves pending (do NOT retire them), so flipping
		// the layer back on resumes the same plan via reconcileLayers.
		if !depthCoversLayer(layer, s.resolveSubDepth(sub)) {
			s.Events.Addf(LevelInfo, layer, "reclaim r/%s: %s disabled mid-recovery — paused, %d wave(s) left pending",
				sub, layer, total-i)
			s.clearReclaimStatus(layer)
			return
		}
		remaining := total - i
		s.setReclaimStatus(layer, sub, i+1, total, remaining)
		if !s.resumePendingWave(ctx, r) {
			s.Events.Addf(LevelInfo, layer, "reclaim r/%s: cycle superseded at wave %d/%d — discarding %d remaining wave(s)",
				sub, i+1, total, remaining-1)
			superseded = true
			break
		}
	}
	s.clearReclaimStatus(layer)
	// L3 sizes its wave count to the work, so the final wave index varies per
	// cycle; drop the live snapshot here on natural completion rather than via a
	// fixed l2WavesPerCycle threshold inside resumePendingWave. Supersession
	// already dropped it; a disable-pause returned early and deliberately kept it.
	if layer == "L3" && !superseded && len(waves) > 0 {
		s.dropL3Cycle(tf, sub, waves[len(waves)-1].CycleID.String)
	}
}

// resumePendingWave fires a single reclaimed wave. Returns true if the caller
// should continue driving subsequent waves; false if this wave was superseded
// (the entire cycle is stale and remaining waves should be discarded).
func (s *Scheduler) resumePendingWave(ctx context.Context, r store.PrefetchRun) bool {
	tf := r.Bucket.String
	sub := r.Subreddit.String
	subInterval := int(r.SubInterval.Int32)
	waveTotal := s.cycleWaveCount(r.Layer, tf, sub, r.CycleID.String)
	if wait := time.Until(r.ScheduledAt); wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return true
		}
	} else {
		s.Events.Addf(LevelInfo, r.Layer, "reclaim r/%s wave %d/%s: overdue by %s — firing immediately (best-effort)",
			sub, subInterval, fmtWaveTotal(waveTotal), formatDur(-wait))
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
	// the same (tf, sub) opened a newer wave set. Return false so the caller
	// discards remaining waves in this cycle (they share the same cycle_id).
	if s.runStore != nil {
		ok, err := s.runStore.TryMarkRunning(r.ID)
		if err != nil {
			s.Events.Addf(LevelWarn, r.Layer, "reclaim r/%s wave %d: mark running: %v", sub, subInterval, err)
		} else if !ok {
			s.Events.Addf(LevelInfo, r.Layer, "reclaim r/%s wave %d/%s: superseded by newer cycle — skipped",
				sub, subInterval, fmtWaveTotal(waveTotal))
			if r.Layer == "L2" {
				s.maybeDropL2Cycle(tf, sub, cycleID, subInterval)
			}
			if r.Layer == "L3" {
				s.dropL3Cycle(tf, sub, cycleID)
			}
			return false
		}
	}
	s.Events.Addf(LevelInfo, r.Layer, "reclaim r/%s wave %d/%s firing (chunk=%d, cycle=%s)",
		sub, subInterval, fmtWaveTotal(waveTotal), chunk, cycleID)

	if r.Layer == "L3" {
		s.advanceL3Wave(tf, sub, cycleID, subInterval)
	}
	if r.Layer == "L2" {
		s.advanceL2Wave(tf, sub, cycleID, subInterval)
	}

	var runErr error
	switch r.Layer {
	case "L2":
		depth := s.resolveSubDepth(sub)
		if !depthHasL2(depth) {
			if s.runStore != nil {
				_ = s.runStore.MarkFinished(r.ID, "skipped", "depth no longer covers L2")
			}
			s.maybeDropL2Cycle(tf, sub, cycleID, subInterval)
			return true
		}
		runErr = s.runL2Wave(ctx, tf, sub, chunk, cycleID, subInterval)
	case "L3":
		depth := s.resolveSubDepth(sub)
		if !depthHasL3(depth) {
			if s.runStore != nil {
				_ = s.runStore.MarkFinished(r.ID, "skipped", "depth no longer covers L3")
			}
			return true
		}
		runErr = s.runL3Wave(ctx, tf, sub, chunk, cycleID, subInterval)
	}
	if s.runStore != nil {
		if runErr != nil {
			_ = s.runStore.MarkFinished(r.ID, "fail", runErr.Error())
		} else {
			_ = s.runStore.MarkFinished(r.ID, "ok", "")
		}
	}
	if r.Layer == "L2" {
		s.maybeDropL2Cycle(tf, sub, cycleID, subInterval)
	}
	// L3's snapshot is dropped by driveReclaimedCycle on natural completion (its
	// wave count is variable, so there is no fixed last-wave index here).
	return true
}

// maybeDropL2Cycle clears the in-memory snapshot once the last wave has
// fired — outside the original runL2Cycle goroutine we have to do this by
// hand instead of via its defer.
func (s *Scheduler) maybeDropL2Cycle(tf, sub, cycleID string, justFired int) {
	if justFired >= l2WavesPerCycle {
		s.dropL2Cycle(tf, sub, cycleID)
	}
}

// driverKey, driverEnter, driverExit and driverActive track how many wave-driving
// goroutines (live driveWaves or driveReclaimedCycle) are currently firing for a
// (layer, tf, sub). reconcileLoop reads driverActive to avoid launching a
// duplicate driver while a healthy cycle is still running.
func driverKey(layer, tf, sub string) string { return layer + "|" + tf + "|" + sub }

func (s *Scheduler) driverEnter(layer, tf, sub string) {
	s.driverMu.Lock()
	if s.drivers == nil {
		s.drivers = make(map[string]int)
	}
	s.drivers[driverKey(layer, tf, sub)]++
	s.driverMu.Unlock()
}

func (s *Scheduler) driverExit(layer, tf, sub string) {
	s.driverMu.Lock()
	k := driverKey(layer, tf, sub)
	if s.drivers[k] > 1 {
		s.drivers[k]--
	} else {
		delete(s.drivers, k)
	}
	s.driverMu.Unlock()
}

func (s *Scheduler) driverActive(layer, tf, sub string) bool {
	s.driverMu.Lock()
	defer s.driverMu.Unlock()
	return s.drivers[driverKey(layer, tf, sub)] > 0
}

// reconcileInterval is how often reconcileLoop polls for re-enable transitions.
// Short enough that flipping a layer back on feels responsive, long enough that
// the ListWavesForActiveCycles scan is negligible.
const reconcileInterval = 15 * time.Second

// reconcileLoop is the live re-enable supervisor. The coordinator already
// rebuilds bucket loops when settings change, and the per-wave depth re-check
// (driveWaves / driveReclaimedCycle) pauses a layer the instant it is switched
// off — but a layer switched back *on* mid-period would otherwise sit idle until
// the next L1 fetch. This loop closes that gap: on a disabled→enabled transition
// for a (layer, sub) it resumes the paused wave plan if rows are still pending,
// or generates a fresh plan from scratch (dispersed across the time left until
// the next L1 cycle) and drives it. In steady state — no transition — it does
// nothing, leaving normal L1-triggered cycling untouched.
func (s *Scheduler) reconcileLoop(ctx context.Context) {
	if s.runStore == nil {
		return
	}
	prev := map[string]bool{}
	seeded := false
	for {
		if err := sleep(ctx, reconcileInterval); err != nil {
			return
		}
		if !s.isEnabled() {
			// Forget transitions while the whole instance is off, so re-enabling
			// it later doesn't read as a per-sub transition storm.
			prev = map[string]bool{}
			seeded = false
			continue
		}
		cur := map[string]bool{}
		for tf, subs := range s.groupSubsByBucket(s.activeSubs()) {
			for _, sub := range subs {
				depth := s.resolveSubDepth(sub)
				for _, layer := range []string{"L2", "L3"} {
					if !depthCoversLayer(layer, depth) {
						continue
					}
					key := driverKey(layer, tf, sub)
					cur[key] = true
					// Only act on a genuine disabled→enabled transition; the first
					// pass merely seeds `prev` so steady state never regenerates.
					if seeded && !prev[key] {
						s.resumeOrRegenerate(ctx, layer, tf, sub)
					}
				}
			}
		}
		prev = cur
		seeded = true
	}
}

// resumeOrRegenerate is invoked when a layer flips back on for a sub. It resumes
// the surviving paused plan if any waves are still pending, otherwise generates a
// fresh plan and drives it ("recovery on top of a freshly generated plan"). It
// is a no-op when a driver is already firing — i.e. the disable was too brief
// for the cycle to ever pause.
func (s *Scheduler) resumeOrRegenerate(ctx context.Context, layer, tf, sub string) {
	if s.driverActive(layer, tf, sub) {
		return
	}
	if pending := s.pendingWavesFor(layer, tf, sub); len(pending) > 0 {
		s.Events.Addf(LevelInfo, layer, "reconcile r/%s: %s re-enabled — resuming %d pending wave(s)",
			sub, layer, len(pending))
		go s.driveReclaimedCycle(ctx, layer, tf, sub, pending)
		return
	}
	// A sub freshly added to the crawl list whose first L1 fetch hasn't landed
	// any posts yet has nothing for L2/L3 to chew on: ListNeedingMedia /
	// ListL3Candidates return zero, so a regenerated cycle fires only empty
	// "0 eligible posts — skipped" waves and litters /debug + the ledger with a
	// phantom plan (the 10-wave post_count=50 placeholder seen when golang was
	// first added). Defer to the first L1 fetch's own fan-out (runL2Cycle), which
	// spawns a correctly-sized L2/L3 cycle once posts actually exist. Only skip
	// when we can positively confirm zero posts; an unknown count (no store)
	// keeps the prior regenerate behaviour.
	if n, known := s.subArchivedPostCount(sub); known && n == 0 {
		s.Events.Addf(LevelInfo, layer, "reconcile r/%s: %s re-enabled but no posts archived yet — deferring to first L1 fetch", sub, layer)
		return
	}

	// No surviving plan — roll a fresh one across the time remaining before the
	// next L1 cycle so the whole batch still lands before L1 comes round again.
	// Size it like one L1 round (reconcilePostCount), NOT the full archive
	// backlog — sizing off the candidate pool is what produced the runaway
	// 184-wave cycle. The actual posts are still chosen per wave by the
	// candidate query; postCount only sets the wave plan's shape.
	period := s.timeUntilNextCycle(tf)
	postCount := s.reconcilePostCount(tf, sub)
	switch layer {
	case "L2":
		go func() {
			cycleID := fmt.Sprintf("L2:%s:%s:%d", tf, sub, time.Now().Unix())
			s.Events.Addf(LevelInfo, "L2", "reconcile r/%s: L2 re-enabled with no pending plan — generating a fresh cycle (post_count=%d)", sub, postCount)
			s.driveL2Cycle(ctx, tf, sub, postCount, period, cycleID, s.resolveSubDepth(sub))
		}()
	case "L3":
		go func() {
			s.Events.Addf(LevelInfo, "L3", "reconcile r/%s: L3 re-enabled with no pending plan — generating a fresh cycle (post_count=%d)", sub, postCount)
			s.runL3Cycle(ctx, tf, sub, postCount, period)
		}()
	}
}

// pendingWavesFor returns this (layer, tf, sub)'s still-pending wave rows, sorted
// by wave index — the surviving plan reconcileLoop resumes after a re-enable.
func (s *Scheduler) pendingWavesFor(layer, tf, sub string) []store.PrefetchRun {
	if s.runStore == nil {
		return nil
	}
	rows, err := s.runStore.ListWavesForActiveCycles()
	if err != nil {
		return nil
	}
	var out []store.PrefetchRun
	for _, r := range rows {
		if r.Layer == layer && r.Status == "pending" &&
			r.Bucket.String == tf && r.Subreddit.String == sub {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SubInterval.Int32 < out[j].SubInterval.Int32
	})
	return out
}

// timeUntilNextCycle reports how long until the sub's bucket fires its next L1
// fetch, used as the dispersal window for a reconcile-generated plan so it still
// completes before the next L1 round. Floors at minCyclePeriod when the next
// cycle is unknown or already due.
func (s *Scheduler) timeUntilNextCycle(tf string) time.Duration {
	if st := s.loadBucketState(tf); st != nil && !st.NextCycleAt.IsZero() {
		if d := time.Until(st.NextCycleAt); d > minCyclePeriod {
			return d
		}
	}
	return minCyclePeriod
}

// subArchivedPostCount returns how many posts are archived for sub and whether
// that count is known. It is "unknown" (known=false) when there is no postStore
// and no postCountFn seam, or the query errors — callers must treat unknown as
// "don't gate" so a transient DB hiccup never suppresses a legitimate cycle. The
// postCountFn seam lets tests drive the brand-new-sub gate without a live DB.
func (s *Scheduler) subArchivedPostCount(sub string) (count int, known bool) {
	if s.postCountFn != nil {
		n, err := s.postCountFn(sub)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	if s.postStore != nil {
		n, err := s.postStore.CountBySubreddit(sub, false)
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// reconcilePostCount sizes a reconcile-generated L2/L3 cycle the SAME way a real
// L1 round does: by the number of posts the last listing fetch surfaced for this
// (tf, sub) — NOT by the full eligible backlog. Sizing off ListL3Candidates /
// ListNeedingMedia (the whole archive) would plan an absurd cycle — e.g. 184
// waves' worth of comments in one period when a round only ever surfaces ~one
// listing page. Falls back to the configured listing page size (page_limit),
// then to a sane default, when no prior cycle exists yet.
func (s *Scheduler) reconcilePostCount(tf, sub string) int {
	if s.runStore != nil {
		if n, err := s.runStore.LastCyclePostCount(tf, sub); err == nil && n > 0 {
			return n
		}
	}
	if s.settings != nil {
		if n := parsePositiveInt(s.settings.Get("page_limit")); n > 0 {
			return n
		}
	}
	return defaultReconcilePostCount
}

// defaultReconcilePostCount is the last-resort L1-round size for a regenerated
// cycle when neither a prior cycle nor a page_limit setting is available. Mirrors
// the default "posts per upstream page" so a fresh re-enable behaves like one
// ordinary listing round.
const defaultReconcilePostCount = 50

// parsePositiveInt parses a trimmed positive integer, returning 0 on any miss.
func parsePositiveInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0
	}
	return n
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
			subs := ""
			if s.settings != nil {
				subs = s.settings.Get("prefetch_subs")
			}
			s.setL1Status("disabled", 0, 0, nil, nil, time.Time{})
			s.Events.Addf(LevelSkip, "L1", "disabled (no crawl list, prefetch_subs=%q), sleeping 30s", subs)
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
			// Loops were just launched; each one is waiting on its own schedule
			// and will publish its real phase ("idle" until its next fetch,
			// "running" while fetching). Don't claim "running" globally here —
			// that is the phantom that made L1 look busy while every bucket was
			// actually sleeping until a far-future NextCycleAt.
			s.setL1Status("idle", 0, 0, subs, nil, time.Time{})
			lastSig = sig
			lastSubs = subs
		}
		_ = lastSubs // retained for future reload diagnostics

		if err := sleep(ctx, 30*time.Second); err != nil {
			return
		}
	}
}

// isEnabled reports whether NP should run. There is no separate on/off toggle:
// the crawl list IS the switch. A non-empty prefetch_subs (from the settings UI
// or REDMEMO_DEFAULT_PREFETCH_SUBS) enables the layer; a blank one — including
// input that was pure punctuation/whitespace and got filtered down to nothing —
// disables it, since there is nothing to crawl.
func (s *Scheduler) isEnabled() bool {
	if s.settings == nil {
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

// pruneCursors removes cursor entries whose subreddit is not in the active crawl
// set `subs`. Cursor keys are "<sub>|<sort>[|<tf>]" (see cursorKey), so the sub
// is the segment before the first '|'. Used on bucket-state load so a sub
// dropped from the crawl list does not leave a phantom cursor lingering in the
// persisted map and showing up on /debug. Mutates the map in place.
func pruneCursors(cursors map[string]string, subs []string) {
	if len(cursors) == 0 {
		return
	}
	keep := make(map[string]bool, len(subs))
	for _, sub := range subs {
		keep[strings.ToLower(strings.TrimSpace(sub))] = true
	}
	for k := range cursors {
		sub := k
		if i := strings.IndexByte(k, '|'); i >= 0 {
			sub = k[:i]
		}
		if !keep[strings.ToLower(sub)] {
			delete(cursors, k)
		}
	}
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
	// Drop cursors for subs no longer in this bucket's crawl list. bucketState
	// persists across settings changes, so a sub removed from prefetch_subs
	// (e.g. an old r/gfur) would otherwise linger in the map and surface as a
	// phantom L1 cursor on /debug forever. Pruning here cleans both the live
	// display and — on the cycle's next saveBucketState — the persisted state.
	pruneCursors(cursors, subs)

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
		// immediately; the user is owed a fetch. While waiting the bucket is
		// idle, not running: reflect that so /debug does not show a stale
		// "running" for the whole inter-cycle (and post-restart) sleep — the
		// same phantom-status class as the L2 recovering fix. "running" is set
		// only once the cycle actually starts fetching (below).
		if w := time.Until(nextFetchAt); w > 0 {
			s.setL1Status(fmt.Sprintf("bucket=%s idle", tf), 0, 0, subs, cursors, nextFetchAt)
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

			// L2 media work is bound to the most recent L1 cycle for this
			// (tf, sub): every L1 fetch — success, fail, empty, or depth=none —
			// supersedes any prior pending L2 wave here, BEFORE runL2Cycle's own
			// short-circuits (postCount=0, depth=none) might return without ever
			// calling supersede themselves. cycleID is always non-empty
			// (runOneSubFetch stamps it before the fetch), so prior-cycle rows
			// always lose the cycle_id comparison.
			//
			// L3 is NOT superseded here. It is a self-standing layer with its
			// own cycle lineage (L3:<tf>:<sub>:<unix>) decoupled from L1/L2 in
			// the ledger; runL3Cycle supersedes its own prior pending waves
			// against its freshly-minted L3 cycle id.
			if s.runStore != nil {
				if n, err := s.runStore.SupersedePending("L2", tf, sub, cycleID, "superseded by newer L1 cycle"); err == nil && n > 0 {
					s.Events.Addf(LevelInfo, "L2", "r/%s: discarded %d stale wave(s) from previous cycle", sub, n)
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
			// ArchiveListing (not ArchivePosts): record each post's index in the
			// upstream hot listing so L3 fetches comments top-to-bottom in the
			// order a homepage visitor sees them.
			s.archiver.ArchiveListing(posts, sub, "natural_prefetch")
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
	urlPath string
	postID  string
	items   []mediaItem
	okSoFar bool // whether the post's non-frozen items all downloaded cleanly
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

// l3WaveTarget is the desired *average* number of L3 comment fetches per wave.
// Unlike L2 (CDN downloads, effectively unmetered), every L3 fetch spends one
// real OAuth API request, so each wave should stay small. The number of L3
// waves in a cycle is derived from postCount/l3WaveTarget (not a fixed 5 like
// L2), so the whole round's candidates are drained before the next L1 fetch —
// "before the next L1 round, L3 must finish this round's batch." The per-wave
// chunk still fluctuates around this target (splitNonUniform's non-uniform
// partition) rather than landing on a flat l3WaveTarget every wave.
const l3WaveTarget = 5

// l3WaveCap is the hard per-wave ceiling — a safety burst limit so a pathologic
// non-uniform draw can't make one wave fire far more than the target. With the
// wave *count* scaled to postCount the average stays at l3WaveTarget, so this
// cap almost never bites; it exists purely to bound the worst case.
const l3WaveCap = 10

// l3MaxWaves bounds the per-cycle L3 wave count so an unexpectedly huge L1 round
// can't schedule hundreds of waves into one period. Beyond this the leftover
// candidates roll into the next L3 cycle (L3 re-walks recent posts each cycle),
// matching the pre-existing "fetch later" overflow semantics. A normal L1
// listing surfaces ≤~100 posts, so l3MaxWaves is reached only in extreme cases.
const l3MaxWaves = 64

// planWaveOffsets rolls `waves` non-uniform time offsets across `period`, with a
// guaranteed ≥waveMinGapFrac-of-period gap between consecutive waves (to avoid
// bunching) plus a non-uniform random portion on top. Shared by L2 and L3 so
// both layers disperse their requests with the same stealth tempo. The trailing
// i*minGap shift lets the last offset reach up to the full period — a deliberate
// slight overrun of the random span that keeps the gaps honest.
func planWaveOffsets(period time.Duration, waves int) []time.Duration {
	minGap := time.Duration(float64(period) * waveMinGapFrac)
	if minGap < 0 {
		minGap = 0
	}
	reserved := time.Duration(waves-1) * minGap
	randomSpan := period - reserved
	if randomSpan < 0 {
		randomSpan = 0
	}
	offsets := make([]time.Duration, waves)
	for i := range offsets {
		offsets[i] = time.Duration(rand.Float64() * float64(randomSpan))
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	// Shift each sorted offset by i*minGap so consecutive waves are always
	// ≥minGap apart, while keeping their relative spacing non-uniform.
	for i := range offsets {
		offsets[i] += time.Duration(i) * minGap
	}
	return offsets
}

// planWaves rolls the per-cycle L2 stealth plan: l2WavesPerCycle time offsets
// across `period` and per-wave chunk sizes drawn from a non-uniform partition of
// postCount (capped at l2WaveCap). Both the firing tempo *and* the per-wave
// request volume vary every cycle so an observer cannot pin either to a fixed
// quintile.
func planWaves(postCount int, period time.Duration) (chunks []int, offsets []time.Duration) {
	offsets = planWaveOffsets(period, l2WavesPerCycle)
	chunks = splitNonUniform(postCount, l2WavesPerCycle, l2WaveCap)
	return chunks, offsets
}

// planL3Waves rolls the per-cycle L3 stealth plan. Unlike L2 (a fixed
// l2WavesPerCycle waves with overflow dropped), L3 sizes the *wave count* to the
// work — ceil(postCount/l3WaveTarget), clamped to [1, l3MaxWaves] — so every
// candidate this round is scheduled and the whole batch lands before the next
// L1 fetch. The per-wave chunk still fluctuates (non-uniform partition) around
// l3WaveTarget rather than being a flat target, and offsets are dispersed
// non-uniformly across the period by planL3WaveOffsets, whose inter-wave floor
// scales with the wave count so a large count never overruns into the next L1
// round. Only when postCount exceeds l3MaxWaves*l3WaveCap does the tail roll
// into the next cycle (the pre-existing "fetch later" overflow).
func planL3Waves(postCount int, period time.Duration) (chunks []int, offsets []time.Duration) {
	if postCount <= 0 {
		return nil, nil
	}
	waves := (postCount + l3WaveTarget - 1) / l3WaveTarget
	if waves < 1 {
		waves = 1
	}
	if waves > l3MaxWaves {
		waves = l3MaxWaves
	}
	offsets = planL3WaveOffsets(period, waves)
	chunks = splitL3(postCount, waves, l3WaveCap)
	return chunks, offsets
}

// l3CycleChunksInvalid encodes the validity invariant of an L3 plan produced by
// planL3Waves, shared by the generation path and the startup hangover cleanup so
// the two can never drift. A valid plan (1) fully covers the L1 post_count — the
// sum of per-wave chunks equals postCount, the whole point of the new
// many-waves-around-l3WaveTarget dispersal — and (2) never schedules a wave
// larger than l3WaveCap. It returns a short human reason when the plan is
// invalid. postCount==0 (no work) is always valid. The full-coverage check is
// skipped above l3MaxWaves*l3WaveCap, the one regime where planL3Waves *does*
// legitimately leave overflow for the next cycle.
func l3CycleChunksInvalid(chunks []int, postCount int) (string, bool) {
	if postCount <= 0 {
		return "", false
	}
	sum, maxChunk := 0, 0
	for _, c := range chunks {
		sum += c
		if c > maxChunk {
			maxChunk = c
		}
	}
	if maxChunk > l3WaveCap {
		return fmt.Sprintf("wave chunk %d > cap %d", maxChunk, l3WaveCap), true
	}
	if postCount <= l3MaxWaves*l3WaveCap && sum < postCount {
		return fmt.Sprintf("covers only %d of post_count %d", sum, postCount), true
	}
	return "", false
}

// l3PlanHangover reconstructs an L3 cycle's plan (per-wave chunks + its own
// post_count) straight from its persisted prefetch_runs rows and decides whether
// it should be discarded rather than recovered. It is the DB-facing "hangover"
// detector the reclaim path uses to drop stale/old-planner/mis-sized L3 cycles.
// Three ways a cycle is a hangover:
//   - it under-covers its post_count, or has a wave > l3WaveCap (l3CycleChunksInvalid);
//   - its post_count overshoots the L1 round size (l1Count) it should have been
//     sized to — the runaway-184 bug, where the cycle was sized off the whole
//     candidate backlog instead of one L1 listing round. l1Count<=0 (unknown)
//     skips this check.
//
// Chunks/post_count are read across *all* rows of the cycle (fired or pending)
// since they describe the whole cycle, not just what is left.
func l3PlanHangover(runs []store.PrefetchRun, l1Count int) (string, bool) {
	postCount := 0
	chunks := make([]int, 0, len(runs))
	for _, r := range runs {
		var meta struct {
			Chunk     int `json:"chunk"`
			PostCount int `json:"post_count"`
		}
		_ = json.Unmarshal(r.Payload, &meta)
		if meta.PostCount > postCount {
			postCount = meta.PostCount
		}
		chunks = append(chunks, meta.Chunk)
	}
	if reason, bad := l3CycleChunksInvalid(chunks, postCount); bad {
		return reason, true
	}
	if l1Count > 0 && postCount > l1Count {
		return fmt.Sprintf("post_count %d overshoots L1 round %d", postCount, l1Count), true
	}
	return "", false
}

// splitL3 partitions postCount across `waves` bins that fluctuate around the
// average (postCount/waves ≈ l3WaveTarget) yet sum to *exactly* postCount, with
// every bin clamped to [0, cap]. splitNonUniform (L2's partitioner) only fully
// covers postCount when the residual fits one bin — fine for L2's fixed 5 waves
// but it silently drops units across L3's many bins. Here an exact even split is
// the floor and a series of sum-preserving, bound-respecting pairwise transfers
// adds the non-uniform jitter, so no candidate is ever dropped within the
// unsaturated range. When postCount > waves*cap the even split saturates at cap
// and the surplus is intentionally left for the next cycle ("fetch later").
func splitL3(postCount, waves, cap int) []int {
	out := make([]int, waves)
	if waves <= 0 || postCount <= 0 {
		return out
	}
	if cap < 1 {
		cap = 1
	}
	base := postCount / waves
	rem := postCount % waves
	if base > cap {
		// Saturated: every bin pinned at cap, the rest rolls to the next cycle.
		base, rem = cap, 0
	}
	for i := range out {
		out[i] = base
	}
	// Hand out the remainder one unit at a time to random bins (capped), so the
	// "+1" slots aren't always the same indices.
	for k, i := range rand.Perm(waves) {
		if k >= rem {
			break
		}
		if out[i] < cap {
			out[i]++
		}
	}
	// Jitter: sum-preserving 1-unit transfers between in-bounds bins.
	for t := 0; t < waves*2; t++ {
		a, b := rand.Intn(waves), rand.Intn(waves)
		if a != b && out[a] > 0 && out[b] < cap {
			out[a]--
			out[b]++
		}
	}
	return out
}

// planL3WaveOffsets disperses `waves` non-uniform offsets across `period`,
// scaling the inter-wave floor to the wave count so however many waves there
// are they all land before the period ends — i.e. before the next L1 round.
// planWaveOffsets reserves a fixed waveMinGapFrac of the period per gap, which
// only works for L2's fixed 5 waves; with a variable L3 count that scheme would
// either bunch (many waves) or overrun the period (the reserved sum exceeding
// it). Here the floor is half of an even per-wave slice of the usable span, and
// a small tail (last 5% of the period) is left free so the final wave completes
// comfortably before the next L1 cycle starts.
func planL3WaveOffsets(period time.Duration, waves int) []time.Duration {
	if waves <= 0 {
		return nil
	}
	offsets := make([]time.Duration, waves)
	usable := time.Duration(float64(period) * 0.95)
	if usable <= 0 {
		return offsets
	}
	slice := usable / time.Duration(waves)
	minGap := slice / 2
	reserved := time.Duration(waves-1) * minGap
	randomSpan := usable - reserved
	if randomSpan < 0 {
		randomSpan = 0
	}
	for i := range offsets {
		offsets[i] = time.Duration(rand.Float64() * float64(randomSpan))
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	for i := range offsets {
		offsets[i] += time.Duration(i) * minGap
	}
	return offsets
}

// splitNonUniform partitions postCount across `waves` bins with a non-uniform
// random distribution, clamping each bin at `cap`. iid uniform weights (floored
// at 0.1 so a near-zero draw can't crush its wave) are normalized; each wave
// gets at least 1 post when postCount ≥ waves, the rest is divided
// proportionally, and the result is shuffled so the residual slot isn't always
// the same wave index. With postCount ≤ waves, postCount waves get 1 each (still
// shuffled) and the remainder gets 0. When postCount exceeds waves*cap the bins
// saturate at cap and their sum is intentionally below postCount — the caller
// (L3) treats the overflow as "fetch later", not "fetch now".
func splitNonUniform(postCount, waves, maxPerWave int) []int {
	out := make([]int, waves)
	if waves <= 0 || postCount <= 0 {
		return out
	}
	if maxPerWave < 1 {
		maxPerWave = 1
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
		if out[i] > maxPerWave {
			out[i] = maxPerWave
		}
		assigned += out[i]
	}
	last := postCount - assigned
	if last < 1 {
		last = 1
	}
	if last > maxPerWave {
		last = maxPerWave
	}
	out[waves-1] = last
	rand.Shuffle(waves, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// waveRunner is the per-wave fetch primitive both L2 and L3 implement.
type waveRunner func(ctx context.Context, tf, sub string, chunk int, cycleID string, subInterval int) error

// driveOutcome reports why driveWaves stopped, so the caller can decide whether
// to clear the live cycle snapshot. Only driveDisabled keeps it: an operator who
// just turned the layer off may turn it back on, and the persisted pending wave
// rows + retained snapshot let reconcileLayers resume the same plan.
type driveOutcome int

const (
	driveDone       driveOutcome = iota // all waves fired
	driveCtx                            // ctx cancelled (shutdown / coordinator teardown)
	driveDisabled                       // depth no longer covers this layer (paused, plan kept)
	driveSuperseded                     // a newer cycle demoted this one's wave rows
)

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
) driveOutcome {
	s.driverEnter(layer, tf, sub)
	defer s.driverExit(layer, tf, sub)
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
				return driveCtx
			}
		}
		if err := ctx.Err(); err != nil {
			return driveCtx
		}
		// Aggressive live disable: re-check depth right before firing each wave so
		// switching the layer off mid-cycle stops further requests at once (rather
		// than waiting out the cycle). The remaining wave rows stay 'pending' and
		// the live snapshot is kept (caller skips the drop) so re-enabling resumes
		// this exact plan via reconcileLayers.
		if !depthCoversLayer(layer, s.resolveSubDepth(sub)) {
			s.Events.Addf(LevelInfo, layer, "r/%s: %s disabled mid-cycle — paused at wave %d/%d, %d wave(s) left pending",
				sub, layer, i+1, len(offsets), len(offsets)-i)
			return driveDisabled
		}
		if runIDs[i] != 0 && s.runStore != nil {
			ok, _ := s.runStore.TryMarkRunning(runIDs[i])
			if !ok {
				s.Events.Addf(LevelInfo, layer, "r/%s wave %d/%d: superseded by newer cycle — skipped",
					sub, i+1, len(offsets))
				return driveSuperseded
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
			return driveDone
		}
	}
	return driveDone
}

// runL2Cycle is the per-L1-fetch fan-out: it kicks off this sub's two
// downstream layers and lets each run on its own terms. L3 (comments) and L2
// (media) are fully independent here — neither blocks or gates the other, and
// they no longer share a cycle key in the ledger.
//
//   - L3 is the precious layer for a forum archive: comments cost an OAuth
//     request and are what readers actually come back for. Whenever the sub's
//     depth covers comments, an independent L3 cycle is spawned with its own
//     cycle id; it walks recent posts via ListL3Candidates and is blind to
//     whether L2 has downloaded any media.
//   - L2 is pure CDN cache acceleration. The CDN is effectively unmetered, so
//     media carries no OAuth budget and exists only to pre-warm local storage.
//
// postCount == 0 still emits a single skipped L2 record (when the sub runs L2)
// so the unified ledger reflects "L1 found nothing" rather than going silent.
func (s *Scheduler) runL2Cycle(ctx context.Context, tf, sub string, postCount int, period time.Duration, cycleID string) {
	depth := s.resolveSubDepth(sub)

	// L3 stands on its own: spawn a full standalone comment cycle in parallel
	// with L2, with its own ledger lineage. Guarded only by postStore (the
	// candidate query needs it) — a media-less deployment still archives
	// comments.
	if depthHasL3(depth) {
		go s.runL3Cycle(ctx, tf, sub, postCount, period)
	} else {
		// L3 is off for this sub. L2's leftover pending waves are swept by the
		// unconditional L1→L2 SupersedePending in bucketLoop, but nothing retires
		// L3's once runL3Cycle stops being spawned — they would linger 'pending'
		// in the ledger and the /debug L3 cycle snapshot would persist as a
		// phantom ("L3 off" yet "wave 5/16") until the next container restart.
		// Sweep them here so a sub flipped away from L3 self-heals on the next L1
		// fetch, mirroring L2.
		s.retireStandaloneL3(tf, sub)
	}
	s.driveL2Cycle(ctx, tf, sub, postCount, period, cycleID, depth)
}

// retireStandaloneL3 discards any leftover pending L3 wave rows and the live
// /debug cycle snapshot for (tf, sub) once L3 stops running for the sub. Called
// from the L1 fan-out (runL2Cycle) on every cycle where the sub's depth does not
// cover L3, so an L3 plan paused by a mid-cycle disable does not outlive the
// disable. keepCycleID="" demotes every pending L3 wave row for the sub — none
// is kept, since no L3 cycle should be running. Safe to call when nothing is
// pending (the UPDATE simply affects zero rows) and when L3 was never enabled.
func (s *Scheduler) retireStandaloneL3(tf, sub string) {
	if s.runStore != nil {
		if n, err := s.runStore.SupersedePending("L3", tf, sub, "", "L3 disabled — retiring orphaned waves"); err == nil && n > 0 {
			s.Events.Addf(LevelInfo, "L3", "r/%s: L3 off — retired %d orphaned pending wave(s)", sub, n)
		}
	}
	s.dropL3CycleAny(tf, sub)
}

// driveL2Cycle is the L2-only half of runL2Cycle: plan + drive this sub's media
// waves for the given depth. Split out so reconcileLoop can regenerate just the
// L2 layer on re-enable without re-spawning an L3 cycle (runL2Cycle's L3 fan-out
// is L1-only).
func (s *Scheduler) driveL2Cycle(ctx context.Context, tf, sub string, postCount int, period time.Duration, cycleID, depth string) {
	if !depthHasL2(depth) {
		// depth=none → record the skip so /debug shows L1 fired but nothing
		// downstream ran. depth=l3 (comments only) emits no L2 row.
		if s.runStore != nil && depth == "none" {
			payload, _ := json.Marshal(map[string]any{"post_count": postCount, "reason": "depth=none"})
			_ = s.runStore.Record("L2", tf, sub, "", cycleID, "skipped", "", payload)
		}
		return
	}
	if s.postStore == nil || s.media == nil {
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
	outcome := s.driveWaves(ctx, "L2", tf, sub, cycleID, chunks, offsets, cycleStart, period, postCount, nil,
		func(wave int) { s.advanceL2Wave(tf, sub, cycleID, wave) },
		s.runL2Wave)
	// Keep the snapshot + pending rows on a disable-pause so flipping L2 back on
	// resumes this plan; drop it on every other terminal outcome.
	if outcome != driveDisabled {
		s.dropL2Cycle(tf, sub, cycleID)
	}
}

// listNeedingMedia returns the next batch of posts whose media is not yet
// archived. The pendingMediaFn seam lets tests drive runL2Wave's bind/media
// branching without a live DB; production uses the PostStore query.
func (s *Scheduler) listNeedingMedia(sub string, limit int) ([]*store.StoredPost, error) {
	if s.pendingMediaFn != nil {
		return s.pendingMediaFn(sub, limit)
	}
	return s.postStore.ListNeedingMedia(sub, limit)
}

// markMediaDone flags a post's media as fully archived. Nil-guarded so a
// test driving runL2Wave purely through pendingMediaFn (postStore == nil)
// does not panic on the bookkeeping write.
func (s *Scheduler) markMediaDone(urlPath string) {
	if s.postStore != nil {
		s.postStore.SetMediaDone(urlPath)
	}
}

// commentMediaURLs returns the raw CDN URLs of inline images embedded in the
// post's archived comment bodies, via the archiver (or the commentMediaFn test
// seam). Empty when no archiver is wired or no comments carry an image.
func (s *Scheduler) commentMediaURLs(urlPath string) []string {
	if s.commentMediaFn != nil {
		return s.commentMediaFn(urlPath)
	}
	if s.archiver == nil || urlPath == "" {
		return nil
	}
	return s.archiver.CommentMediaURLs(urlPath)
}

// appendCommentMedia adds any inline preview/i.redd.it images embedded in the
// post's archived comment bodies to items, deduped (by CanonicalKey) against the
// media already queued for the post. Comment images are signed and expire like
// selftext body images, so L2 caches them here rather than letting them 403
// after the user finally opens the thread.
func (s *Scheduler) appendCommentMedia(items []mediaItem, urlPath string) []mediaItem {
	urls := s.commentMediaURLs(urlPath)
	if len(urls) == 0 {
		return items
	}
	seen := make(map[string]bool, len(items)+len(urls))
	for _, it := range items {
		seen[reddit.CanonicalKey(it.URL)] = true
	}
	for _, raw := range urls {
		if key := reddit.CanonicalKey(raw); !seen[key] {
			seen[key] = true
			items = append(items, mediaItem{URL: raw, Kind: "image"})
		}
	}
	return items
}

// requeueForCommentMedia re-arms a post's media queue when its freshly fetched
// comments carry inline images not yet on disk, so the next L2 wave harvests and
// downloads them. L3 only signals here — the download itself is L2's job. No-op
// when every comment image is already cached, so a periodic L3 re-fetch of an
// unchanged thread does not churn the L2 queue.
func (s *Scheduler) requeueForCommentMedia(urlPath string, comments []reddit.Comment) {
	if s.postStore == nil || urlPath == "" {
		return
	}
	for _, raw := range reddit.ExtractCommentImageURLs(comments) {
		if s.media == nil || !s.media.IsCached(raw) {
			if err := s.postStore.ClearMediaDone(urlPath); err != nil {
				s.Events.Addf(LevelWarn, "L3", "%s: re-arm media for comment image failed: %v", urlPath, err)
			}
			return
		}
	}
}

// listL3Candidates returns the next batch of L3-eligible posts for sub under
// the cycle-freeze + min-comments rules. The l3CandidatesFn seam lets tests
// drive runL3Wave without a live DB; production uses the PostStore query.
func (s *Scheduler) listL3Candidates(sub, cycleID, prevCycleID string, limit, minComments int) ([]*store.StoredPost, error) {
	if s.l3CandidatesFn != nil {
		return s.l3CandidatesFn(sub, cycleID, prevCycleID, limit, minComments)
	}
	if s.postStore == nil {
		return nil, nil
	}
	return s.postStore.ListL3Candidates(sub, cycleID, prevCycleID, limit, minComments)
}

// runL2Wave drains up to `limit` pending-media posts for sub through the NP
// dispatcher, downloading every media URL each post needs. L2 is pure CDN
// cache: it touches media only and never fetches comments — L3 is a separate,
// self-standing layer driven by runL3Cycle/runL3Wave. Returns the first ctx
// error if any.
func (s *Scheduler) runL2Wave(ctx context.Context, tf, sub string, limit int, cycleID string, subInterval int) error {
	if s.media == nil || (s.postStore == nil && s.pendingMediaFn == nil) {
		return nil
	}
	depth := s.resolveSubDepth(sub)
	if !depthHasL2(depth) {
		// Reachable only on a mid-cycle settings flip away from L2; comments-only
		// and "none" are routed away in runL2Cycle.
		s.setL2Status("idle", "", 0)
		return nil
	}

	pending, err := s.listNeedingMedia(sub, limit)
	if err != nil {
		s.Events.Addf(LevelError, "L2", "r/%s: query pending media: %v", sub, err)
		return nil
	}

	if len(pending) == 0 {
		s.setL2Status("idle", sub, 0)
		return nil
	}

	s.setL2Status("downloading", sub, len(pending))
	s.Events.Addf(LevelInfo, "L2", "r/%s wave %d/%d: %d posts need media -- submitting to NP queue",
		sub, subInterval, l2WavesPerCycle, len(pending))

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
		// Images pasted into the post's archived comment bodies (signed,
		// short-lived preview.redd.it/i.redd.it links) live only in the comment
		// tree, not in the post JSON ExtractMediaItems sees. Harvest them here so
		// L2 — the NP media layer — caches the bytes; an L3 fetch only signals
		// that they exist (it re-arms media_done), it never downloads.
		mediaItems = s.appendCommentMedia(mediaItems, sp.URLPath)
		if len(mediaItems) == 0 {
			// Text/link posts carry no media, so the L2 download path is a
			// no-op and the post is marked done immediately. Comments (if the
			// sub runs L3) are handled by the independent L3 cycle, not here.
			s.markMediaDone(sp.URLPath)
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
			s.markMediaDone(sp.URLPath)
			completed++
			s.Events.Addf(LevelOK, "L2", "r/%s post %s: %d media done (%s)",
				sub, sp.PostID, len(mediaItems), mediaKindSummary(mediaItems))
			s.recordL2Post(tf, sub, sp.PostID, cycleID, subInterval, len(mediaItems), "ok", "")
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
				s.markMediaDone(fp.urlPath)
				completed++
				s.recordL2Post(tf, sub, fp.postID, cycleID, subInterval, len(fp.items), "ok", "")
			}
		}
	}

	if completed > 0 {
		s.Events.Addf(LevelOK, "L2", "r/%s: media complete for %d/%d posts", sub, completed, len(pending))
	}

	s.setL2Status("idle", "", 0)
	return nil
}

// runL3Cycle is the self-standing comment layer, spawned once per L1 fetch for
// any sub whose depth covers comments (both depth="l3" and depth="l2+l3"). It
// is fully decoupled from L1 and L2 in the ledger: it mints its OWN cycle id
// (L3:<tf>:<sub>:<unix>), supersedes its own prior pending waves against it,
// and chooses work via ListL3Candidates rather than L2's media-done queue.
// Comments are the scarce, OAuth-budgeted resource, so L3 shares the dispatcher
// + budget gate with L1 directly and never waits on a CDN media download.
//
// L3 deliberately does not drain a queue: each wave re-walks the recent slice;
// ListL3Candidates skips posts already fetched this/last L3 cycle and re-admits
// any whose comment count grew since. Flip a sub to depth=l2 to turn it off.
func (s *Scheduler) runL3Cycle(ctx context.Context, tf, sub string, postCount int, period time.Duration) {
	if s.postStore == nil && s.l3CandidatesFn == nil {
		return
	}
	l3CycleID := fmt.Sprintf("L3:%s:%s:%d", tf, sub, time.Now().Unix())

	// Supersede this sub's own stale pending L3 waves — a previous L3 cycle
	// whose goroutine is still mid-flight or was revived after a restart.
	// Decoupled from L1: only L3 rows carrying a different L3 cycle id are
	// demoted, so an L1 (or L2) cycle boundary never disturbs L3's lineage.
	if s.runStore != nil {
		if n, err := s.runStore.SupersedePending("L3", tf, sub, l3CycleID, "superseded by newer L3 cycle"); err == nil && n > 0 {
			s.Events.Addf(LevelInfo, "L3", "r/%s: discarded %d stale wave(s) from previous L3 cycle", sub, n)
		}
	}

	if postCount <= 0 {
		if s.runStore != nil {
			payload, _ := json.Marshal(map[string]any{"post_count": 0})
			_ = s.runStore.Record("L3", tf, sub, "", l3CycleID, "skipped", "", payload)
		}
		return
	}
	chunks, offsets := planL3Waves(postCount, period)
	cycleStart := time.Now()
	s.recordL3CycleStart(tf, sub, postCount, chunks, cycleStart, period, offsets, l3CycleID)
	outcome := s.driveWaves(ctx, "L3", tf, sub, l3CycleID, chunks, offsets, cycleStart, period, postCount,
		map[string]any{"standalone": true},
		func(wave int) { s.advanceL3Wave(tf, sub, l3CycleID, wave) },
		s.runL3Wave)
	// Keep the snapshot + pending rows on a disable-pause so flipping L3 back on
	// resumes this plan; drop it on every other terminal outcome.
	if outcome != driveDisabled {
		s.dropL3Cycle(tf, sub, l3CycleID)
	}
}

// runL3Wave fetches comments for up to `limit` posts of sub via the same NP
// dispatcher + budget gate L1 uses — the two API-budget layers jointly pace
// their requests through the single dispatch queue.
//
// Dedup is cycle-id based on L3's OWN lineage, not L1's: ListL3Candidates
// excludes any post whose most recent successful L3 fetch landed in the current
// L3 cycle or the L3 cycle just before it (PreviousL3CycleID). The result is a
// fixed 1-cycle freeze measured in L3 cycles: a post archived during L3 cycle N
// stays out of N+1 and reappears at N+2. A post whose comment count grew since
// is re-admitted immediately via the candidate query's growth override. The
// min-comments waterline is applied both at the SQL layer and re-checked here.
func (s *Scheduler) runL3Wave(ctx context.Context, tf, sub string, limit int, cycleID string, subInterval int) error {
	if s.postStore == nil && s.l3CandidatesFn == nil {
		return nil
	}
	waveTotal := s.cycleWaveCount("L3", tf, sub, cycleID)
	prevCycle := ""
	if s.runStore != nil {
		var err error
		prevCycle, err = s.runStore.PreviousL3CycleID(sub, cycleID)
		if err != nil {
			s.Events.Addf(LevelWarn, "L3", "r/%s: prev L3 cycle lookup: %v", sub, err)
		}
	}
	minComments := s.resolveL3MinComments()
	pending, err := s.listL3Candidates(sub, cycleID, prevCycle, limit, minComments)
	if err != nil {
		s.Events.Addf(LevelError, "L3", "r/%s: query candidates: %v", sub, err)
		return nil
	}
	if len(pending) == 0 {
		s.Events.Addf(LevelInfo, "L3", "r/%s wave %d/%s: 0 eligible posts (freeze + min_comments=%d) — skipped",
			sub, subInterval, fmtWaveTotal(waveTotal), minComments)
		return nil
	}
	s.Events.Addf(LevelInfo, "L3", "r/%s wave %d/%s: %d posts -- fetching comments (prev_cycle=%q, min_comments=%d)",
		sub, subInterval, fmtWaveTotal(waveTotal), len(pending), prevCycle, minComments)
	for _, sp := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		nc := numCommentsOfJSON(sp.JSONData)
		// Defensive re-check of the SQL min-comments prefilter so the verdict
		// lives in one Go-testable place and a borderline row can't slip
		// through; a miss records a 'skipped' ledger row.
		if !s.l3MeetsThreshold(nc, sub, sp.PostID) {
			continue
		}
		s.fetchL3Standalone(ctx, tf, sub, sp.PostID, sp.URLPath, cycleID, nc)
	}
	return nil
}

// numCommentsOfJSON parses the upstream-reported comment count (Comments[1])
// straight out of a stored post's JSON, without a full reddit.Post decode. Used
// where the wave already holds the raw JSONData and only needs the count for
// the rumination baseline. Returns 0 on any decode/parse miss.
func numCommentsOfJSON(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	var p reddit.Post
	if err := json.Unmarshal(data, &p); err != nil {
		return 0
	}
	return numCommentsOf(&p)
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

// depthCoversLayer reports whether a sub resolved to `depth` still runs the
// given prefetch layer. Used by the reclaim path so waves of a layer the
// operator has since switched off — e.g. leftover pending L2 waves after
// flipping a sub to depth=l3 — are retired instead of revived. Without this a
// stale L2 wave set would rebuild its /debug cycle snapshot and flip the reclaim
// status to "L2 recovering" for the whole period until each wave individually
// skipped itself. Layers other than L2/L3 (none scheduled today) default to
// covered so an unrecognised value never silently drops a wave.
func depthCoversLayer(layer, depth string) bool {
	switch layer {
	case "L2":
		return depthHasL2(depth)
	case "L3":
		return depthHasL3(depth)
	}
	return true
}

// globalBindDefaultEnabled reports whether the global prefetch_default_depth
// covers L3. It only reads the global default and ignores per-sub overrides, so
// it is the fallback for effectiveL3Enabled when there is no crawl list yet.
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

// effectiveL3Enabled reports whether L3 (comment archiving) actually runs for
// the current crawl list — true if *any* active sub resolves to a depth
// covering L3 (its per-sub override in prefetch_sub_modes, else the global
// default). The /debug "bind mode" / "bind L3" badges use this so a per-sub
// depth that drops L3 (e.g. golang=depth:l2) turns the badge off. The old badge
// read only prefetch_default_depth and so stayed on for a sub switched to L2 via
// a per-sub override — the very case the operator was toggling. With no active
// subs it falls back to the global default so a fresh instance still reflects
// its configured intent.
func (s *Scheduler) effectiveL3Enabled() bool {
	subs := s.activeSubs()
	if len(subs) == 0 {
		return s.globalBindDefaultEnabled()
	}
	for _, sub := range subs {
		if depthHasL3(s.resolveSubDepth(sub)) {
			return true
		}
	}
	return false
}

// effectiveL2Enabled is the L2 (media) analogue of effectiveL3Enabled: true if
// any active sub's resolved depth covers L2. Drives the explicit "L2 enabled"
// status on /debug — previously there was no indicator for L2 at all (the panel
// only showed an L3-binding badge), so an operator could not tell from the page
// whether media prefetch was on. With no crawl list it falls back to the global
// default depth's L2 coverage.
func (s *Scheduler) effectiveL2Enabled() bool {
	subs := s.activeSubs()
	if len(subs) == 0 {
		if s.settings == nil {
			return true
		}
		v := strings.TrimSpace(s.settings.Get("prefetch_default_depth"))
		if v == "" {
			return true
		}
		c, ok := canonDepth(v)
		if !ok {
			return true
		}
		return depthHasL2(c)
	}
	for _, sub := range subs {
		if depthHasL2(s.resolveSubDepth(sub)) {
			return true
		}
	}
	return false
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

// fetchL3Standalone is the single-post L3 fetch primitive used by the L3 cycle
// (invoked from runL3Wave). It synchronously fetches a post's comments through
// the NP dispatcher using the same budget gate L1 uses (`needsBudget=true`).
// This keeps combined L1+L3 traffic from exceeding the 10-minute OAuth window —
// waitForBudget pauses the dispatch loop the moment remaining quota crosses the
// reserved threshold, so a busy cycle naturally stretches across multiple
// windows instead of burning the entire budget in one burst.
//
// Failures are logged but never propagated: an L3 miss must not block the
// surrounding wave's remaining posts.
func (s *Scheduler) fetchL3Standalone(ctx context.Context, tf, sub, postID, urlPath, cycleID string, numComments int) {
	s.fetchL3Single(ctx, tf, sub, postID, urlPath, cycleID, false, numComments)
}

// fetchL3Single fetches and archives one post's comments. numComments is the
// upstream-reported comment count L1 last stored for the post (Comments[1]); it
// is persisted into the ok-run payload as "num_comments" so a later L3 cycle
// can tell whether the thread has since grown (the ListL3Candidates freeze
// override re-admits grown threads). Pass 0 when the count is unknown.
func (s *Scheduler) fetchL3Single(ctx context.Context, tf, sub, postID, urlPath, cycleID string, bound bool, numComments int) {
	if s.cli == nil && s.l3FetchFn == nil {
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
		// l3FetchFn seam: tests record which posts reached the L3 pipeline and
		// short-circuit the live Reddit fetch + archive write. Still runs inside
		// submit() so the dispatcher cooldown + budget gate path is exercised.
		if s.l3FetchFn != nil {
			n, err := s.l3FetchFn(ctx, sub, postID, urlPath)
			s.recordUpstream(ctx)
			if err != nil {
				fetchErr = err
				s.Events.Addf(LevelWarn, "L3", "%s r/%s post %s: fetch failed: %v", mode, sub, postID, err)
				return
			}
			fetched = n
			s.Events.Addf(LevelOK, "L3", "%s r/%s post %s: archived %d comments", mode, sub, postID, fetched)
			return
		}
		_, comments, err := s.cli.FetchPost(ctx, sub, postID, commentSort)
		s.recordUpstream(ctx)
		if err != nil {
			fetchErr = err
			s.Events.Addf(LevelWarn, "L3", "%s r/%s post %s: fetch failed: %v", mode, sub, postID, err)
			return
		}
		if s.archiver != nil && urlPath != "" {
			s.archiver.ArchiveComments(urlPath, comments)
			// Comment bodies can embed signed preview images L2's post-only media
			// pass never saw. Re-arm the post's media queue so L2 (not L3) caches
			// them before the signatures expire.
			s.requeueForCommentMedia(urlPath, comments)
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
			// num_comments is the upstream-reported thread size at fetch time —
			// the rumination baseline a later cycle compares the fresh L1 count
			// against to decide "new replies since, re-fetch". Distinct from
			// "comments" (how many we actually archived this round).
			"num_comments": numComments,
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

	// For a link post, parseMedia sets Media.URL to the *external destination*
	// (the url field) — a blog/GitHub/etc. page, not a Reddit-CDN asset. It is
	// never downloadable media: the media proxy's host allow-list rejects any
	// non-redd.it host, and fetching the page yields HTML the cache discards as
	// a poisoned error page. Emitting it here made every link post (the bulk of
	// link-heavy subs like r/golang) fail its media wave, never reach
	// media_done, and re-fail on every subsequent wave — a flood of L2 warns.
	// Link posts cache only their thumbnail (handled below).
	if p.Media.URL != "" && p.PostType != "link" {
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
	// Images pasted directly into a self post's selftext (e.g. a footer
	// screenshot) live only in the rendered body HTML, not in the structured
	// Media/Gallery/Thumbnail fields above. Harvest them so the archive caches
	// the bytes while Reddit's signed preview URL is still valid — otherwise the
	// inline image 403s once the `s=` signature expires and the post renders a
	// permanent "Sorry, we missed it" placeholder. Skip any that duplicate a URL
	// already queued above (canonicalisation collapses them anyway, but this
	// keeps the per-post media-kind summary honest).
	if p.Body != "" {
		seen := make(map[string]bool, len(items))
		for _, it := range items {
			seen[reddit.CanonicalKey(it.URL)] = true
		}
		for _, raw := range reddit.ExtractBodyImageURLs(string(p.Body)) {
			if key := reddit.CanonicalKey(raw); !seen[key] {
				seen[key] = true
				items = append(items, mediaItem{URL: raw, Kind: "image"})
			}
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
	L1Phase        string
	L1Round        int
	L1MaxRounds    int
	L1Subs         []string
	L1Cursors      map[string]string
	L1NextCycle    string
	L1NextCycleAbs string
	// L1Buckets is the per-bucket schedule snapshot for the debug page. One
	// entry per active bucket, ordered finest-to-coarsest (hour → all).
	L1Buckets []PrefetchBucketStatus
	L2Phase   string
	L2Sub     string
	L2Pending int
	// L2Enabled / L3Enabled report whether the layer actually runs for the
	// current crawl list (effective per-sub depth, not just the global default).
	// They drive the explicit on/off status shown per layer on /debug.
	L2Enabled      bool
	L2Cycles       []PrefetchL2Cycle
	L5Phase        string
	L5Current      string
	L5Pending      int
	L3Phase        string
	L3Current      string
	L3LastAt       string
	L3LastAtAbs    string
	L3Count        int
	L3Enabled      bool
	L3Recent       []PrefetchL3Bind
	L3Cycles       []PrefetchL2Cycle
	L4Phase        string
	L4Current      string
	L4QueueLen     int
	L4NextTick     string
	L4NextTickAbs  string
	NPPhase        string
	NPCurrent      string
	QueueLen       int
	Enabled        bool
	ActiveSubs     []string
	ReclaimL2Phase string
	ReclaimL2Info  string
	ReclaimL3Phase string
	ReclaimL3Info  string
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

	// Suppress a stale "recovering" banner if the sub's depth no longer covers
	// the layer. A driveReclaimedCycle goroutine parked on a *future* wave sets
	// the banner before its long sleep and only re-checks depth at the top of the
	// next wave iteration — so an operator disabling the layer mid-sleep (e.g.
	// flipping a sub to depth=l3) would otherwise leave a phantom "L2 recovering"
	// on /debug for the rest of the period. Re-resolving depth here makes the
	// banner reflect CURRENT settings regardless of in-flight reclaim state.
	reclaimL2Phase, reclaimL2Info := s.reclaimL2Phase, s.reclaimL2Info
	if reclaimL2Phase != "" && s.reclaimL2Sub != "" && !depthCoversLayer("L2", s.resolveSubDepth(s.reclaimL2Sub)) {
		reclaimL2Phase, reclaimL2Info = "", ""
	}
	reclaimL3Phase, reclaimL3Info := s.reclaimL3Phase, s.reclaimL3Info
	if reclaimL3Phase != "" && s.reclaimL3Sub != "" && !depthCoversLayer("L3", s.resolveSubDepth(s.reclaimL3Sub)) {
		reclaimL3Phase, reclaimL3Info = "", ""
	}

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
		L1Phase:        s.l1Phase,
		L1Round:        s.l1Round,
		L1MaxRounds:    s.l1MaxRounds,
		L1Subs:         subs,
		L1Cursors:      cursors,
		L1NextCycle:    nextCycle,
		L1NextCycleAbs: nextCycleAbs,
		L1Buckets:      buckets,
		L2Phase:        s.l2Phase,
		L2Sub:          s.l2Sub,
		L2Pending:      s.l2Pending,
		L2Enabled:      s.effectiveL2Enabled(),
		L2Cycles:       s.snapshotL2Cycles(),
		L5Phase:        s.l5Phase,
		L5Current:      s.l5Current,
		L5Pending:      s.l5Pending,
		L3Phase:        s.l3Phase,
		L3Current:      s.l3Current,
		L3LastAt:       l3LastAt,
		L3LastAtAbs:    l3LastAtAbs,
		L3Count:        s.l3Count,
		L3Enabled:      s.effectiveL3Enabled(),
		L3Recent:       s.snapshotL3Recent(),
		L3Cycles:       s.snapshotL3Cycles(),
		L4Phase:        s.l4Phase,
		L4Current:      s.l4Current,
		L4QueueLen:     s.l4QueueLen,
		L4NextTick:     l4NextTick,
		L4NextTickAbs:  l4NextTickAbs,
		NPPhase:        s.npPhase,
		NPCurrent:      s.npCurrent,
		QueueLen:       len(s.queue),
		Enabled:        s.isEnabled(),
		ActiveSubs:     s.activeSubs(),
		ReclaimL2Phase: reclaimL2Phase,
		ReclaimL2Info:  reclaimL2Info,
		ReclaimL3Phase: reclaimL3Phase,
		ReclaimL3Info:  reclaimL3Info,
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
func (s *Scheduler) advanceL2Wave(tf, sub, cycleID string, wave int) {
	s.statusMu.Lock()
	if c, ok := s.l2Cycles[l2CycleKey(tf, sub)]; ok && c.cycleID == cycleID {
		c.currentWave = wave
	}
	s.statusMu.Unlock()
}

// dropL2Cycle removes the cycle entry once all waves have fired (or the cycle
// aborted) so /debug only shows in-flight cycles.
func (s *Scheduler) dropL2Cycle(tf, sub, cycleID string) {
	s.statusMu.Lock()
	// cycleID guard: a stale, still-draining previous cycle's deferred drop must
	// not delete the snapshot of a newer cycle that took over the (tf,sub) slot
	// one period later (overlapping cycles share the same map key).
	if c, ok := s.l2Cycles[l2CycleKey(tf, sub)]; ok && c.cycleID == cycleID {
		delete(s.l2Cycles, l2CycleKey(tf, sub))
	}
	s.statusMu.Unlock()
}

func l2CycleKey(tf, sub string) string { return tf + "|" + sub }

// recordL3CycleStart / advanceL3Wave / dropL3Cycle are the L3 analogues of the
// L2 cycle-snapshot helpers, operating on s.l3Cycles so the standalone comment
// layer gets the same live /debug wave view. bindMode is always false for L3
// (it is no longer bound to L2); the field is retained only for view symmetry.
func (s *Scheduler) recordL3CycleStart(tf, sub string, postCount int, chunks []int, cycleStart time.Time, period time.Duration, offsets []time.Duration, cycleID string) {
	off := append([]time.Duration(nil), offsets...)
	ch := append([]int(nil), chunks...)
	s.statusMu.Lock()
	if s.l3Cycles == nil {
		s.l3Cycles = make(map[string]*l2CycleSnap)
	}
	s.l3Cycles[l2CycleKey(tf, sub)] = &l2CycleSnap{
		tf:          tf,
		sub:         sub,
		postCount:   postCount,
		waveChunks:  ch,
		cycleStart:  cycleStart,
		period:      period,
		waveOffsets: off,
		currentWave: 0,
		cycleID:     cycleID,
	}
	s.statusMu.Unlock()
}

func (s *Scheduler) advanceL3Wave(tf, sub, cycleID string, wave int) {
	s.statusMu.Lock()
	if c, ok := s.l3Cycles[l2CycleKey(tf, sub)]; ok && c.cycleID == cycleID {
		c.currentWave = wave
	}
	s.statusMu.Unlock()
}

func (s *Scheduler) dropL3Cycle(tf, sub, cycleID string) {
	s.statusMu.Lock()
	if c, ok := s.l3Cycles[l2CycleKey(tf, sub)]; ok && c.cycleID == cycleID {
		delete(s.l3Cycles, l2CycleKey(tf, sub))
	}
	s.statusMu.Unlock()
}

// dropL3CycleAny removes the live L3 cycle snapshot for (tf, sub) regardless of
// cycle id. Unlike dropL3Cycle (cycle-id guarded so a stale deferred drop can't
// delete a newer cycle), this is used by retireStandaloneL3 when L3 is switched
// off for the sub: there is no successor cycle to protect, and the phantom
// snapshot left behind by a disable-pause must be cleared unconditionally.
func (s *Scheduler) dropL3CycleAny(tf, sub string) {
	s.statusMu.Lock()
	delete(s.l3Cycles, l2CycleKey(tf, sub))
	s.statusMu.Unlock()
}

// cycleWaveCount returns the total number of waves planned for a (layer, cycle).
// L2 is always l2WavesPerCycle; L3's count is variable, so it is read from the
// live snapshot. Used only to label "wave i/N" event-log lines accurately —
// callers fall back to l2WavesPerCycle when no snapshot is present.
func (s *Scheduler) cycleWaveCount(layer, tf, sub, cycleID string) int {
	if layer != "L3" {
		return l2WavesPerCycle
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if c, ok := s.l3Cycles[l2CycleKey(tf, sub)]; ok && c != nil && c.cycleID == cycleID && len(c.waveOffsets) > 0 {
		return len(c.waveOffsets)
	}
	// L3's wave count is variable and only known from the live snapshot; with no
	// snapshot we genuinely don't know it. Return 0 (rendered as "?" by
	// fmtWaveTotal) rather than L2's fixed l2WavesPerCycle, which would mislabel
	// an L3 cycle as "wave i/5".
	return 0
}

// fmtWaveTotal renders a wave-count total for event-log "wave i/N" labels,
// substituting "?" when the total is unknown (0 — see cycleWaveCount's L3
// no-snapshot fallback) so a log line never claims a wrong fixed count.
func fmtWaveTotal(total int) string {
	if total <= 0 {
		return "?"
	}
	return strconv.Itoa(total)
}

// snapshotL2Cycles / snapshotL3Cycles materialize the live L2 / L3 cycle maps
// into a stable, sorted slice for the /debug panels. Caller already holds
// statusMu (RLock or Lock).
func (s *Scheduler) snapshotL2Cycles() []PrefetchL2Cycle {
	return s.snapshotCoveredCycles("L2", s.l2Cycles)
}
func (s *Scheduler) snapshotL3Cycles() []PrefetchL2Cycle {
	return s.snapshotCoveredCycles("L3", s.l3Cycles)
}

// snapshotCoveredCycles drops any cycle whose sub's CURRENT resolved depth no
// longer covers `layer` before formatting. A leftover L2 cycle snapshot rebuilt
// by reclaim (rebuildL2CycleSnapshot) survives in l2Cycles until its last wave
// fires; if the operator disables the layer while the reclaim driver is parked
// on a future wave, that snapshot would otherwise render as a phantom L2/L3
// cycle panel for a sub that no longer runs the layer. Filtering here keeps the
// /debug panels in lock-step with current depth — the same contract the
// reclaim-path depth gates enforce, applied at the presentation boundary.
func (s *Scheduler) snapshotCoveredCycles(layer string, m map[string]*l2CycleSnap) []PrefetchL2Cycle {
	if len(m) == 0 {
		return nil
	}
	covered := make(map[string]*l2CycleSnap, len(m))
	for k, c := range m {
		if depthCoversLayer(layer, s.resolveSubDepth(c.sub)) {
			covered[k] = c
		}
	}
	return snapshotCycles(covered)
}

// snapshotCycles is the shared formatter behind both layer snapshots — the
// PrefetchL2Cycle view shape (offsets, per-wave intervals+chunks, current wave)
// is identical for L2 media waves and L3 comment waves.
func snapshotCycles(m map[string]*l2CycleSnap) []PrefetchL2Cycle {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]PrefetchL2Cycle, 0, len(keys))
	for _, k := range keys {
		c := m[k]
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
func (s *Scheduler) RecordL3Fetch(sub, postID string, commentCount, numComments int) {
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
			// Record the upstream-reported thread size so an on-demand view sets
			// the same rumination baseline a prefetch fetch would — otherwise the
			// most-recent-run lookup would see a baseline-less row and mask the
			// prefetch baseline, stalling rumination for user-viewed posts.
			"num_comments": numComments,
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

func (s *Scheduler) setReclaimStatus(layer, sub string, current, total, remaining int) {
	info := fmt.Sprintf("r/%s wave %d/%d (%d remaining)", sub, current, total, remaining)
	s.statusMu.Lock()
	switch layer {
	case "L2":
		s.reclaimL2Phase = "recovering"
		s.reclaimL2Info = info
		s.reclaimL2Sub = sub
	case "L3":
		s.reclaimL3Phase = "recovering"
		s.reclaimL3Info = info
		s.reclaimL3Sub = sub
	}
	s.statusMu.Unlock()
}

func (s *Scheduler) clearReclaimStatus(layer string) {
	s.statusMu.Lock()
	switch layer {
	case "L2":
		s.reclaimL2Phase = ""
		s.reclaimL2Info = ""
		s.reclaimL2Sub = ""
	case "L3":
		s.reclaimL3Phase = ""
		s.reclaimL3Info = ""
		s.reclaimL3Sub = ""
	}
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
