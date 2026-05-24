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
	// Fail fast when miniredis is closed mid-test (redis-down cases): no
	// retries and short timeouts keep those tests quick.
	client := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		MaxRetries:  -1,
		DialTimeout: 200 * time.Millisecond,
		ReadTimeout: 200 * time.Millisecond,
	})
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

func TestAdmit_RedisDownFailsClosed(t *testing.T) {
	m, mr := newTestManager(t)
	mr.Close() // simulate Redis going down

	ok, reason := m.Admit(context.Background())
	if ok || reason != "hr_redis_down" {
		t.Fatalf("redis down: ok=%v reason=%q, want false/hr_redis_down", ok, reason)
	}
}

func TestAdmit_RedisDownExponentialBackoff(t *testing.T) {
	m, mr := newTestManager(t)
	mr.Close()

	now := time.Unix(1000, 0)
	m.SetClock(func() time.Time { return now })
	ctx := context.Background()

	// First failure: backoff starts at the minimum.
	if ok, _ := m.Admit(ctx); ok {
		t.Fatal("want blocked on first failure")
	}
	if m.backoff != redisBackoffMin {
		t.Fatalf("backoff=%v, want %v", m.backoff, redisBackoffMin)
	}

	// Within the backoff window: blocked without re-probing, backoff steady.
	if ok, _ := m.Admit(ctx); ok {
		t.Fatal("want blocked within backoff window")
	}
	if m.backoff != redisBackoffMin {
		t.Fatalf("backoff grew within window: %v", m.backoff)
	}

	// Past the probe time: the next failed probe doubles the backoff.
	now = now.Add(redisBackoffMin + time.Millisecond)
	if ok, _ := m.Admit(ctx); ok {
		t.Fatal("want blocked after probe window")
	}
	if m.backoff != 2*redisBackoffMin {
		t.Fatalf("backoff=%v, want %v", m.backoff, 2*redisBackoffMin)
	}
}

func TestAdmit_RedisDownBackoffCapped(t *testing.T) {
	m, mr := newTestManager(t)
	mr.Close()

	now := time.Unix(1000, 0)
	m.SetClock(func() time.Time { return now })
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if ok, _ := m.Admit(ctx); ok {
			t.Fatal("want blocked while redis down")
		}
		now = now.Add(m.backoff + time.Millisecond)
		if m.backoff > redisBackoffMax {
			t.Fatalf("backoff exceeded cap: %v > %v", m.backoff, redisBackoffMax)
		}
	}
	if m.backoff != redisBackoffMax {
		t.Fatalf("backoff did not settle at cap: %v", m.backoff)
	}
}

func TestAdmit_RecoversWhenRedisBack(t *testing.T) {
	m, _ := newTestManager(t) // miniredis is up

	// Pretend we were in redis-down backoff with the probe window elapsed.
	m.backoff = redisBackoffMax
	m.nextProbe = m.now().Add(-time.Second)

	ok, reason := m.Admit(context.Background())
	if !ok || reason != "" {
		t.Fatalf("recovery: ok=%v reason=%q, want true/empty", ok, reason)
	}
	if m.backoff != 0 {
		t.Fatalf("backoff not cleared after recovery: %v", m.backoff)
	}
}

func TestRedisDownReset_RecoversOnPing(t *testing.T) {
	m, _ := newTestManager(t) // miniredis is up

	m.backoff = redisBackoffMin
	m.nextProbe = m.now().Add(-time.Second) // probe window elapsed

	down, _ := m.RedisDownReset(context.Background())
	if down {
		t.Fatal("want recovered (down=false) when redis is reachable")
	}
	if m.backoff != 0 {
		t.Fatalf("backoff not cleared: %v", m.backoff)
	}
}

func TestRedisDownReset_StaysDownWithinWindow(t *testing.T) {
	m, mr := newTestManager(t)
	mr.Close()

	now := time.Unix(2000, 0)
	m.SetClock(func() time.Time { return now })
	m.backoff = redisBackoffMin
	m.nextProbe = now.Add(redisBackoffMin)

	down, until := m.RedisDownReset(context.Background())
	if !down {
		t.Fatal("want down within backoff window")
	}
	if until != m.nextProbe.Unix() {
		t.Fatalf("until=%d, want %d", until, m.nextProbe.Unix())
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
