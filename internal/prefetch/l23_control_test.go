package prefetch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

// TestDriveWaves_PausesOnMidCycleDisable pins the aggressive-disable contract:
// flipping a layer's depth off mid-cycle stops further waves at the next
// pre-fire depth re-check and reports driveDisabled, so the caller keeps the
// plan instead of dropping it.
func TestDriveWaves_PausesOnMidCycleDisable(t *testing.T) {
	ms := &mockSettings{data: map[string]string{"prefetch_default_depth": "l2+l3"}}
	s := &Scheduler{Events: NewEventLog(20), settings: ms}

	chunks := []int{1, 1, 1}
	offsets := []time.Duration{0, 0, 0} // all due now → no sleeps
	fired := 0
	runWave := func(_ context.Context, _, _ string, _ int, _ string, _ int) error {
		fired++
		// Operator disables L3 partway through the cycle.
		ms.data["prefetch_default_depth"] = "l2"
		return nil
	}

	out := s.driveWaves(context.Background(), "L3", "day", "golang", "cyc-1",
		chunks, offsets, time.Now(), time.Hour, 3, nil, nil, runWave)

	if out != driveDisabled {
		t.Fatalf("outcome = %v, want driveDisabled", out)
	}
	if fired != 1 {
		t.Fatalf("fired %d wave(s), want exactly 1 before the disable pause", fired)
	}
}

// TestDriveWaves_CompletesWhenEnabled is the control case: a layer left enabled
// fires every wave and reports driveDone.
func TestDriveWaves_CompletesWhenEnabled(t *testing.T) {
	ms := &mockSettings{data: map[string]string{"prefetch_default_depth": "l2+l3"}}
	s := &Scheduler{Events: NewEventLog(20), settings: ms}

	fired := 0
	runWave := func(_ context.Context, _, _ string, _ int, _ string, _ int) error {
		fired++
		return nil
	}
	out := s.driveWaves(context.Background(), "L3", "day", "golang", "cyc-1",
		[]int{1, 1, 1}, []time.Duration{0, 0, 0}, time.Now(), time.Hour, 3, nil, nil, runWave)

	if out != driveDone {
		t.Fatalf("outcome = %v, want driveDone", out)
	}
	if fired != 3 {
		t.Fatalf("fired %d wave(s), want 3", fired)
	}
}

// TestEffectiveL3Enabled drives the /debug bind-mode badge logic. The headline
// regression: a per-sub depth override that drops L3 (golang=depth:l2) must turn
// the badge OFF even while the global default still covers L3 — the old badge
// read only prefetch_default_depth and stayed stuck "on".
func TestEffectiveL3Enabled(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
		want bool
	}{
		{"global none (L1 only)", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "none"}, false},
		{"global l2", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2"}, false},
		{"global l3", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l3"}, true},
		{"global l2+l3", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2+l3"}, true},
		// Regression: per-sub override drops L3 while the global default keeps it.
		{"per-sub l2 over global l2+l3", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2+l3",
			"prefetch_sub_modes": "golang=depth:l2"}, false},
		{"per-sub none over global l2+l3", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2+l3",
			"prefetch_sub_modes": "golang=depth:none"}, false},
		// Per-sub override re-enables L3 while the global default is L2-only.
		{"per-sub l3 over global l2", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2",
			"prefetch_sub_modes": "golang=depth:l3"}, true},
		// One sub keeps L3 → badge on even if another dropped it.
		{"mixed subs, one keeps L3", map[string]string{
			"prefetch_subs": "sub:golang+rust", "prefetch_default_depth": "l2+l3",
			"prefetch_sub_modes": "golang=depth:l2"}, true},
		// No crawl list → fall back to the global default intent.
		{"no subs falls back to global l2+l3", map[string]string{
			"prefetch_subs": "", "prefetch_default_depth": "l2+l3"}, true},
		{"no subs falls back to global none", map[string]string{
			"prefetch_subs": "", "prefetch_default_depth": "none"}, false},
	}
	for _, c := range cases {
		s := &Scheduler{settings: &mockSettings{data: c.data}}
		if got := s.effectiveL3Enabled(); got != c.want {
			t.Errorf("%s: effectiveL3Enabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStatusEnabled_ReflectsPerSubDepth is the end-to-end check that Status()
// surfaces the per-sub-aware enabled flags (L2Enabled / L3Enabled) the /debug
// template renders. With a per-sub depth:l2 override, L2 stays on and L3 goes
// off — independently.
func TestStatusEnabled_ReflectsPerSubDepth(t *testing.T) {
	s := &Scheduler{
		Events: NewEventLog(4),
		queue:  make(chan *workItem, 1),
		settings: &mockSettings{data: map[string]string{
			"prefetch_subs":          "sub:golang",
			"prefetch_default_depth": "l2+l3",
			"prefetch_sub_modes":     "golang=depth:l2",
		}},
	}
	st := s.Status()
	if !st.L2Enabled {
		t.Errorf("per-sub depth:l2 must show L2 enabled ON, got %v", st.L2Enabled)
	}
	if st.L3Enabled {
		t.Errorf("per-sub depth:l2 must show L3 enabled OFF, got %v", st.L3Enabled)
	}
}

// TestEffectiveL2Enabled mirrors the L3 table for the media layer's status.
func TestEffectiveL2Enabled(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
		want bool
	}{
		{"global none (L1 only)", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "none"}, false},
		{"global l2", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2"}, true},
		{"global l3 (comments only, no media)", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l3"}, false},
		{"global l2+l3", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2+l3"}, true},
		{"per-sub l3 over global l2+l3 drops media", map[string]string{
			"prefetch_subs": "sub:golang", "prefetch_default_depth": "l2+l3",
			"prefetch_sub_modes": "golang=depth:l3"}, false},
		{"no subs falls back to global l2+l3", map[string]string{
			"prefetch_subs": "", "prefetch_default_depth": "l2+l3"}, true},
	}
	for _, c := range cases {
		s := &Scheduler{settings: &mockSettings{data: c.data}}
		if got := s.effectiveL2Enabled(); got != c.want {
			t.Errorf("%s: effectiveL2Enabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestL3CycleChunksInvalid pins the hangover invariant: a plan must fully cover
// post_count and never exceed l3WaveCap, else it is discarded.
func TestL3CycleChunksInvalid(t *testing.T) {
	cases := []struct {
		name      string
		chunks    []int
		postCount int
		wantBad   bool
	}{
		// The exact stale plan seen on the instance: old fixed-5-wave planner
		// dropped overflow (sum 48 < 82) — must be flagged.
		{"old 5-wave plan dropping overflow", []int{10, 8, 10, 10, 10}, 82, true},
		// A correct new plan: 17 waves around 5, summing to the full post_count.
		{"new full-coverage plan", []int{5, 4, 6, 5, 4, 5, 6, 4, 5, 5, 4, 6, 5, 4, 5, 4, 5}, 82, false},
		{"wave exceeds cap", []int{5, 5, 11, 5}, 26, true},
		{"exact small coverage", []int{1, 1, 1}, 3, false},
		{"undercoverage small", []int{1, 1}, 5, true},
		{"zero postCount always valid", []int{}, 0, false},
	}
	for _, c := range cases {
		if _, bad := l3CycleChunksInvalid(c.chunks, c.postCount); bad != c.wantBad {
			t.Errorf("%s: l3CycleChunksInvalid=%v, want %v", c.name, bad, c.wantBad)
		}
	}
}

// TestL3PlanHangover_FromRows checks the DB-facing detector reconstructs the
// plan from persisted prefetch_runs payloads and flags the stale cycle.
func TestL3PlanHangover_FromRows(t *testing.T) {
	mkRows := func(chunks []int, postCount int) []store.PrefetchRun {
		var rows []store.PrefetchRun
		for i, c := range chunks {
			payload, _ := json.Marshal(map[string]any{"chunk": c, "post_count": postCount, "wave": i + 1})
			rows = append(rows, store.PrefetchRun{Layer: "L3", Status: "pending", Payload: payload})
		}
		return rows
	}
	full17 := []int{5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 4, 3} // sums to 82
	// Stale: 5 waves, sum 48 < post_count 82 (l1Count irrelevant here).
	if _, bad := l3PlanHangover(mkRows([]int{10, 8, 10, 10, 10}, 82), 82); !bad {
		t.Error("expected stale 5-wave/82-post cycle to be flagged as hangover")
	}
	// Healthy: full coverage, matches the L1 round size.
	if _, bad := l3PlanHangover(mkRows(full17, 82), 82); bad {
		t.Error("full-coverage 17-wave cycle matching L1 round must not be flagged")
	}
	// Oversized: post_count 184 overshoots the L1 round of 82 → the runaway bug.
	big := make([]int, 37)
	for i := range big {
		big[i] = 5
	}
	big[0] = 4 // 36*5 + 4 = 184
	if _, bad := l3PlanHangover(mkRows(big, 184), 82); !bad {
		t.Error("expected oversized 184-post cycle to be flagged against L1 round 82")
	}
	// Unknown L1 round size (0) → over-size check skipped, full-coverage stays valid.
	if _, bad := l3PlanHangover(mkRows(big, 184), 0); bad {
		t.Error("with unknown L1 round size, a self-consistent plan must not be flagged")
	}
	// Regression (stale-reference misfire): a healthy cycle sized to its OWN live
	// L1 round must pass even when post_count == l1Count exactly. In production a
	// depth=l3 sub stopped writing L2's `post_count`, so LastCyclePostCount froze
	// at a stale 85 from the last L2 run while the live L1 round grew to 99; the
	// guard then discarded the perfectly-valid 99-post cycle every restart. The
	// fix makes the reference track L1's `fetched`, so the count it sees equals
	// the cycle's own post_count — which must NOT be flagged at the boundary.
	full99 := make([]int, 20) // 20 waves covering 99 posts (planL3Waves shape)
	{
		chunks, _ := planL3Waves(99, 12*time.Hour)
		full99 = chunks
	}
	if _, bad := l3PlanHangover(mkRows(full99, 99), 99); bad {
		t.Error("a cycle sized to its own live L1 round (99==99) must survive — the stale-reference regression")
	}
}

// TestPlanL3Waves_NeverHangover ties the generator to the validator: whatever
// planL3Waves rolls must always pass the hangover invariant, so the cleanup can
// never discard a freshly generated cycle.
func TestPlanL3Waves_NeverHangover(t *testing.T) {
	for postCount := 0; postCount <= 640; postCount += 11 {
		chunks, _ := planL3Waves(postCount, 12*time.Hour)
		if reason, bad := l3CycleChunksInvalid(chunks, postCount); bad {
			t.Errorf("planL3Waves(%d) produced a hangover (%s): chunks=%v", postCount, reason, chunks)
		}
	}
}

// TestReconcilePostCount_FallsBackToPageLimit pins that a regenerated cycle is
// sized like one L1 round (page_limit), NOT the whole archive backlog — the bug
// that produced a 184-post / 37-wave cycle. With no runStore the helper falls
// through to page_limit, then to the default.
func TestReconcilePostCount_FallsBackToPageLimit(t *testing.T) {
	s := &Scheduler{settings: &mockSettings{data: map[string]string{"page_limit": "50"}}}
	if got := s.reconcilePostCount("day", "golang"); got != 50 {
		t.Errorf("reconcilePostCount with page_limit=50 = %d, want 50", got)
	}
	s2 := &Scheduler{settings: &mockSettings{data: map[string]string{}}}
	if got := s2.reconcilePostCount("day", "golang"); got != defaultReconcilePostCount {
		t.Errorf("reconcilePostCount with no page_limit = %d, want %d", got, defaultReconcilePostCount)
	}
	// Sanity: a page-limit-sized plan is small (≈10 waves), never a 37-wave runaway.
	chunks, _ := planL3Waves(50, 12*time.Hour)
	if len(chunks) > 12 {
		t.Errorf("page-limit-sized L3 plan has %d waves, expected ~10", len(chunks))
	}
}

// TestPruneCursors drops cursors for subs no longer in the crawl list — the
// phantom r/gfur cursor that lingered on /debug after gfur was removed.
func TestPruneCursors(t *testing.T) {
	cursors := map[string]string{
		"golang|hot":    "t3_aaa",
		"gfur|hot":      "t3_bbb", // abandoned sub — must be pruned
		"rust|top|week": "t3_ccc", // abandoned sub with timeframe key
		"GoLang|new":    "t3_ddd", // case-insensitive keep
	}
	pruneCursors(cursors, []string{"golang"})
	if _, ok := cursors["gfur|hot"]; ok {
		t.Error("gfur cursor should have been pruned")
	}
	if _, ok := cursors["rust|top|week"]; ok {
		t.Error("rust cursor should have been pruned")
	}
	if _, ok := cursors["golang|hot"]; !ok {
		t.Error("golang cursor must be kept")
	}
	if _, ok := cursors["GoLang|new"]; !ok {
		t.Error("golang cursor (mixed case) must be kept")
	}
	if len(cursors) != 2 {
		t.Errorf("expected 2 cursors kept, got %d: %v", len(cursors), cursors)
	}
}

// seedL3Snap installs a live /debug L3 cycle snapshot for (tf, sub) so a test
// can assert whether a code path clears it.
func seedL3Snap(s *Scheduler, tf, sub, cycleID string, waves int) {
	offsets := make([]time.Duration, waves)
	chunks := make([]int, waves)
	for i := range offsets {
		offsets[i] = time.Duration(i) * time.Minute
		chunks[i] = 5
	}
	s.statusMu.Lock()
	if s.l3Cycles == nil {
		s.l3Cycles = make(map[string]*l2CycleSnap)
	}
	s.l3Cycles[l2CycleKey(tf, sub)] = &l2CycleSnap{
		tf: tf, sub: sub, waveOffsets: offsets, waveChunks: chunks, cycleID: cycleID,
	}
	s.statusMu.Unlock()
}

func hasL3Snap(s *Scheduler, tf, sub string) bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	_, ok := s.l3Cycles[l2CycleKey(tf, sub)]
	return ok
}

// TestDropL3CycleAny verifies the unconditional drop clears the snapshot even
// when the caller does not know (or match) the live cycle id — the phantom-snap
// cleanup retireStandaloneL3 relies on.
func TestDropL3CycleAny(t *testing.T) {
	s := &Scheduler{}
	seedL3Snap(s, "day", "golang", "L3:day:golang:111", 16)
	// A cycle-id-guarded drop with the WRONG id must NOT remove it.
	s.dropL3Cycle("day", "golang", "L3:day:golang:999")
	if !hasL3Snap(s, "day", "golang") {
		t.Fatal("cycle-id-guarded dropL3Cycle wrongly removed a non-matching snapshot")
	}
	// dropL3CycleAny removes it regardless of id.
	s.dropL3CycleAny("day", "golang")
	if hasL3Snap(s, "day", "golang") {
		t.Fatal("dropL3CycleAny did not remove the snapshot")
	}
}

// TestRetireStandaloneL3_ClearsSnapshot pins the in-memory half of the orphaned-
// L3 sweep: with no runStore the supersede is skipped (nil-guarded) but the
// phantom /debug snapshot is still cleared.
func TestRetireStandaloneL3_ClearsSnapshot(t *testing.T) {
	s := &Scheduler{Events: NewEventLog(8)}
	seedL3Snap(s, "day", "golang", "L3:day:golang:111", 16)
	s.retireStandaloneL3("day", "golang")
	if hasL3Snap(s, "day", "golang") {
		t.Fatal("retireStandaloneL3 left a phantom L3 cycle snapshot")
	}
}

// TestRunL2Cycle_RetiresOrphanedL3 is the regression for the headline bug: once a
// sub's depth no longer covers L3, the next L1 fan-out (runL2Cycle) must clear
// the leftover /debug L3 cycle snapshot — instead of letting it linger as a
// phantom "L3 off / wave 5/16" until restart. The control case (depth still
// covers L3) must keep it.
func TestRunL2Cycle_RetiresOrphanedL3(t *testing.T) {
	cases := []struct {
		depth    string
		wantSnap bool
	}{
		{"l2", false},   // L3 switched off → snapshot swept
		{"none", false}, // everything off → snapshot swept
		{"l2+l3", true}, // L3 still on → snapshot preserved
		{"l3", true},    // comments-only, still on → preserved
	}
	for _, c := range cases {
		t.Run(c.depth, func(t *testing.T) {
			s := &Scheduler{
				Events:   NewEventLog(8),
				settings: &mockSettings{data: map[string]string{"prefetch_default_depth": c.depth}},
				// postStore/media/runStore/l3CandidatesFn all nil: runL2Cycle's L3
				// fan-out and L2 drive both bail on their nil-guards before touching
				// the seeded snapshot, so only the retire path can clear it.
			}
			seedL3Snap(s, "day", "golang", "L3:day:golang:111", 16)
			s.runL2Cycle(context.Background(), "day", "golang", 10, time.Hour, "day:golang:111")
			if got := hasL3Snap(s, "day", "golang"); got != c.wantSnap {
				t.Fatalf("depth=%s: L3 snapshot present=%v, want %v", c.depth, got, c.wantSnap)
			}
		})
	}
}

// TestCycleWaveCount pins the per-layer wave-count source: L2 is the fixed
// constant; L3 reads the live snapshot and reports an unknown (0) total — never
// L2's 5 — when no matching snapshot exists.
func TestCycleWaveCount(t *testing.T) {
	s := &Scheduler{}
	if got := s.cycleWaveCount("L2", "day", "golang", "anything"); got != l2WavesPerCycle {
		t.Errorf("L2 wave count = %d, want %d", got, l2WavesPerCycle)
	}
	// No snapshot → unknown, not 5.
	if got := s.cycleWaveCount("L3", "day", "golang", "L3:day:golang:111"); got != 0 {
		t.Errorf("L3 wave count with no snapshot = %d, want 0 (unknown)", got)
	}
	seedL3Snap(s, "day", "golang", "L3:day:golang:111", 16)
	if got := s.cycleWaveCount("L3", "day", "golang", "L3:day:golang:111"); got != 16 {
		t.Errorf("L3 wave count from snapshot = %d, want 16", got)
	}
	// Cycle-id mismatch → unknown again (a stale snapshot must not mislabel).
	if got := s.cycleWaveCount("L3", "day", "golang", "L3:day:golang:222"); got != 0 {
		t.Errorf("L3 wave count with mismatched cycle id = %d, want 0", got)
	}
}

func TestFmtWaveTotal(t *testing.T) {
	cases := map[int]string{0: "?", -1: "?", 1: "1", 16: "16"}
	for in, want := range cases {
		if got := fmtWaveTotal(in); got != want {
			t.Errorf("fmtWaveTotal(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestDriverTracking verifies the driver counter that reconcileLoop reads to
// avoid double-driving a layer that is already firing.
func TestDriverTracking(t *testing.T) {
	s := &Scheduler{}
	if s.driverActive("L3", "day", "golang") {
		t.Fatal("expected no active driver initially")
	}
	s.driverEnter("L3", "day", "golang")
	s.driverEnter("L3", "day", "golang") // overlapping cycle for same key
	if !s.driverActive("L3", "day", "golang") {
		t.Fatal("expected active driver after enter")
	}
	if s.driverActive("L2", "day", "golang") {
		t.Fatal("L2 key must be independent of L3 key")
	}
	s.driverExit("L3", "day", "golang")
	if !s.driverActive("L3", "day", "golang") {
		t.Fatal("still one driver left after a single exit")
	}
	s.driverExit("L3", "day", "golang")
	if s.driverActive("L3", "day", "golang") {
		t.Fatal("expected no active driver after balanced exits")
	}
}
