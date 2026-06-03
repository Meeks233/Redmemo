package handler

import (
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

// statsSnapshot holds the union of expensive aggregates the /settings and
// /debug pages need. Both pages render synchronously and used to fire 6+
// independent table scans per request; in LAN/bypass deployments the round-trip
// to Postgres is the dominant latency. A short TTL deduplicates bursts (page
// load + the navbar quota-ring poll + any reload) without letting numbers go
// visibly stale.
type statsSnapshot struct {
	postCount    int64
	subCount     int64
	subStats     []store.SubredditStat
	mediaCount   int64
	mediaSize    int64
	distinctSubs []string
	liveSubs     []string
	fetchedAt    time.Time
}

const statsCacheTTL = 5 * time.Second

type statsCache struct {
	mu   sync.Mutex
	snap *statsSnapshot
}

// get returns a still-fresh snapshot or computes a new one. Only one caller
// refreshes at a time; concurrent callers wait on the same mutex and observe
// the just-refreshed result instead of stampeding the database.
func (c *statsCache) get(h *Handler) *statsSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap != nil && time.Since(c.snap.fetchedAt) < statsCacheTTL {
		return c.snap
	}
	s := &statsSnapshot{fetchedAt: time.Now()}
	if h.postStore != nil {
		s.postCount, _ = h.postStore.Count()
		s.subCount, _ = h.postStore.SubredditCount()
		if stats, err := h.postStore.SubredditStats(10, 10); err == nil {
			s.subStats = stats
		}
		s.distinctSubs, _ = h.postStore.DistinctSubreddits()
	}
	if h.mediaStore != nil {
		s.mediaCount, s.mediaSize, _ = h.mediaStore.Stats()
	}
	if h.subStatusStore != nil {
		s.liveSubs, _ = h.subStatusStore.ListLive()
	}
	c.snap = s
	return s
}
