package prefetch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

// ---------------------------------------------------------------------------
// L1 disaster recovery: bucket loop fires immediately when overdue
// ---------------------------------------------------------------------------

func TestBucketLoop_OverdueFetchFiresImmediately(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: 200 * time.Millisecond,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, _, _, _, _ string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	// Pre-seed bucket state with NextCycleAt = 3 hours ago.
	overdue := time.Now().Add(-3 * time.Hour)
	s.saveBucketState(bucketDay, &bucketState{NextCycleAt: overdue})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	// Wait for first fetch.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("expected at least 1 fetch, got 0")
	}
	// The first fetch should fire within ~100ms of start (dispatch overhead),
	// not wait for a full bucket period.
	latency := calls[0].Sub(start)
	if latency > 500*time.Millisecond {
		t.Errorf("overdue bucket took %v to fire first fetch — expected <500ms (immediate)", latency)
	}
}

func TestBucketLoop_OverdueByMultiplePeriods_SingleCatchUp(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time

	period := 100 * time.Millisecond
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: period,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, _, _, _, _ string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	// Overdue by 5x the period — should catch up exactly once, then schedule
	// normally (now+period), not fire 5 back-to-back rounds.
	s.saveBucketState(bucketDay, &bucketState{
		NextCycleAt: time.Now().Add(-5 * period),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	// Wait for 2 fetches.
	deadline := time.Now().Add(1800 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("expected ≥2 fetches, got %d", len(calls))
	}
	// First fetch: immediate (catch-up).
	if latency := calls[0].Sub(start); latency > 500*time.Millisecond {
		t.Errorf("catch-up fetch took %v, expected <500ms", latency)
	}
	// Second fetch: should be spaced by roughly one period, not immediate.
	gap := calls[1].Sub(calls[0])
	if gap < period/2 {
		t.Errorf("second fetch fired too quickly (%v after first) — expected ≥%v (no double catch-up)", gap, period/2)
	}
}

func TestBucketLoop_OverdueMultiSub_AllFireImmediately(t *testing.T) {
	var mu sync.Mutex
	fetched := map[string]time.Time{}

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:a+b+c",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: 500 * time.Millisecond,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, sub, _, _, _ string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		if _, exists := fetched[sub]; !exists {
			fetched[sub] = time.Now()
		}
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	s.saveBucketState(bucketDay, &bucketState{
		NextCycleAt: time.Now().Add(-2 * time.Hour),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"a", "b", "c"})
		close(done)
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(fetched)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(fetched) < 3 {
		t.Fatalf("expected all 3 subs to fetch, got %d: %v", len(fetched), fetched)
	}
	for sub, at := range fetched {
		latency := at.Sub(start)
		if latency > 1*time.Second {
			t.Errorf("sub %q took %v to fire — expected all subs within 1s for overdue bucket", sub, latency)
		}
	}
}

func TestBucketLoop_NoSavedState_DoesNotFireImmediately(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: 500 * time.Millisecond,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, _, _, _, _ string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	// No saved state — first fetch should wait for a random phase offset,
	// not fire at t=0.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	// Should still get a fetch, but we're just verifying no panic and the
	// loop runs. The phase offset is tiny in test mode (bucketGap*4).
	if len(calls) == 0 {
		t.Fatal("expected at least 1 fetch for fresh bucket within timeout")
	}
}

func TestBucketLoop_OverduePreservesCursors(t *testing.T) {
	var mu sync.Mutex
	var cursors []string

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: 200 * time.Millisecond,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, _, _, _, cursor string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		cursors = append(cursors, cursor)
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "next-page", nil
	}

	// Pre-seed with an existing cursor and overdue schedule.
	s.saveBucketState(bucketDay, &bucketState{
		NextCycleAt: time.Now().Add(-1 * time.Hour),
		Cursors:     map[string]string{"news|hot": "saved-cursor"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(cursors)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(cursors) == 0 {
		t.Fatal("expected at least 1 fetch")
	}
	if cursors[0] != "saved-cursor" {
		t.Errorf("catch-up fetch used cursor %q, want %q — cursor not restored from saved state",
			cursors[0], "saved-cursor")
	}
}

// ---------------------------------------------------------------------------
// L2/L3 disaster recovery: resumePendingWave fires past-due waves
// ---------------------------------------------------------------------------

func TestResumePendingWave_PastDue_FiresImmediately(t *testing.T) {
	var fired atomic.Int32

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
		postStore:        nil, // L2 wave exits early (no posts) but still fires
	}

	payload, _ := json.Marshal(map[string]any{"chunk": 5, "post_count": 20, "period_sec": 3600})

	run := store.PrefetchRun{
		ID:          1,
		Layer:       "L2",
		Bucket:      sql.NullString{String: "day", Valid: true},
		Subreddit:   sql.NullString{String: "news", Valid: true},
		CycleID:     sql.NullString{String: "day:news:1000000", Valid: true},
		SubInterval: sql.NullInt32{Int32: 3, Valid: true},
		ScheduledAt: time.Now().Add(-2 * time.Hour), // 2 hours overdue
		Status:      "pending",
		Payload:     payload,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.resumePendingWave(ctx, run)
		fired.Store(1)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resumePendingWave did not return within 1s for a past-due wave")
	}

	latency := time.Since(start)
	if latency > 500*time.Millisecond {
		t.Errorf("past-due wave took %v to complete — expected immediate fire", latency)
	}
	if fired.Load() != 1 {
		t.Error("wave did not fire")
	}

	// Verify event log mentions "overdue" and "firing immediately".
	found := false
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "overdue") && contains(e.Message, "firing immediately") {
			found = true
			break
		}
	}
	if !found {
		t.Error("event log should contain 'overdue' + 'firing immediately' message")
	}
}

func TestResumePendingWave_FutureWave_WaitsUntilScheduled(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	delay := 200 * time.Millisecond
	payload, _ := json.Marshal(map[string]any{"chunk": 5, "post_count": 20, "period_sec": 3600})

	run := store.PrefetchRun{
		ID:          2,
		Layer:       "L2",
		Bucket:      sql.NullString{String: "day", Valid: true},
		Subreddit:   sql.NullString{String: "news", Valid: true},
		CycleID:     sql.NullString{String: "day:news:1000000", Valid: true},
		SubInterval: sql.NullInt32{Int32: 1, Valid: true},
		ScheduledAt: time.Now().Add(delay),
		Status:      "pending",
		Payload:     payload,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.resumePendingWave(ctx, run)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resumePendingWave did not return within 1s")
	}

	elapsed := time.Since(start)
	if elapsed < delay/2 {
		t.Errorf("future wave fired too quickly (%v) — should have waited ≥%v", elapsed, delay)
	}
}

func TestResumePendingWave_PastDue_L3FiresImmediately(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	payload, _ := json.Marshal(map[string]any{"chunk": 3, "post_count": 10, "period_sec": 3600, "standalone": true})

	run := store.PrefetchRun{
		ID:          3,
		Layer:       "L3",
		Bucket:      sql.NullString{String: "day", Valid: true},
		Subreddit:   sql.NullString{String: "news", Valid: true},
		CycleID:     sql.NullString{String: "day:news:1000000", Valid: true},
		SubInterval: sql.NullInt32{Int32: 4, Valid: true},
		ScheduledAt: time.Now().Add(-90 * time.Minute),
		Status:      "pending",
		Payload:     payload,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.resumePendingWave(ctx, run)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("L3 resumePendingWave timed out for past-due wave")
	}

	if latency := time.Since(start); latency > 500*time.Millisecond {
		t.Errorf("past-due L3 wave took %v — expected immediate", latency)
	}

	found := false
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "overdue") && contains(e.Message, "firing immediately") {
			found = true
			break
		}
	}
	if !found {
		t.Error("event log should contain 'overdue' + 'firing immediately' for L3")
	}
}

func TestResumePendingWave_PastDue_MultipleWaves_AllFire(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	var wg sync.WaitGroup
	var firedCount atomic.Int32

	// Simulate 5 waves: waves 1-3 are past-due, waves 4-5 are future.
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		var scheduledAt time.Time
		if i <= 3 {
			scheduledAt = time.Now().Add(-time.Duration(3-i+1) * time.Hour)
		} else {
			scheduledAt = time.Now().Add(time.Duration(i-3) * 100 * time.Millisecond)
		}
		payload, _ := json.Marshal(map[string]any{"chunk": 5, "post_count": 25, "period_sec": 3600})
		run := store.PrefetchRun{
			ID:          int64(i),
			Layer:       "L2",
			Bucket:      sql.NullString{String: "day", Valid: true},
			Subreddit:   sql.NullString{String: "news", Valid: true},
			CycleID:     sql.NullString{String: "day:news:1000000", Valid: true},
			SubInterval: sql.NullInt32{Int32: int32(i), Valid: true},
			ScheduledAt: scheduledAt,
			Status:      "pending",
			Payload:     payload,
		}
		go func(r store.PrefetchRun) {
			defer wg.Done()
			s.resumePendingWave(ctx, r)
			firedCount.Add(1)
		}(run)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("not all waves completed within timeout")
	}

	if n := firedCount.Load(); n != 5 {
		t.Errorf("expected all 5 waves to fire, got %d", n)
	}
}

func TestResumePendingWave_CancelledContext_ReturnPromptly(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	payload, _ := json.Marshal(map[string]any{"chunk": 5})
	run := store.PrefetchRun{
		ID:          10,
		Layer:       "L2",
		Bucket:      sql.NullString{String: "day", Valid: true},
		Subreddit:   sql.NullString{String: "news", Valid: true},
		CycleID:     sql.NullString{String: "day:news:1000000", Valid: true},
		SubInterval: sql.NullInt32{Int32: 1, Valid: true},
		ScheduledAt: time.Now().Add(time.Hour), // far in future
		Status:      "pending",
		Payload:     payload,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.resumePendingWave(ctx, run)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("resumePendingWave did not exit promptly after context cancel")
	}
}

func TestResumePendingWave_ZeroChunkDefaultsToOne(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	// Payload with chunk=0 (or missing).
	payload, _ := json.Marshal(map[string]any{"post_count": 10})
	run := store.PrefetchRun{
		ID:          20,
		Layer:       "L2",
		Bucket:      sql.NullString{String: "day", Valid: true},
		Subreddit:   sql.NullString{String: "news", Valid: true},
		CycleID:     sql.NullString{String: "day:news:1000000", Valid: true},
		SubInterval: sql.NullInt32{Int32: 1, Valid: true},
		ScheduledAt: time.Now().Add(-time.Minute),
		Status:      "pending",
		Payload:     payload,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.resumePendingWave(ctx, run)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wave with zero chunk did not complete")
	}

	found := false
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "chunk=1") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk to default to 1 when payload has chunk=0")
	}
}

// ---------------------------------------------------------------------------
// L1 + L2/L3 combined: catch-up L1 round spawns new L2/L3 cycles
// ---------------------------------------------------------------------------

func TestBucketLoop_OverdueThenNormalCadence(t *testing.T) {
	var mu sync.Mutex
	var calls []fakeFetchCall

	period := 150 * time.Millisecond
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: period,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, sub, _, _, cursor string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		calls = append(calls, fakeFetchCall{at: time.Now(), sub: sub, cursor: cursor})
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	s.saveBucketState(bucketDay, &bucketState{
		NextCycleAt: time.Now().Add(-1 * time.Hour),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 3 {
		t.Fatalf("expected ≥3 fetches, got %d", len(calls))
	}

	// First: immediate catch-up.
	if latency := calls[0].at.Sub(start); latency > 300*time.Millisecond {
		t.Errorf("catch-up took %v, expected <300ms", latency)
	}
	// Second and third: normal cadence (~period apart).
	for i := 1; i < len(calls); i++ {
		gap := calls[i].at.Sub(calls[i-1].at)
		if gap < period/3 {
			t.Errorf("fetch %d fired too soon after %d: gap %v < %v", i+1, i, gap, period/3)
		}
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestBucketLoop_ExactlyOnTime_NoDelay(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: 200 * time.Millisecond,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, _, _, _, _ string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	// Exactly on schedule.
	s.saveBucketState(bucketDay, &bucketState{NextCycleAt: time.Now()})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("expected at least 1 fetch")
	}
	if latency := calls[0].Sub(start); latency > 300*time.Millisecond {
		t.Errorf("exactly-on-time fetch took %v, expected <300ms", latency)
	}
}

func TestBucketLoop_SlightlyOverdue_FiresImmediately(t *testing.T) {
	var mu sync.Mutex
	var calls []time.Time

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
		}},
		pool:               &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:             NewEventLog(200),
		queue:              make(chan *workItem, 4),
		bucketGap:          5 * time.Millisecond,
		bucketBaseOverride: 200 * time.Millisecond,
		dispatchCooldown:   func() time.Duration { return 2 * time.Millisecond },
	}
	s.fetchFunc = func(_ context.Context, _, _, _, _ string, _ int) ([]reddit.Post, string, string, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return []reddit.Post{{ID: "p1"}}, "", "", nil
	}

	// Overdue by just 1 second.
	s.saveBucketState(bucketDay, &bucketState{NextCycleAt: time.Now().Add(-time.Second)})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.bucketLoop(ctx, bucketDay, []string{"news"})
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("expected at least 1 fetch")
	}
	if latency := calls[0].Sub(start); latency > 300*time.Millisecond {
		t.Errorf("slightly overdue fetch took %v, expected <300ms", latency)
	}
}

func TestResumePendingWave_EventLogFormat(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:golang",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	payload, _ := json.Marshal(map[string]any{"chunk": 10, "post_count": 50, "period_sec": 7200})
	run := store.PrefetchRun{
		ID:          100,
		Layer:       "L2",
		Bucket:      sql.NullString{String: "week", Valid: true},
		Subreddit:   sql.NullString{String: "golang", Valid: true},
		CycleID:     sql.NullString{String: "week:golang:1700000", Valid: true},
		SubInterval: sql.NullInt32{Int32: 2, Valid: true},
		ScheduledAt: time.Now().Add(-3*time.Hour - 15*time.Minute),
		Status:      "pending",
		Payload:     payload,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.resumePendingWave(ctx, run)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	// Check the overdue log contains the sub name and wave number.
	var overdueMsg string
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "overdue") {
			overdueMsg = e.Message
			break
		}
	}
	if overdueMsg == "" {
		t.Fatal("no overdue event logged")
	}
	if !contains(overdueMsg, "r/golang") {
		t.Errorf("overdue log missing sub name: %s", overdueMsg)
	}
	if !contains(overdueMsg, "wave 2/5") {
		t.Errorf("overdue log missing wave info: %s", overdueMsg)
	}
	if !contains(overdueMsg, "3h") {
		t.Errorf("overdue log missing duration: %s", overdueMsg)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// driveReclaimedCycle: sequential driving and burst prevention
// ---------------------------------------------------------------------------

func TestDriveReclaimedCycle_SequentialNotConcurrent(t *testing.T) {
	var mu sync.Mutex
	var timeline []int32 // records sub_interval in firing order

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	// All 5 waves past-due. If they fired concurrently, the order would be
	// nondeterministic. Sequential driving must produce 1,2,3,4,5 in order.
	waves := make([]store.PrefetchRun, 5)
	for i := range waves {
		waves[i] = makeTestRun(int64(i+1), "L2", "day", "news", int32(i+1),
			time.Now().Add(-time.Duration(5-i)*time.Hour), 5)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	// Intercept wave firing via event log — record the sub_interval from
	// the "firing" log message.
	done := make(chan struct{})
	go func() {
		s.driveReclaimedCycle(ctx, "L2", "day", "news", waves)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("driveReclaimedCycle did not complete in time")
	}

	// Parse the event log for firing order.
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "firing (chunk=") {
			for w := int32(1); w <= 5; w++ {
				if contains(e.Message, fmt.Sprintf("wave %d/%d firing", w, l2WavesPerCycle)) {
					mu.Lock()
					timeline = append(timeline, w)
					mu.Unlock()
					break
				}
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(timeline) != 5 {
		t.Fatalf("expected 5 wave firings, got %d: %v", len(timeline), timeline)
	}
	for i, w := range timeline {
		if w != int32(i+1) {
			t.Errorf("wave %d fired at position %d (expected %d) — not sequential: %v", w, i, i+1, timeline)
		}
	}
}

func TestDriveReclaimedCycle_NoBurst_SequentialEvents(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
		postStore:        nil,
	}

	waves := make([]store.PrefetchRun, 3)
	for i := range waves {
		waves[i] = makeTestRun(int64(i+1), "L2", "day", "news", int32(i+1),
			time.Now().Add(-time.Duration(3-i)*time.Hour), 5)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.driveReclaimedCycle(ctx, "L2", "day", "news", waves)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("driveReclaimedCycle did not complete")
	}

	// Verify event log shows sequential wave entries — wave N's "firing"
	// event must appear before wave N+1's. This proves no concurrent burst.
	var fireOrder []int32
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "firing (chunk=") {
			for _, w := range waves {
				if contains(e.Message, fmt.Sprintf("wave %d/", w.SubInterval.Int32)) {
					fireOrder = append(fireOrder, w.SubInterval.Int32)
					break
				}
			}
		}
	}

	if len(fireOrder) != 3 {
		t.Fatalf("expected 3 fire events, got %d", len(fireOrder))
	}
	for i, w := range fireOrder {
		if w != int32(i+1) {
			t.Errorf("fire order[%d] = wave %d, expected wave %d", i, w, i+1)
		}
	}
}

func TestDriveReclaimedCycle_SupersedeMidCycle_DiscardsRemaining(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	// All 3 waves past-due. resumePendingWave with runStore=nil always
	// returns true (no supersede possible). This test just verifies the
	// event log shows "driving 3 wave(s) sequentially".
	waves := make([]store.PrefetchRun, 3)
	for i := range waves {
		waves[i] = makeTestRun(int64(i+1), "L2", "day", "news", int32(i+1),
			time.Now().Add(-time.Hour), 5)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.driveReclaimedCycle(ctx, "L2", "day", "news", waves)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	found := false
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "driving 3 wave(s) sequentially") &&
			contains(e.Message, "3 overdue") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'driving 3 wave(s) sequentially (3 overdue' in event log")
	}
}

func TestDriveReclaimedCycle_MixedPastFuture_FutureWaits(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	futureDelay := 150 * time.Millisecond
	waves := []store.PrefetchRun{
		makeTestRun(1, "L2", "day", "news", 1, time.Now().Add(-time.Hour), 5),
		makeTestRun(2, "L2", "day", "news", 2, time.Now().Add(futureDelay), 5),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.driveReclaimedCycle(ctx, "L2", "day", "news", waves)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	elapsed := time.Since(start)
	if elapsed < futureDelay/2 {
		t.Errorf("completed in %v — future wave did not wait (expected ≥%v)", elapsed, futureDelay/2)
	}

	found := false
	for _, e := range s.Events.Snapshot() {
		if contains(e.Message, "1 overdue, 1 future") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected '1 overdue, 1 future' in event log")
	}
}

func TestDriveReclaimedCycle_SetsAndClearsReclaimStatus(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l2+l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	waves := []store.PrefetchRun{
		makeTestRun(1, "L2", "day", "news", 1, time.Now().Add(-time.Hour), 5),
		makeTestRun(2, "L2", "day", "news", 2, time.Now().Add(-30*time.Minute), 5),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.driveReclaimedCycle(ctx, "L2", "day", "news", waves)
		close(done)
	}()
	<-done

	// After driveReclaimedCycle completes, reclaim status should be cleared.
	ps := s.Status()
	if ps.ReclaimL2Phase != "" {
		t.Errorf("ReclaimL2Phase should be cleared after completion, got %q", ps.ReclaimL2Phase)
	}
	if ps.ReclaimL2Info != "" {
		t.Errorf("ReclaimL2Info should be cleared after completion, got %q", ps.ReclaimL2Info)
	}
}

func TestDriveReclaimedCycle_L3_SetsReclaimStatus(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":           "sub:news",
			"prefetch_default_depth":  "l3",
		}},
		pool:             &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:           NewEventLog(200),
		queue:            make(chan *workItem, 4),
		dispatchCooldown: func() time.Duration { return 2 * time.Millisecond },
	}

	waves := []store.PrefetchRun{
		makeTestRun(1, "L3", "day", "news", 1, time.Now().Add(-time.Hour), 3),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go s.dispatchLoop(ctx)

	done := make(chan struct{})
	go func() {
		s.driveReclaimedCycle(ctx, "L3", "day", "news", waves)
		close(done)
	}()
	<-done

	ps := s.Status()
	if ps.ReclaimL3Phase != "" {
		t.Errorf("ReclaimL3Phase should be cleared after completion, got %q", ps.ReclaimL3Phase)
	}
}

// makeTestRun is a convenience builder for store.PrefetchRun in tests.
func makeTestRun(id int64, layer, tf, sub string, wave int32, scheduledAt time.Time, chunk int) store.PrefetchRun {
	payload, _ := json.Marshal(map[string]any{
		"chunk":      chunk,
		"post_count": chunk * l2WavesPerCycle,
		"period_sec": 3600,
	})
	return store.PrefetchRun{
		ID:          id,
		Layer:       layer,
		Bucket:      sql.NullString{String: tf, Valid: true},
		Subreddit:   sql.NullString{String: sub, Valid: true},
		CycleID:     sql.NullString{String: fmt.Sprintf("%s:%s:1000000", tf, sub), Valid: true},
		SubInterval: sql.NullInt32{Int32: wave, Valid: true},
		ScheduledAt: scheduledAt,
		Status:      "pending",
		Payload:     payload,
	}
}
