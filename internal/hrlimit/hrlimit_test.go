package hrlimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/redmemo/redmemo/internal/config"
)

func newTestManager(t *testing.T) (*Manager, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	m := NewManager(client, config.HRLimitConfig{
		Enabled:     true,
		L1Window:    5 * time.Second,
		L1Threshold: 5,
		L2Window:    30 * time.Second,
		L2Threshold: 15,
		L3Window:    5 * time.Minute,
		L3Threshold: 50,
	})
	return m, mr
}

func TestAdmit_BaselineAllows(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	ok, reason := m.Admit(ctx)
	if !ok || reason != "" {
		t.Fatalf("baseline: ok=%v reason=%q, want true/empty", ok, reason)
	}
}

func TestRecordUpstream_TripsL1(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		m.RecordUpstream(ctx)
	}
	ok, reason := m.Admit(ctx)
	if ok {
		t.Fatalf("expected admit=false after 5 records, got true")
	}
	if reason != "hr_l1" {
		t.Fatalf("expected reason hr_l1, got %q", reason)
	}
}

func TestRecordUpstream_SeverityL3WinsOverL1(t *testing.T) {
	m, mr := newTestManager(t)
	ctx := context.Background()
	// 50 records crosses all three thresholds; L3 (highest) must win.
	for i := 0; i < 50; i++ {
		m.RecordUpstream(ctx)
	}
	_ = mr
	ok, reason := m.Admit(ctx)
	if ok {
		t.Fatalf("expected admit=false, got true")
	}
	if reason != "hr_l3" {
		t.Fatalf("expected reason hr_l3, got %q", reason)
	}
}

func TestRecordUpstream_L2IndependentOfL1Cooldown(t *testing.T) {
	m, mr := newTestManager(t)
	ctx := context.Background()
	// Trip L1 only (5 records < L2's 15 threshold).
	for i := 0; i < 5; i++ {
		m.RecordUpstream(ctx)
	}
	// L1 cooldown must exist, L2 must not.
	if !mr.Exists("hr:cooldown:l1") {
		t.Error("L1 cooldown should be set")
	}
	if mr.Exists("hr:cooldown:l2") {
		t.Error("L2 cooldown should NOT be set after only 5 records")
	}
	if mr.Exists("hr:cooldown:l3") {
		t.Error("L3 cooldown should NOT be set after only 5 records")
	}
}

func TestCooldown_SpansCurrentPlusNextWindow(t *testing.T) {
	m, mr := newTestManager(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		m.RecordUpstream(ctx)
	}
	ttl := mr.TTL("hr:cooldown:l1")
	// L1 window is 5s; cooldown is remaining_current + next = (0..5] + 5.
	// So TTL must be in (5s, 10s].
	if ttl <= 5*time.Second || ttl > 10*time.Second {
		t.Fatalf("L1 cooldown TTL = %v, want in (5s, 10s]", ttl)
	}
}

func TestCounterResetsOnWindowRollover(t *testing.T) {
	m, mr := newTestManager(t)
	ctx := context.Background()
	fakeNow := time.Unix(1_000_000_000, 0) // aligned: 1e9 % 5 == 0
	m.SetClock(func() time.Time { return fakeNow })

	for i := 0; i < 4; i++ {
		m.RecordUpstream(ctx)
	}
	if mr.Exists("hr:cooldown:l1") {
		t.Fatal("L1 should not be tripped after 4 records")
	}
	// Advance both clocks past one L1 window (5s) so the bucket key changes
	// and the old counter is reachable only by its TTL.
	fakeNow = fakeNow.Add(6 * time.Second)
	mr.FastForward(6 * time.Second)
	for i := 0; i < 4; i++ {
		m.RecordUpstream(ctx)
	}
	if mr.Exists("hr:cooldown:l1") {
		t.Fatal("L1 should still not be tripped: counter reset on rollover")
	}
}

func TestNPStyleIncrementsStillTrip(t *testing.T) {
	// NP path calls RecordUpstream without ever calling Admit. Verify the
	// counter still trips and an HR caller after will be blocked.
	m, _ := newTestManager(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		m.RecordUpstream(ctx) // NP-style: no Admit check
	}
	ok, reason := m.Admit(ctx)
	if ok || reason != "hr_l1" {
		t.Fatalf("HR after NP-tripped L1: ok=%v reason=%q", ok, reason)
	}
}

func TestCooldownReason_NoneActive(t *testing.T) {
	m, _ := newTestManager(t)
	reason, until := m.CooldownReason(context.Background())
	if reason != "" || until != 0 {
		t.Fatalf("idle: got reason=%q until=%d, want empty/0", reason, until)
	}
}

func TestCooldownReason_ReturnsMostSevere(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		m.RecordUpstream(ctx)
	}
	reason, until := m.CooldownReason(ctx)
	if reason != "hr_l2" {
		t.Fatalf("expected hr_l2 (highest active), got %q", reason)
	}
	if until <= time.Now().Unix() {
		t.Fatalf("until=%d should be in future (now=%d)", until, time.Now().Unix())
	}
}

func TestDisabledManagerIsNoOp(t *testing.T) {
	m := NewManager(nil, config.HRLimitConfig{Enabled: false})
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		m.RecordUpstream(ctx)
	}
	ok, reason := m.Admit(ctx)
	if !ok || reason != "" {
		t.Fatalf("disabled manager: ok=%v reason=%q, want true/empty", ok, reason)
	}
}
