// Package hrlimit implements the HR (Human and Robots) rate-limit layer.
//
// It applies a global, coarse, three-tier tumbling-window cap on RedMemo's
// upstream Reddit traffic. HR-originated requests check the cooldown gate
// before issuing an upstream call; NP-originated requests (background
// prefetch / archive refresh) never check the gate but still contribute to
// the counters. See HR.md for the full design.
package hrlimit

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/redmemo/redmemo/internal/config"
)

// Redis-down backoff bounds. When Redis (the authority for the HR gate) is
// unreachable, HR traffic is blocked and Redis is re-probed on an exponential
// schedule between these bounds until it recovers.
const (
	redisBackoffMin = 1 * time.Second
	redisBackoffMax = 30 * time.Second
)

type tier struct {
	name      string // "l1", "l2", "l3"
	window    time.Duration
	threshold int
}

type Manager struct {
	client  *redis.Client
	tiers   []tier
	enabled bool
	now     func() time.Time // injectable for tests

	// Redis-down backoff state. backoff == 0 means Redis is considered
	// healthy; backoff > 0 means we're failing closed and nextProbe is the
	// earliest time the next Redis probe may run.
	mu        sync.Mutex
	backoff   time.Duration
	nextProbe time.Time
}

// recordScript atomically increments three per-tier bucket counters and, if
// any meets its threshold, sets the corresponding cooldown key with a TTL
// covering the remainder of the current window plus one full next window.
//
// KEYS:  unused (all keys derived from ARGV).
// ARGV:  nowUnix, l1_window, l1_threshold, l2_window, l2_threshold,
// l3_window, l3_threshold.
var recordScript = redis.NewScript(`
local now = tonumber(ARGV[1])
for i = 0, 2 do
    local w = tonumber(ARGV[2 + i*2])
    local t = tonumber(ARGV[3 + i*2])
    local bucket = math.floor(now / w)
    local tier_name = "l" .. (i + 1)
    local counter_key = "hr:" .. tier_name .. ":" .. bucket
    local cur = redis.call("INCR", counter_key)
    if cur == 1 then
        redis.call("EXPIRE", counter_key, w * 2)
    end
    if cur >= t then
        local remaining_current = w - (now % w)
        local cooldown_ttl = remaining_current + w
        redis.call("SET", "hr:cooldown:" .. tier_name, "1", "EX", cooldown_ttl)
    end
end
return 1
`)

// NewManager builds a Manager from config. Returns a disabled no-op Manager
// when cfg.Enabled is false or client is nil.
func NewManager(client *redis.Client, cfg config.HRLimitConfig) *Manager {
	m := &Manager{
		client:  client,
		enabled: cfg.Enabled && client != nil,
		now:     time.Now,
		tiers: []tier{
			{name: "l1", window: cfg.L1Window, threshold: cfg.L1Threshold},
			{name: "l2", window: cfg.L2Window, threshold: cfg.L2Threshold},
			{name: "l3", window: cfg.L3Window, threshold: cfg.L3Threshold},
		},
	}
	return m
}

// SetClock overrides the clock used for bucket computation. Intended for
// tests; callers in production should leave the default time.Now.
func (m *Manager) SetClock(now func() time.Time) {
	if m != nil {
		m.now = now
	}
}

// Admit reports whether an HR-originated request is allowed to proceed to
// the upstream Reddit API. When blocked, reason is "hr_l1", "hr_l2", or
// "hr_l3" (most-severe tier wins: L3 > L2 > L1), or "hr_redis_down" when the
// gate cannot be evaluated because Redis is unreachable.
//
// Redis is the authority for the HR cooldown gate, so when it's down we fail
// closed rather than letting HR traffic flood the upstream ungated. While
// down, requests are blocked and Redis is re-probed on an exponential backoff
// schedule (capped at redisBackoffMax); a single successful probe clears the
// backoff and resumes normal admission.
func (m *Manager) Admit(ctx context.Context) (admitted bool, reason string) {
	if m == nil || !m.enabled {
		return true, ""
	}

	now := m.now()
	m.mu.Lock()
	if m.backoff > 0 && now.Before(m.nextProbe) {
		// Redis is known-down and we're inside the current backoff window:
		// block without probing so we don't hammer a dead Redis.
		m.mu.Unlock()
		return false, "hr_redis_down"
	}
	m.mu.Unlock()

	keys := []string{"hr:cooldown:l1", "hr:cooldown:l2", "hr:cooldown:l3"}
	vals, err := m.client.MGet(ctx, keys...).Result()
	if err != nil {
		m.failClosed(now)
		return false, "hr_redis_down"
	}

	// Probe succeeded: Redis is healthy again, clear any backoff state.
	m.recover()

	// Severity order: L3 > L2 > L1.
	for i := len(vals) - 1; i >= 0; i-- {
		if vals[i] != nil {
			return false, fmt.Sprintf("hr_l%d", i+1)
		}
	}
	return true, ""
}

// failClosed grows the Redis-down backoff (or starts it) and arms the next
// probe time. Caller must not hold m.mu.
func (m *Manager) failClosed(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.backoff == 0 {
		m.backoff = redisBackoffMin
	} else {
		m.backoff *= 2
		if m.backoff > redisBackoffMax {
			m.backoff = redisBackoffMax
		}
	}
	m.nextProbe = now.Add(m.backoff)
}

// recover clears the Redis-down backoff state. Caller must not hold m.mu.
func (m *Manager) recover() {
	m.mu.Lock()
	m.backoff = 0
	m.nextProbe = time.Time{}
	m.mu.Unlock()
}

// RedisDownReset reports whether the HR gate is currently failing closed
// because Redis is unreachable and, if so, the unix timestamp of the next
// probe attempt. When the backoff window has elapsed it actively pings Redis:
// on success it clears the backoff (so callers learn of recovery even without
// an intervening Admit call), on failure it grows the backoff.
//
// The status endpoint polls this so the degrade page keeps showing the banner
// — and counts down to the next probe — instead of bouncing the user back into
// a redirect loop while Redis is down.
func (m *Manager) RedisDownReset(ctx context.Context) (down bool, untilUnix int64) {
	if m == nil || !m.enabled {
		return false, 0
	}

	m.mu.Lock()
	backoff := m.backoff
	nextProbe := m.nextProbe
	m.mu.Unlock()
	if backoff == 0 {
		return false, 0
	}

	now := m.now()
	if now.Before(nextProbe) {
		return true, nextProbe.Unix()
	}

	// Probe window elapsed: ping Redis to detect recovery.
	if err := m.client.Ping(ctx).Err(); err != nil {
		m.failClosed(now)
		m.mu.Lock()
		next := m.nextProbe
		m.mu.Unlock()
		return true, next.Unix()
	}
	m.recover()
	return false, 0
}

// RecordUpstream registers one upstream Reddit request against all three
// tiers. Called by both HR and NP paths after a successful upstream call
// (or any attempt that consumes Reddit quota).
func (m *Manager) RecordUpstream(ctx context.Context) {
	if m == nil || !m.enabled {
		return
	}
	now := m.now().Unix()
	if _, err := recordScript.Run(
		ctx, m.client, nil,
		now,
		int64(m.tiers[0].window.Seconds()), m.tiers[0].threshold,
		int64(m.tiers[1].window.Seconds()), m.tiers[1].threshold,
		int64(m.tiers[2].window.Seconds()), m.tiers[2].threshold,
	).Result(); err != nil {
		// A failed counter increment under-counts upstream traffic, weakening the
		// cap precisely when Redis is unhealthy. Surface it so the failure is
		// observable rather than silent (upstream calls are themselves throttled,
		// so this cannot spam). Admit() fails closed separately on Redis errors.
		log.Printf("hrlimit: RecordUpstream counter increment failed: %v", err)
	}
}

// CooldownReason returns the most-severe active cooldown tier and the unix
// timestamp at which it expires. reason is "" when no cooldown is active.
func (m *Manager) CooldownReason(ctx context.Context) (reason string, untilUnix int64) {
	if m == nil || !m.enabled {
		return "", 0
	}
	for i := len(m.tiers) - 1; i >= 0; i-- {
		key := "hr:cooldown:" + m.tiers[i].name
		ttl, err := m.client.TTL(ctx, key).Result()
		if err != nil || ttl <= 0 {
			continue
		}
		return fmt.Sprintf("hr_l%d", i+1), m.now().Add(ttl).Unix()
	}
	return "", 0
}
