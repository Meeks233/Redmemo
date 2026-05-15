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
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/redmemo/redmemo/internal/config"
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
}

// recordScript atomically increments three per-tier bucket counters and, if
// any meets its threshold, sets the corresponding cooldown key with a TTL
// covering the remainder of the current window plus one full next window.
//
// KEYS:  unused (all keys derived from ARGV).
// ARGV:  nowUnix, l1_window, l1_threshold, l2_window, l2_threshold,
//        l3_window, l3_threshold.
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
// "hr_l3" (most-severe tier wins: L3 > L2 > L1).
func (m *Manager) Admit(ctx context.Context) (admitted bool, reason string) {
	if m == nil || !m.enabled {
		return true, ""
	}
	keys := []string{"hr:cooldown:l1", "hr:cooldown:l2", "hr:cooldown:l3"}
	vals, err := m.client.MGet(ctx, keys...).Result()
	if err != nil {
		// Fail open: if Redis is down, do not block HR traffic.
		return true, ""
	}
	// Severity order: L3 > L2 > L1.
	for i := len(vals) - 1; i >= 0; i-- {
		if vals[i] != nil {
			return false, fmt.Sprintf("hr_l%d", i+1)
		}
	}
	return true, ""
}

// RecordUpstream registers one upstream Reddit request against all three
// tiers. Called by both HR and NP paths after a successful upstream call
// (or any attempt that consumes Reddit quota).
func (m *Manager) RecordUpstream(ctx context.Context) {
	if m == nil || !m.enabled {
		return
	}
	now := m.now().Unix()
	_, _ = recordScript.Run(
		ctx, m.client, nil,
		now,
		int64(m.tiers[0].window.Seconds()), m.tiers[0].threshold,
		int64(m.tiers[1].window.Seconds()), m.tiers[1].threshold,
		int64(m.tiers[2].window.Seconds()), m.tiers[2].threshold,
	).Result()
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
