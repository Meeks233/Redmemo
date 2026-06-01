package media

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
)

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
}

func NewEvictor(cfg config.MediaConfig, mediaStore *store.MediaIndexStore) *Evictor {
	return &Evictor{
		cfg:        cfg,
		mediaStore: mediaStore,
		rootPath:   cfg.RootPath,
	}
}

func (e *Evictor) Start(ctx context.Context) {
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
// file_size crossing the target) and the surviving content rows are dropped to
// the -1 absence sentinel in a single batched UPDATE.
func (e *Evictor) RunOnce() (freedBytes int64, evictedCount int, err error) {
	usedBytes, err := e.DiskUsage()
	if err != nil {
		return 0, 0, err
	}

	maxBytes := int64(e.cfg.MaxSizeGB) * 1024 * 1024 * 1024
	if maxBytes <= 0 || usedBytes < maxBytes {
		return 0, 0, nil
	}

	targetFree := int64(float64(maxBytes) * evictionFreeFraction)
	if targetFree <= 0 {
		return 0, 0, nil
	}

	candidates, err := e.mediaStore.SelectEvictionBatch(targetFree, evictionCandidateCap)
	if err != nil {
		return 0, 0, err
	}
	if len(candidates) == 0 {
		return 0, 0, nil
	}

	hashes := make([]string, 0, len(candidates))
	for _, meta := range candidates {
		if meta.FilePath == nil {
			continue
		}
		if err := os.Remove(*meta.FilePath); err != nil && !os.IsNotExist(err) {
			log.Printf("eviction: remove %s: %v", *meta.FilePath, err)
			continue
		}
		hashes = append(hashes, meta.Hash)
		freedBytes += meta.FileSize
		evictedCount++
	}

	if len(hashes) > 0 {
		if err := e.mediaStore.BatchMarkEvicted(hashes); err != nil {
			log.Printf("eviction: batch mark evicted: %v", err)
		}
	}
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
