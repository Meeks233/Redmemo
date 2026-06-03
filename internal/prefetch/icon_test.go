package prefetch

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

// ---------------------------------------------------------------------------
// Mocks specific to L4 icon tests
// ---------------------------------------------------------------------------

type mockSubStatus struct {
	alive []string
	err   error
}

func (m *mockSubStatus) IsAlive(string) (bool, error)        { return true, nil }
func (m *mockSubStatus) MarkLive(string) error               { return nil }
func (m *mockSubStatus) RecordFailure(string, string) error  { return nil }
func (m *mockSubStatus) ListAllAlive() ([]string, error)     { return m.alive, m.err }

type mockIconStore struct {
	mu       sync.Mutex
	icons    map[string]*store.SubIcon
	getErrOn map[string]error
	saved    []*store.SubIcon
}

func (m *mockIconStore) Get(name string) (*store.SubIcon, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.getErrOn[name]; ok {
		return nil, e
	}
	return m.icons[name], nil
}

func (m *mockIconStore) Save(icon *store.SubIcon) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.icons == nil {
		m.icons = map[string]*store.SubIcon{}
	}
	m.icons[icon.Name] = icon
	m.saved = append(m.saved, icon)
	return nil
}

func (m *mockIconStore) SaveAbout(string, []byte) error               { return nil }
func (m *mockIconStore) ListExpired() ([]*store.SubIcon, error)       { return nil, nil }
func (m *mockIconStore) ListAll() ([]*store.SubIcon, error)           { return nil, nil }
func (m *mockIconStore) IconTTL() time.Duration                       { return 30 * 24 * time.Hour }

func newIconTestScheduler(alive []string, icons map[string]*store.SubIcon) *Scheduler {
	if icons == nil {
		icons = map[string]*store.SubIcon{}
	}
	return &Scheduler{
		subStatus: &mockSubStatus{alive: alive},
		iconStore: &mockIconStore{icons: icons},
		Events:    NewEventLog(50),
	}
}

// ---------------------------------------------------------------------------
// previewList
// ---------------------------------------------------------------------------

func TestPreviewList(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"nil", nil, "[]"},
		{"empty", []string{}, "[]"},
		{"short", []string{"a", "b", "c"}, "[a b c]"},
		{"exactly cap", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, "[a b c d e f g h i j]"},
		{"one over cap", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}, "[a b c d e f g h i j] ...(+1 more)"},
		{"many over cap", append([]string{}, makeNames(25)...), fmt.Sprintf("%v ...(+15 more)", makeNames(10))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := previewList(tt.in); got != tt.want {
				t.Errorf("previewList(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func makeNames(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("s%02d", i)
	}
	return out
}

// ---------------------------------------------------------------------------
// sortSubsByPostCount
// ---------------------------------------------------------------------------

func TestSortSubsByPostCount(t *testing.T) {
	subs := []string{"low", "high", "midZ", "midA"}
	counts := map[string]int{
		"low":  10,
		"high": 100,
		"midz": 50,
		"mida": 50,
	}
	sortSubsByPostCount(subs, counts)
	want := []string{"high", "midA", "midZ", "low"}
	for i := range want {
		if subs[i] != want[i] {
			t.Errorf("idx %d: got %q, want %q (full=%v)", i, subs[i], want[i], subs)
		}
	}
}

func TestSortSubsByPostCount_CaseInsensitiveLookup(t *testing.T) {
	subs := []string{"Golang", "RUST"}
	counts := map[string]int{
		"golang": 10,
		"rust":   100,
	}
	sortSubsByPostCount(subs, counts)
	if subs[0] != "RUST" {
		t.Errorf("expected RUST first, got %v", subs)
	}
}

func TestSortSubsByPostCount_MissingCountsAreZero(t *testing.T) {
	subs := []string{"unknown", "knownLow", "knownHigh"}
	counts := map[string]int{
		"knownlow":  5,
		"knownhigh": 50,
	}
	sortSubsByPostCount(subs, counts)
	// Expected: knownHigh (50), knownLow (5), unknown (0)
	want := []string{"knownHigh", "knownLow", "unknown"}
	for i := range want {
		if subs[i] != want[i] {
			t.Errorf("idx %d: got %q, want %q (full=%v)", i, subs[i], want[i], subs)
		}
	}
}

// ---------------------------------------------------------------------------
// buildIconRound
// ---------------------------------------------------------------------------

func TestBuildIconRound_EmptyAlive(t *testing.T) {
	s := newIconTestScheduler(nil, nil)
	if got := s.buildIconRound(); got != nil {
		t.Errorf("expected nil for empty alive, got %v", got)
	}
}

func TestBuildIconRound_IncludesMissingIcons(t *testing.T) {
	s := newIconTestScheduler([]string{"a", "b"}, nil)
	got := s.buildIconRound()
	if len(got) != 2 {
		t.Errorf("expected 2 candidates, got %v", got)
	}
}

func TestBuildIconRound_SkipsFreshIcons(t *testing.T) {
	future := time.Now().Add(time.Hour)
	s := newIconTestScheduler([]string{"a"}, map[string]*store.SubIcon{
		"a": {Name: "a", HasIcon: true, ExpiresAt: future, IconURL: "http://x"},
	})
	if got := s.buildIconRound(); len(got) != 0 {
		t.Errorf("fresh icon should be skipped, got %v", got)
	}
}

func TestBuildIconRound_IncludesExpiredIcons(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	s := newIconTestScheduler([]string{"a"}, map[string]*store.SubIcon{
		"a": {Name: "a", HasIcon: true, ExpiresAt: past, IconURL: "http://x"},
	})
	got := s.buildIconRound()
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("expired should be included, got %v", got)
	}
}

func TestBuildIconRound_HasIconFalseIsStickySkip(t *testing.T) {
	// Even with an expired ExpiresAt, has_icon=false must never re-enter the queue.
	past := time.Now().Add(-30 * 24 * time.Hour)
	s := newIconTestScheduler([]string{"golang"}, map[string]*store.SubIcon{
		"golang": {Name: "golang", HasIcon: false, ExpiresAt: past},
	})
	if got := s.buildIconRound(); len(got) != 0 {
		t.Errorf("has_icon=false should be sticky-skipped, got %v", got)
	}
}

func TestBuildIconRound_RecentUserActivityIsSkipped(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	recent := time.Now().Add(-30 * time.Minute) // inside cooldown window
	s := newIconTestScheduler([]string{"a"}, map[string]*store.SubIcon{
		"a": {Name: "a", HasIcon: true, ExpiresAt: past, AboutFetchedAt: &recent},
	})
	if got := s.buildIconRound(); len(got) != 0 {
		t.Errorf("recently user-fetched sub should be skipped, got %v", got)
	}
}

func TestBuildIconRound_OldUserActivityDoesNotSkip(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	longAgo := time.Now().Add(-2 * iconCheckInterval) // outside cooldown
	s := newIconTestScheduler([]string{"a"}, map[string]*store.SubIcon{
		"a": {Name: "a", HasIcon: true, ExpiresAt: past, AboutFetchedAt: &longAgo},
	})
	if got := s.buildIconRound(); len(got) != 1 {
		t.Errorf("old user fetch should not skip, got %v", got)
	}
}

func TestBuildIconRound_CooldownBoundary(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour)
	// `inside` is 1s younger than the threshold → should be skipped.
	// `outside` is older than the threshold by a safe margin → should be included.
	inside := time.Now().Add(-iconCheckInterval + time.Second)
	outside := time.Now().Add(-iconCheckInterval - time.Minute)
	s := newIconTestScheduler([]string{"in", "out"}, map[string]*store.SubIcon{
		"in":  {Name: "in", HasIcon: true, ExpiresAt: past, AboutFetchedAt: &inside},
		"out": {Name: "out", HasIcon: true, ExpiresAt: past, AboutFetchedAt: &outside},
	})
	got := s.buildIconRound()
	if len(got) != 1 || got[0] != "out" {
		t.Errorf("boundary: expected only [out], got %v", got)
	}
}

func TestBuildIconRound_IconStoreGetErrorFailsOpen(t *testing.T) {
	s := &Scheduler{
		subStatus: &mockSubStatus{alive: []string{"a"}},
		iconStore: &mockIconStore{
			icons:    map[string]*store.SubIcon{},
			getErrOn: map[string]error{"a": errors.New("db down")},
		},
		Events: NewEventLog(50),
	}
	got := s.buildIconRound()
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("expected fail-open inclusion on Get error, got %v", got)
	}
}

func TestBuildIconRound_SubStatusErrorFallsBack(t *testing.T) {
	s := &Scheduler{
		subStatus: &mockSubStatus{err: errors.New("sql err")},
		settings:  &mockSettings{data: map[string]string{"prefetch_subs": "sub:foo"}},
		iconStore: &mockIconStore{icons: map[string]*store.SubIcon{}},
		Events:    NewEventLog(50),
	}
	got := s.buildIconRound()
	if len(got) != 1 || got[0] != "foo" {
		t.Errorf("expected fallback to activeSubs=[foo], got %v", got)
	}
}

func TestBuildIconRound_MixedStates(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	recent := time.Now().Add(-10 * time.Minute)
	s := newIconTestScheduler(
		[]string{"fresh", "expired", "noicon", "userrecent", "missing"},
		map[string]*store.SubIcon{
			"fresh":      {Name: "fresh", HasIcon: true, ExpiresAt: future},
			"expired":    {Name: "expired", HasIcon: true, ExpiresAt: past},
			"noicon":     {Name: "noicon", HasIcon: false, ExpiresAt: past},
			"userrecent": {Name: "userrecent", HasIcon: true, ExpiresAt: past, AboutFetchedAt: &recent},
			// "missing" has no row → should be a candidate
		},
	)
	got := s.buildIconRound()
	want := map[string]bool{"expired": true, "missing": true}
	if len(got) != len(want) {
		t.Fatalf("mixed: got %v, want only %v", got, want)
	}
	for _, sub := range got {
		if !want[sub] {
			t.Errorf("unexpected sub %q in round (full=%v)", sub, got)
		}
	}
}

// ---------------------------------------------------------------------------
// nextIconBatch
// ---------------------------------------------------------------------------

func TestNextIconBatch_BatchSizeWithinBounds(t *testing.T) {
	// Seed a very large round and verify every drawn batch stays in [1, 4]
	// across many draws. The final partial draw (when round shrinks below
	// iconMaxPerCycle) may be < iconMaxPerCycle but is still ≥ 1 because
	// nextIconBatch returns nil instead of a zero-length batch.
	s := &Scheduler{Events: NewEventLog(50)}
	const size = 200
	s.iconRound = makeNames(size)

	drawn := 0
	for {
		batch := s.nextIconBatch()
		if batch == nil {
			break
		}
		if len(batch) < iconMinPerCycle {
			t.Errorf("batch size %d < min %d", len(batch), iconMinPerCycle)
		}
		if len(batch) > iconMaxPerCycle {
			t.Errorf("batch size %d > max %d", len(batch), iconMaxPerCycle)
		}
		drawn += len(batch)
	}
	if drawn != size {
		t.Errorf("expected %d total drawn, got %d", size, drawn)
	}
}

func TestNextIconBatch_CoversRoundOnce_NoRepeatsBeforeRebuild(t *testing.T) {
	alive := makeNames(20)
	s := newIconTestScheduler(alive, nil)
	seen := make(map[string]int)
	// Drain exactly one round: keep pulling until we've covered len(alive)
	// unique subs. Verify nothing appears twice during that span.
	for len(seen) < len(alive) {
		batch := s.nextIconBatch()
		if batch == nil {
			t.Fatalf("ran out of subs mid-round; seen=%v", seen)
		}
		for _, sub := range batch {
			seen[sub]++
			if seen[sub] > 1 {
				t.Errorf("sub %q appeared %d times in one round", sub, seen[sub])
			}
		}
	}
	if len(seen) != len(alive) {
		t.Errorf("round coverage incomplete: seen %d, want %d", len(seen), len(alive))
	}
}

func TestNextIconBatch_RebuildsAfterDrain(t *testing.T) {
	alive := []string{"only"}
	s := newIconTestScheduler(alive, nil)

	first := s.nextIconBatch()
	if len(first) != 1 || first[0] != "only" {
		t.Fatalf("first batch want [only], got %v", first)
	}

	// Round is now empty; the next call should rebuild and re-include "only".
	second := s.nextIconBatch()
	if len(second) != 1 || second[0] != "only" {
		t.Errorf("rebuild should return [only], got %v", second)
	}
}

func TestNextIconBatch_NilWhenNothingEligible(t *testing.T) {
	s := newIconTestScheduler(nil, nil)
	if got := s.nextIconBatch(); got != nil {
		t.Errorf("expected nil batch when alive=empty, got %v", got)
	}
}

func TestNextIconBatch_NilWhenAllIneligible(t *testing.T) {
	future := time.Now().Add(time.Hour)
	s := newIconTestScheduler([]string{"a", "b"}, map[string]*store.SubIcon{
		"a": {Name: "a", HasIcon: true, ExpiresAt: future},
		"b": {Name: "b", HasIcon: false, ExpiresAt: future},
	})
	if got := s.nextIconBatch(); got != nil {
		t.Errorf("expected nil when no eligible subs, got %v", got)
	}
}

func TestNextIconBatch_BatchSizesAreNotAllIdentical(t *testing.T) {
	// Statistical sanity: with iconMinPerCycle=1 and iconMaxPerCycle=4 we
	// expect a spread of sizes across many draws. A flat distribution would
	// produce ~25% of each size; we require strictly more than one distinct
	// size observed. A round large enough to fit many draws even at max=4.
	s := &Scheduler{Events: NewEventLog(50)}
	const size = 1000
	s.iconRound = makeNames(size)
	sizes := map[int]int{}
	for {
		batch := s.nextIconBatch()
		if batch == nil {
			break
		}
		sizes[len(batch)]++
	}
	if len(sizes) < 2 {
		t.Errorf("expected at least 2 distinct batch sizes across many draws, got %v", sizes)
	}
	for sz := range sizes {
		if sz < iconMinPerCycle || sz > iconMaxPerCycle {
			t.Errorf("unexpected batch size %d (allowed [%d, %d])", sz, iconMinPerCycle, iconMaxPerCycle)
		}
	}
}

// ---------------------------------------------------------------------------
// Concurrency: hourly tick + passive trigger draining the same queue
// ---------------------------------------------------------------------------

func TestNextIconBatch_ConcurrentDrainsHaveNoDuplicates(t *testing.T) {
	// Two goroutines (simulating the 1h tick and a /archive passive trigger)
	// race on the same round queue. The mutex must ensure every sub is
	// returned at most once across all concurrent draws within one round.
	const size = 500
	s := &Scheduler{Events: NewEventLog(50)}
	s.iconRound = makeNames(size)

	var mu sync.Mutex
	seen := make(map[string]int)
	var wg sync.WaitGroup
	const workers = 8
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				s.iconMu.Lock()
				empty := len(s.iconRound) == 0
				s.iconMu.Unlock()
				if empty {
					return
				}
				batch := s.nextIconBatch()
				if batch == nil {
					return
				}
				mu.Lock()
				for _, sub := range batch {
					seen[sub]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != size {
		t.Errorf("expected exactly %d unique subs drawn, got %d", size, len(seen))
	}
	for sub, n := range seen {
		if n != 1 {
			t.Errorf("sub %q appeared %d times across concurrent drain, want 1", sub, n)
		}
	}
}

// Compile-time check: previewList format does not panic on weird inputs.
func TestPreviewList_HandlesSpecialCharacters(t *testing.T) {
	// Spaces and bracket characters in sub names should round-trip through
	// fmt.Sprintf("%v", ...) without crashing. Reddit sub names cannot contain
	// these in practice but we guard against future format-string drift.
	in := []string{"a b", "[bracket]", strings.Repeat("x", 100)}
	if got := previewList(in); got == "" {
		t.Error("previewList returned empty on special-char input")
	}
}
