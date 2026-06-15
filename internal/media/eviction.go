package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
)

func humanBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// evictionFreeFraction is the share of the configured cap reclaimed in one
// pass once usage reaches the cap. 0.10 → free 10% of the allowance (e.g. a
// 1 GiB cap reclaims ~102.4 MiB per cycle).
const evictionFreeFraction = 0.10

// evictionCandidateCap bounds how many rows the DB-side selector may return
// per cycle as a safety net against pathological lists; the SQL window-sum
// already stops at the first row that crosses the target.
const evictionCandidateCap = 5000

type Evictor struct {
	cfg        config.MediaConfig
	mediaStore *store.MediaIndexStore
	rootPath   string
	Events     *EventLog
}

func NewEvictor(cfg config.MediaConfig, mediaStore *store.MediaIndexStore) *Evictor {
	return &Evictor{
		cfg:        cfg,
		mediaStore: mediaStore,
		rootPath:   cfg.RootPath,
		Events:     NewEventLog(50),
	}
}

func (e *Evictor) Start(ctx context.Context) {
	e.Events.Addf(LevelInfo, "init", "evictor started, cap %.1f GB, check interval %s",
		e.cfg.MaxSizeGB, e.cfg.EvictionCheckInterval)
	go func() {
		ticker := time.NewTicker(e.cfg.EvictionCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				freed, count, err := e.RunOnce()
				if err != nil {
					log.Printf("eviction: error: %v", err)
				} else if count > 0 {
					log.Printf("eviction: freed %d bytes from %d files", freed, count)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// RunOnce reclaims 10% of the configured cap when usage has reached the cap.
// The selection is done in the database (highest cache-score first, cumulative
// file_size crossing the target); each selected file is removed and its content
// row dropped to the -1 absence sentinel under the row's per-hash publish lock,
// so a concurrent re-download of the same content can never interleave.
func (e *Evictor) RunOnce() (freedBytes int64, evictedCount int, err error) {
	usedBytes, err := e.DiskUsage()
	if err != nil {
		return 0, 0, err
	}

	maxBytes := int64(e.cfg.MaxSizeGB * 1024 * 1024 * 1024)
	if maxBytes <= 0 || usedBytes < maxBytes {
		return 0, 0, nil
	}

	e.Events.Addf(LevelInfo, "evict", "usage %s exceeds cap %s, starting eviction pass",
		humanBytes(usedBytes), humanBytes(maxBytes))

	targetFree := int64(float64(maxBytes) * evictionFreeFraction)
	if targetFree <= 0 {
		return 0, 0, nil
	}

	candidates, err := e.mediaStore.SelectEvictionBatch(targetFree, evictionCandidateCap)
	if err != nil {
		e.Events.Addf(LevelError, "evict", "select batch: %v", err)
		return 0, 0, err
	}
	if len(candidates) == 0 {
		e.Events.Add(LevelSkip, "evict", "no eviction candidates found")
		return 0, 0, nil
	}

	// Reclaim each candidate under its per-hash publish lock (see
	// store.LockHash). Holding the lock across os.Remove + MarkEvicted serializes
	// the reclaim against a concurrent re-download's rename + Save for the same
	// content: paths are content-addressed, so a re-download lands at the exact
	// path we just removed, and without the lock it could re-create and re-point
	// the file between our remove and the row-NULL — stranding a present file
	// behind a file_path=NULL row that eviction can never re-select. The lock
	// closes that download/evict TOCTOU outright (the prior code only narrowed it
	// with a best-effort re-stat). The lock is per-hash and never nested, so the
	// loop can't deadlock; an unlock is paired with every acquire on every path.
	var removeFails, markFails int
	for _, meta := range candidates {
		if meta.FilePath == nil {
			continue
		}
		unlock := e.mediaStore.LockHash(meta.Hash)
		if err := os.Remove(*meta.FilePath); err != nil && !os.IsNotExist(err) {
			unlock()
			log.Printf("eviction: remove %s: %v", *meta.FilePath, err)
			removeFails++
			continue
		}
		if err := e.mediaStore.MarkEvicted(meta.Hash); err != nil {
			unlock()
			log.Printf("eviction: mark evicted %s: %v", meta.Hash, err)
			markFails++
			continue
		}
		unlock()
		freedBytes += meta.FileSize
		evictedCount++
	}

	level := LevelOK
	msg := fmt.Sprintf("freed %s from %d files (target %s)",
		humanBytes(freedBytes), evictedCount, humanBytes(targetFree))
	if removeFails > 0 || markFails > 0 {
		level = LevelWarn
		msg += fmt.Sprintf(", %d remove / %d mark errors", removeFails, markFails)
	}
	e.Events.Add(level, "evict", msg)
	return freedBytes, evictedCount, nil
}

func (e *Evictor) DiskUsage() (int64, error) {
	var total int64
	err := filepath.Walk(e.rootPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// selectByCumulativeSize mirrors the SQL window-sum selector in pure Go: given
// a candidate slice already sorted by eviction priority (highest score first),
// it returns the shortest prefix whose cumulative file_size first reaches
// targetBytes — i.e. the row that crosses the line is included, every later
// row is dropped. Rows with a nil FilePath are skipped (already absent). A
// non-positive target yields an empty slice. Exported via lower-case for the
// package-internal tests; it also acts as a deterministic fallback in case
// the DB path ever needs to be re-run on an in-memory list.
func selectByCumulativeSize(candidates []*store.MediaMeta, targetBytes int64) []*store.MediaMeta {
	if targetBytes <= 0 || len(candidates) == 0 {
		return nil
	}
	var acc int64
	out := make([]*store.MediaMeta, 0, len(candidates))
	for _, m := range candidates {
		if m == nil || m.FilePath == nil {
			continue
		}
		out = append(out, m)
		acc += m.FileSize
		if acc >= targetBytes {
			break
		}
	}
	return out
}

// simulateEviction is the disk-less core of RunOnce, exposed for tests. It
// accepts a sorted candidate list and the cap math directly, applies the
// 10%-free target, runs the in-memory selector, and reports what RunOnce
// would have freed plus the hash list it would have batched into
// BatchMarkEvicted. No filesystem or database calls.
func simulateEviction(candidates []*store.MediaMeta, usedBytes, maxBytes int64) (selected []*store.MediaMeta, hashes []string, freedBytes int64) {
	if maxBytes <= 0 || usedBytes < maxBytes {
		return nil, nil, 0
	}
	target := int64(float64(maxBytes) * evictionFreeFraction)
	selected = selectByCumulativeSize(candidates, target)
	for _, m := range selected {
		hashes = append(hashes, m.Hash)
		freedBytes += m.FileSize
	}
	return selected, hashes, freedBytes
}
