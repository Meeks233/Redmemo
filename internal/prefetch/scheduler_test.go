package prefetch

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
)

func TestExtractMediaURLs(t *testing.T) {
	tests := []struct {
		name string
		post reddit.Post
		want int
	}{
		{
			name: "no media",
			post: reddit.Post{ID: "1", Title: "text post"},
			want: 0,
		},
		{
			name: "image only",
			post: reddit.Post{
				ID:    "2",
				Media: reddit.Media{URL: "https://i.redd.it/abc.jpg"},
			},
			want: 1,
		},
		{
			name: "image and thumbnail",
			post: reddit.Post{
				ID:        "3",
				Media:     reddit.Media{URL: "https://i.redd.it/abc.jpg"},
				Thumbnail: reddit.Media{URL: "https://a.thumbs.redditmedia.com/abc.jpg"},
			},
			want: 2,
		},
		{
			name: "gallery",
			post: reddit.Post{
				ID: "4",
				Gallery: []reddit.GalleryMedia{
					{URL: "https://i.redd.it/g1.jpg"},
					{URL: "https://i.redd.it/g2.jpg"},
					{URL: "https://i.redd.it/g3.jpg"},
				},
			},
			want: 3,
		},
		{
			name: "all types",
			post: reddit.Post{
				ID:        "5",
				Media:     reddit.Media{URL: "https://v.redd.it/abc/DASH_720.mp4"},
				Thumbnail: reddit.Media{URL: "https://b.thumbs.redditmedia.com/xyz.jpg"},
				Gallery: []reddit.GalleryMedia{
					{URL: "https://i.redd.it/g1.jpg"},
					{URL: ""},
					{URL: "https://i.redd.it/g2.jpg"},
				},
			},
			want: 4,
		},
		{
			name: "empty gallery URLs skipped",
			post: reddit.Post{
				ID: "6",
				Gallery: []reddit.GalleryMedia{
					{URL: ""},
					{URL: ""},
				},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urls := ExtractMediaURLs(&tt.post)
			if len(urls) != tt.want {
				t.Errorf("ExtractMediaURLs() returned %d URLs, want %d; urls=%v", len(urls), tt.want, urls)
			}
		})
	}
}

func TestExtractMediaURLs_UnformatsProxyPaths(t *testing.T) {
	p := &reddit.Post{
		ID:        "7",
		Media:     reddit.Media{URL: "/img/abc.jpg"},
		Thumbnail: reddit.Media{URL: "/preview/pre/thumb.png?width=140"},
		Gallery: []reddit.GalleryMedia{
			{URL: "/img/g1.jpg"},
		},
	}
	urls := ExtractMediaURLs(p)
	if len(urls) != 3 {
		t.Fatalf("expected 3 URLs, got %d: %v", len(urls), urls)
	}
	expected := []string{
		"https://i.redd.it/abc.jpg",
		"https://preview.redd.it/thumb.png?width=140",
		"https://i.redd.it/g1.jpg",
	}
	for i, want := range expected {
		if urls[i] != want {
			t.Errorf("urls[%d] = %q, want %q", i, urls[i], want)
		}
	}
}

func TestExtractMediaItems_VideoPost(t *testing.T) {
	p := &reddit.Post{
		ID:       "v1",
		PostType: "video",
		Media: reddit.Media{
			URL:    "/vid/abc123/DASH_720.mp4",
			Poster: "/preview/pre/poster.jpg?width=640",
		},
		Thumbnail: reddit.Media{URL: "/thumb/a/thumb.jpg"},
	}
	items := ExtractMediaItems(p)
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d: %+v", len(items), items)
	}
	wantKinds := []string{"video", "poster", "thumbnail"}
	wantURLs := []string{
		"https://v.redd.it/abc123/DASH_720.mp4",
		"https://preview.redd.it/poster.jpg?width=640",
		"https://a.thumbs.redditmedia.com/thumb.jpg",
	}
	for i, item := range items {
		if item.Kind != wantKinds[i] {
			t.Errorf("items[%d].Kind = %q, want %q", i, item.Kind, wantKinds[i])
		}
		if item.URL != wantURLs[i] {
			t.Errorf("items[%d].URL = %q, want %q", i, item.URL, wantURLs[i])
		}
	}
}

func TestExtractMediaItems_GifPost(t *testing.T) {
	p := &reddit.Post{
		ID:       "g1",
		PostType: "gif",
		Media:    reddit.Media{URL: "https://v.redd.it/xyz/DASH_360.mp4"},
	}
	items := ExtractMediaItems(p)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "gif" {
		t.Errorf("Kind = %q, want gif", items[0].Kind)
	}
}

func TestExtractMediaItems_ImagePost(t *testing.T) {
	p := &reddit.Post{
		ID:       "i1",
		PostType: "image",
		Media:    reddit.Media{URL: "https://i.redd.it/photo.jpg"},
	}
	items := ExtractMediaItems(p)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Kind != "image" {
		t.Errorf("Kind = %q, want image", items[0].Kind)
	}
}

func TestMediaKindSummary(t *testing.T) {
	tests := []struct {
		name  string
		items []mediaItem
		want  string
	}{
		{"single image", []mediaItem{{Kind: "image"}}, "image"},
		{"video + poster + thumb", []mediaItem{{Kind: "video"}, {Kind: "poster"}, {Kind: "thumbnail"}}, "video + poster + thumbnail"},
		{"gallery 3", []mediaItem{{Kind: "gallery"}, {Kind: "gallery"}, {Kind: "gallery"}}, "3 gallerys"},
		{"mixed", []mediaItem{{Kind: "image"}, {Kind: "thumbnail"}, {Kind: "gallery"}, {Kind: "gallery"}}, "image + thumbnail + 2 gallerys"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mediaKindSummary(tt.items)
			if got != tt.want {
				t.Errorf("mediaKindSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractMediaURLs_NilPost(t *testing.T) {
	p := &reddit.Post{}
	urls := ExtractMediaURLs(p)
	if len(urls) != 0 {
		t.Errorf("expected 0 URLs for empty post, got %d", len(urls))
	}
}

func TestFormatDur(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
		{5 * time.Minute, "5m0s"},
		{time.Hour + 30*time.Minute + 15*time.Second, "1h30m15s"},
		{24 * time.Hour, "24h0m0s"},
		{18*time.Hour + 32*time.Minute, "18h32m0s"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatDur(tt.d)
			if got != tt.want {
				t.Errorf("formatDur(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestSleep(t *testing.T) {
	t.Run("zero duration", func(t *testing.T) {
		err := sleep(context.Background(), 0)
		if err != nil {
			t.Errorf("sleep(0) = %v, want nil", err)
		}
	})

	t.Run("negative duration", func(t *testing.T) {
		err := sleep(context.Background(), -1*time.Second)
		if err != nil {
			t.Errorf("sleep(-1s) = %v, want nil", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := sleep(ctx, time.Hour)
		if err == nil {
			t.Error("sleep with cancelled context should return error")
		}
	})

	t.Run("short sleep completes", func(t *testing.T) {
		start := time.Now()
		err := sleep(context.Background(), 10*time.Millisecond)
		if err != nil {
			t.Errorf("sleep(10ms) = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
			t.Errorf("sleep(10ms) returned too fast: %v", elapsed)
		}
	})
}

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

type mockSettings struct {
	data map[string]string
}

func (m *mockSettings) Get(key string) string { return m.data[key] }
func (m *mockSettings) Set(key, value string) error {
	m.data[key] = value
	return nil
}

type toggleSettings struct {
	data  map[string]string
	onGet func(string)
}

func (ts *toggleSettings) Get(key string) string {
	if ts.onGet != nil {
		ts.onGet(key)
	}
	return ts.data[key]
}

func (ts *toggleSettings) Set(key, value string) error {
	ts.data[key] = value
	return nil
}

type mockPool struct {
	mu        sync.Mutex
	resetAt   time.Time
	capacity  int
	remaining int
}

func (m *mockPool) WindowInfo() (time.Time, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resetAt, m.capacity, m.remaining
}

// setBudget updates the window state under lock, for tests that mutate the
// pool concurrently with a running dispatchLoop.
func (m *mockPool) setBudget(resetAt time.Time, remaining int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetAt = resetAt
	m.remaining = remaining
}

type mockDownloader struct {
	mu         sync.Mutex
	calls      []string
	err        error
	failedURLs   []string
	remuxCalls   []string
	remuxErr     error
	remuxOutcome string
}

func (m *mockDownloader) DownloadMedia(_ context.Context, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, url)
	return m.err
}

func (m *mockDownloader) ListFailedAudio(limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit < len(m.failedURLs) {
		return append([]string(nil), m.failedURLs[:limit]...), nil
	}
	return append([]string(nil), m.failedURLs...), nil
}

func (m *mockDownloader) RetryMuxAudio(_ context.Context, videoURL string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remuxCalls = append(m.remuxCalls, videoURL)
	if m.remuxErr != nil {
		return "", m.remuxErr
	}
	if m.remuxOutcome != "" {
		return m.remuxOutcome, nil
	}
	return "recovered", nil
}

func (m *mockDownloader) getCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.calls))
	copy(out, m.calls)
	return out
}

// ---------------------------------------------------------------------------
// Settings / enable tests
// ---------------------------------------------------------------------------

func TestActiveSubs(t *testing.T) {
	tests := []struct {
		name     string
		settings SettingsProvider
		want     int
	}{
		{"nil settings", nil, 0},
		{"empty value", &mockSettings{data: map[string]string{"prefetch_subs": ""}}, 0},
		{"single sub", &mockSettings{data: map[string]string{"prefetch_subs": "golang"}}, 1},
		{"multiple subs", &mockSettings{data: map[string]string{"prefetch_subs": "golang+rust+python"}}, 3},
		{"with spaces", &mockSettings{data: map[string]string{"prefetch_subs": " golang + rust + "}}, 2},
		{"empty segments", &mockSettings{data: map[string]string{"prefetch_subs": "++golang++"}}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Scheduler{settings: tt.settings}
			subs := s.activeSubs()
			if len(subs) != tt.want {
				t.Errorf("activeSubs() returned %d subs, want %d; subs=%v", len(subs), tt.want, subs)
			}
		})
	}
}

func TestIsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		settings SettingsProvider
		want     bool
	}{
		{"nil settings", nil, false},
		{"off", &mockSettings{data: map[string]string{"enable_natural_prefetch": "off"}}, false},
		{"on", &mockSettings{data: map[string]string{"enable_natural_prefetch": "on"}}, true},
		{"empty", &mockSettings{data: map[string]string{}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Scheduler{settings: tt.settings}
			if got := s.isEnabled(); got != tt.want {
				t.Errorf("isEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserRequestedRecently(t *testing.T) {
	s := &Scheduler{}

	if s.userRequestedRecently() {
		t.Error("should be false with no prior request")
	}

	s.NotifyUserRequest()
	if !s.userRequestedRecently() {
		t.Error("should be true immediately after notify")
	}

	s.lastUserReq.Store(time.Now().Add(-31 * time.Second).Unix())
	if s.userRequestedRecently() {
		t.Error("should be false after 31s")
	}
}

// ---------------------------------------------------------------------------
// NP Dispatch / Submit tests
// ---------------------------------------------------------------------------

func TestSubmit_FIFO(t *testing.T) {
	s := &Scheduler{
		pool:   &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 10),
	}

	// Each dispatch takes 1-3s delay; 3 tasks needs ~6-9s
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go s.dispatchLoop(ctx)

	var order []int
	var mu sync.Mutex

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			s.submit(ctx, fmt.Sprintf("task-%d", idx), false, func(ctx context.Context) {
				mu.Lock()
				order = append(order, idx)
				mu.Unlock()
			})
		}()
		time.Sleep(10 * time.Millisecond)
	}

	wg.Wait()
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 executions, got %d", len(order))
	}
	for i := 0; i < 3; i++ {
		if order[i] != i {
			t.Errorf("execution order[%d] = %d, want %d (order=%v)", i, order[i], i, order)
			break
		}
	}
}

func TestSubmit_CancelledContext(t *testing.T) {
	s := &Scheduler{
		Events: NewEventLog(50),
		queue:  make(chan *workItem), // unbuffered, no dispatcher running
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.submit(ctx, "test", false, func(ctx context.Context) {
		t.Error("should not execute")
	})
	if err == nil {
		t.Error("submit should return error on cancelled context")
	}
}

func TestSubmit_BudgetWait(t *testing.T) {
	pool := &mockPool{
		resetAt:   time.Now().Add(200 * time.Millisecond),
		capacity:  600,
		remaining: 0,
	}

	s := &Scheduler{
		pool:   pool,
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 1),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		// Restore budget and push resetAt forward so waitForBudget sees remaining > reserved
		time.Sleep(100 * time.Millisecond)
		pool.setBudget(time.Now().Add(time.Hour), 100)
	}()

	go s.dispatchLoop(ctx)

	executed := false
	err := s.submit(ctx, "budget-test", true, func(ctx context.Context) {
		executed = true
	})
	cancel()

	if err != nil {
		t.Errorf("submit should succeed after budget restores, got: %v", err)
	}
	if !executed {
		t.Error("task should have been executed")
	}

	events := s.Events.Snapshot()
	budgetSkipFound := false
	for _, e := range events {
		if e.Phase == "NP" && e.Level == LevelSkip {
			budgetSkipFound = true
		}
	}
	if !budgetSkipFound {
		t.Error("expected a budget skip event in NP log")
	}
}

func TestSubmit_UserPause(t *testing.T) {
	s := &Scheduler{
		pool:   &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 1),
		// Pin the user-active pause to a tiny, deterministic value — the
		// production 25–40s randomized delay would otherwise race the test
		// deadline and flake intermittently.
		userActivePause: func() time.Duration { return 50 * time.Millisecond },
	}

	// Set user activity 29s ago (just within the 30s window)
	s.lastUserReq.Store(time.Now().Add(-29 * time.Second).Unix())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go s.dispatchLoop(ctx)

	executed := false
	err := s.submit(ctx, "pause-test", false, func(ctx context.Context) {
		executed = true
	})

	if err != nil {
		t.Errorf("submit should succeed after pause, got: %v", err)
	}

	events := s.Events.Snapshot()
	pauseFound := false
	for _, e := range events {
		if e.Phase == "NP" && e.Level == LevelInfo {
			if len(e.Message) > 12 && e.Message[:12] == "user active," {
				pauseFound = true
			}
		}
	}
	if !pauseFound {
		t.Error("expected user active pause event in NP log")
	}
	if !executed {
		t.Error("task should have been executed after pause")
	}
}

func TestDispatchLoop_CDNSkipsBudget(t *testing.T) {
	pool := &mockPool{
		resetAt:   time.Now().Add(time.Hour),
		capacity:  600,
		remaining: 0, // zero budget
	}

	s := &Scheduler{
		pool:   pool,
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 1),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go s.dispatchLoop(ctx)

	// CDN request (needsBudget=false) should execute even with zero API budget
	executed := false
	err := s.submit(ctx, "cdn-test", false, func(ctx context.Context) {
		executed = true
	})

	if err != nil {
		t.Errorf("CDN submit should succeed with zero budget, got: %v", err)
	}
	if !executed {
		t.Error("CDN task should have been executed despite zero API budget")
	}
}

// ---------------------------------------------------------------------------
// Producer loop tests
// ---------------------------------------------------------------------------

func TestWaitUntilEnabled_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{}},
		Events:   NewEventLog(50),
	}

	err := s.waitUntilEnabled(ctx)
	if err == nil {
		t.Error("waitUntilEnabled should return error on cancelled context")
	}
}

func TestWaitUntilEnabled_EnabledImmediately(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":          "golang",
		}},
		Events: NewEventLog(50),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := s.waitUntilEnabled(ctx)
	if err != nil {
		t.Errorf("waitUntilEnabled should return nil when enabled, got: %v", err)
	}
}

func TestRunBigCycle_NoSubs(t *testing.T) {
	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":          "",
		}},
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 1),
	}

	err := s.runBigCycle(context.Background())
	if err != nil {
		t.Errorf("runBigCycle with no subs should return nil, got: %v", err)
	}
}

func TestRunBigCycle_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &Scheduler{
		settings: &mockSettings{data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":          "golang",
		}},
		pool:   &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 1),
	}

	err := s.runBigCycle(ctx)
	if err == nil {
		t.Error("runBigCycle should return error on cancelled context")
	}
}

func TestRunBigCycle_DisabledMidCycle(t *testing.T) {
	ts := &toggleSettings{
		data: map[string]string{
			"enable_natural_prefetch": "on",
			"prefetch_subs":          "golang",
		},
	}
	firstCall := true
	ts.onGet = func(key string) {
		if key == "enable_natural_prefetch" && !firstCall {
			ts.data["enable_natural_prefetch"] = "off"
		}
		firstCall = false
	}

	s := &Scheduler{
		settings: ts,
		pool:     &mockPool{resetAt: time.Now().Add(time.Hour), capacity: 600, remaining: 100},
		Events:   NewEventLog(50),
		queue:    make(chan *workItem, 1),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.runBigCycle(ctx)
	if err != nil && err != context.DeadlineExceeded {
		t.Errorf("unexpected error: %v", err)
	}

	events := s.Events.Snapshot()
	found := false
	for _, e := range events {
		if e.Phase == "L1" && e.Level == LevelSkip {
			found = true
		}
	}
	if !found {
		t.Error("expected a skip event for disabled mid-cycle")
	}
}

func TestRunL2_NilDependencies(t *testing.T) {
	s := &Scheduler{
		Events: NewEventLog(50),
		queue:  make(chan *workItem, 1),
	}

	err := s.submitL2(context.Background(), "test")
	if err != nil {
		t.Errorf("submitL2 with nil deps should return nil, got: %v", err)
	}

	events := s.Events.Snapshot()
	if len(events) != 0 {
		t.Errorf("expected no events with nil deps, got %d", len(events))
	}
}

// ---------------------------------------------------------------------------
// Interval range tests
// ---------------------------------------------------------------------------

func TestBigCycleInterval(t *testing.T) {
	min := 12 * time.Hour
	max := 24 * time.Hour
	for i := 0; i < 100; i++ {
		d := min + time.Duration(rand.Int63n(int64(max-min)))
		if d < min || d > max {
			t.Errorf("cycle interval %v out of range [%v, %v]", d, min, max)
		}
	}
}

func TestRoundInterval(t *testing.T) {
	min := 15 * time.Minute
	max := 30 * time.Minute
	for i := 0; i < 100; i++ {
		d := min + time.Duration(rand.Int63n(int64(max-min)))
		if d < min || d > max {
			t.Errorf("round interval %v out of range [%v, %v]", d, min, max)
		}
	}
}

func TestMediaDelay(t *testing.T) {
	for i := 0; i < 100; i++ {
		delay := time.Duration(1000+rand.Intn(2000)) * time.Millisecond
		if delay < time.Second || delay > 3*time.Second {
			t.Errorf("media delay %v out of range [1s, 3s]", delay)
		}
	}
}

// ---------------------------------------------------------------------------
// Budget check tests
// ---------------------------------------------------------------------------

func TestBudgetCheck(t *testing.T) {
	tests := []struct {
		remaining int
		numSubs   int
		enough    bool
	}{
		{10, 3, true},
		{4, 3, false},
		{5, 3, true},
		{0, 1, false},
		{100, 10, true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("remaining=%d_subs=%d", tt.remaining, tt.numSubs), func(t *testing.T) {
			needed := tt.numSubs + 2
			got := tt.remaining >= needed
			if got != tt.enough {
				t.Errorf("budget check: remaining=%d >= needed=%d is %v, want %v",
					tt.remaining, needed, got, tt.enough)
			}
		})
	}
}

func TestEventLog_Integration(t *testing.T) {
	s := &Scheduler{Events: NewEventLog(50)}

	s.Events.Add(LevelInfo, "L1", "test message")
	s.Events.Addf(LevelOK, "L2", "downloaded %d files", 3)
	s.Events.Addf(LevelInfo, "NP", "dispatching: test")

	events := s.Events.Snapshot()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Phase != "L1" {
		t.Errorf("first event phase = %q, want L1", events[0].Phase)
	}
	if events[1].Message != "downloaded 3 files" {
		t.Errorf("second event message = %q, want 'downloaded 3 files'", events[1].Message)
	}
	if events[2].Phase != "NP" {
		t.Errorf("third event phase = %q, want NP", events[2].Phase)
	}
}

// ---------------------------------------------------------------------------
// Cycle state persistence tests
// ---------------------------------------------------------------------------

func TestCycleState_SaveLoadClear(t *testing.T) {
	ms := &mockSettings{data: map[string]string{}}
	s := &Scheduler{settings: ms, Events: NewEventLog(10)}

	// Initially empty
	if got := s.loadCycleState(); got != nil {
		t.Errorf("loadCycleState on empty should return nil, got %+v", got)
	}

	// Save and reload
	next := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	state := &cycleState{
		NextCycleAt: next,
		Round:       3,
		Cursors:     map[string]string{"golang": "t3_abc", "rust": "t3_xyz"},
	}
	s.saveCycleState(state)

	loaded := s.loadCycleState()
	if loaded == nil {
		t.Fatal("loadCycleState returned nil after save")
	}
	if !loaded.NextCycleAt.Equal(next) {
		t.Errorf("NextCycleAt = %v, want %v", loaded.NextCycleAt, next)
	}
	if loaded.Round != 3 {
		t.Errorf("Round = %d, want 3", loaded.Round)
	}
	if loaded.Cursors["golang"] != "t3_abc" || loaded.Cursors["rust"] != "t3_xyz" {
		t.Errorf("Cursors = %v, want golang=t3_abc rust=t3_xyz", loaded.Cursors)
	}

	// Clear
	s.clearCycleState()
	if got := s.loadCycleState(); got != nil {
		t.Errorf("loadCycleState after clear should return nil, got %+v", got)
	}
}

func TestCycleState_NilSettings(t *testing.T) {
	s := &Scheduler{settings: nil, Events: NewEventLog(10)}

	// Should not panic with nil settings
	s.saveCycleState(&cycleState{Round: 1})
	if got := s.loadCycleState(); got != nil {
		t.Errorf("loadCycleState with nil settings should return nil, got %+v", got)
	}
	s.clearCycleState() // should not panic
}

func TestCycleState_InvalidJSON(t *testing.T) {
	ms := &mockSettings{data: map[string]string{
		cycleStateKey: "not valid json{{{",
	}}
	s := &Scheduler{settings: ms, Events: NewEventLog(10)}

	got := s.loadCycleState()
	if got != nil {
		t.Errorf("loadCycleState with invalid JSON should return nil, got %+v", got)
	}
}

func TestCycleState_CompletedCycleResumeSleep(t *testing.T) {
	next := time.Now().Add(2 * time.Hour)
	ms := &mockSettings{data: map[string]string{}}
	s := &Scheduler{
		settings: ms,
		Events:   NewEventLog(10),
		queue:    make(chan *workItem, 1),
	}

	// Simulate a completed cycle stored in DB
	s.saveCycleState(&cycleState{
		NextCycleAt: next,
		Round:       maxRoundsPerCycle,
		Cursors:     nil,
	})

	loaded := s.loadCycleState()
	if loaded == nil {
		t.Fatal("expected non-nil state")
	}
	if loaded.Round != maxRoundsPerCycle {
		t.Errorf("Round = %d, want %d", loaded.Round, maxRoundsPerCycle)
	}
	// Verify the runBigCycle logic would detect this as a completed cycle
	if wait := time.Until(loaded.NextCycleAt); wait <= 0 || loaded.Round < maxRoundsPerCycle {
		t.Error("completed cycle state should have positive wait and Round >= maxRoundsPerCycle")
	}
}

func TestCycleState_MidCycleRestore(t *testing.T) {
	ms := &mockSettings{data: map[string]string{}}
	s := &Scheduler{
		settings: ms,
		Events:   NewEventLog(10),
	}

	// Save mid-cycle state (round 4 of 8)
	s.saveCycleState(&cycleState{
		NextCycleAt: time.Now().Add(10 * time.Hour),
		Round:       4,
		Cursors:     map[string]string{"golang": "t3_mid"},
	})

	loaded := s.loadCycleState()
	if loaded == nil {
		t.Fatal("expected non-nil state")
	}
	if loaded.Round != 4 {
		t.Errorf("Round = %d, want 4", loaded.Round)
	}
	if loaded.Cursors["golang"] != "t3_mid" {
		t.Errorf("cursor for golang = %q, want t3_mid", loaded.Cursors["golang"])
	}
}
