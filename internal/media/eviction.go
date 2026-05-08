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

func (e *Evictor) RunOnce() (freedBytes int64, evictedCount int, err error) {
	usedBytes, err := e.DiskUsage()
	if err != nil {
		return 0, 0, err
	}

	maxBytes := int64(e.cfg.MaxSizeGB) * 1024 * 1024 * 1024
	threshold := int64(float64(maxBytes) * e.cfg.EvictionThreshold)
	if usedBytes < threshold {
		return 0, 0, nil
	}

	targetFree := usedBytes - int64(float64(maxBytes)*0.7)

	candidates, err := e.mediaStore.ListEvictionCandidates(100)
	if err != nil {
		return 0, 0, err
	}

	for _, meta := range candidates {
		if freedBytes >= targetFree {
			break
		}

		if meta.FilePath == nil {
			continue
		}

		if err := os.Remove(*meta.FilePath); err != nil && !os.IsNotExist(err) {
			log.Printf("eviction: remove %s: %v", *meta.FilePath, err)
			continue
		}

		if err := e.mediaStore.MarkEvicted(meta.Hash); err != nil {
			log.Printf("eviction: mark evicted %s: %v", meta.Hash, err)
			continue
		}

		freedBytes += meta.FileSize
		evictedCount++
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
