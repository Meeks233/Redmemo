package prefetch

import (
	"sync"
	"testing"
	"time"
)

func TestEventLog_AddAndSnapshot(t *testing.T) {
	l := NewEventLog(5)
	l.Add(LevelInfo, "phase1", "msg1")
	l.Add(LevelWarn, "phase2", "msg2")

	snap := l.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Message != "msg1" || snap[1].Message != "msg2" {
		t.Errorf("order wrong: %q, %q", snap[0].Message, snap[1].Message)
	}
	if snap[0].Level != LevelInfo || snap[1].Level != LevelWarn {
		t.Errorf("levels wrong: %q, %q", snap[0].Level, snap[1].Level)
	}
	if snap[0].Phase != "phase1" {
		t.Errorf("Phase = %q, want phase1", snap[0].Phase)
	}
}

func TestEventLog_RingBufferEviction(t *testing.T) {
	l := NewEventLog(3)
	for i := 1; i <= 5; i++ {
		l.Addf(LevelOK, "p", "msg%d", i)
	}
	snap := l.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3 (capped)", len(snap))
	}
	// Oldest two (msg1, msg2) must have been evicted; newest order preserved.
	want := []string{"msg3", "msg4", "msg5"}
	for i, w := range want {
		if snap[i].Message != w {
			t.Errorf("snap[%d] = %q, want %q", i, snap[i].Message, w)
		}
	}
}

func TestEventLog_CapacityOne(t *testing.T) {
	l := NewEventLog(1)
	l.Add(LevelInfo, "p", "first")
	l.Add(LevelInfo, "p", "second")

	snap := l.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	if snap[0].Message != "second" {
		t.Errorf("kept %q, want most recent (second)", snap[0].Message)
	}
}

func TestEventLog_SnapshotIsIndependentCopy(t *testing.T) {
	l := NewEventLog(3)
	l.Add(LevelInfo, "p", "original")

	snap := l.Snapshot()
	snap[0].Message = "mutated"

	if again := l.Snapshot(); again[0].Message != "original" {
		t.Errorf("mutating a snapshot leaked into the log: %q", again[0].Message)
	}
}

func TestEventLog_Addf(t *testing.T) {
	l := NewEventLog(2)
	l.Addf(LevelError, "fetch", "failed after %d retries: %s", 3, "timeout")
	snap := l.Snapshot()
	if snap[0].Message != "failed after 3 retries: timeout" {
		t.Errorf("Addf message = %q", snap[0].Message)
	}
}

// TestEventLog_Concurrent drives Add and Snapshot from many goroutines. Run
// with -race to verify the RWMutex guards the underlying slice correctly.
func TestEventLog_Concurrent(t *testing.T) {
	l := NewEventLog(100)
	const goroutines = 32
	const iters = 300

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if (g+i)%2 == 0 {
					l.Addf(LevelInfo, "p", "g%d-i%d", g, i)
				} else {
					_ = l.Snapshot()
				}
			}
		}(g)
	}
	wg.Wait()

	if snap := l.Snapshot(); len(snap) != 100 {
		t.Errorf("final Snapshot len = %d, want 100 (capped, log was flooded)", len(snap))
	}
}

func TestEventLog_ZeroCapacity(t *testing.T) {
	// A zero-capacity log must silently discard events, never panic.
	l := NewEventLog(0)
	l.Add(LevelInfo, "p", "dropped")
	l.Addf(LevelWarn, "p", "also %s", "dropped")

	if snap := l.Snapshot(); len(snap) != 0 {
		t.Errorf("zero-capacity Snapshot len = %d, want 0", len(snap))
	}
}

func TestEvent_RelativeTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{5 * time.Second, "5s ago"},
		{45 * time.Second, "45s ago"},
		{90 * time.Second, "1m30s ago"},
		{10 * time.Minute, "10m0s ago"},
		{2*time.Hour + 15*time.Minute, "2h15m ago"},
	}
	for _, c := range cases {
		e := Event{Time: now.Add(-c.ago)}
		if got := e.RelativeTime(); got != c.want {
			t.Errorf("RelativeTime(%v ago) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestEvent_TimeStr(t *testing.T) {
	e := Event{Time: time.Date(2026, 5, 16, 13, 7, 42, 0, time.UTC)}
	if got := e.TimeStr(); got != "2026-05-16 13:07:42 UTC" {
		t.Errorf("TimeStr = %q, want 2026-05-16 13:07:42 UTC", got)
	}
}
