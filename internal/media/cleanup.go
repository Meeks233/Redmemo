package media

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// legacySentinel is created inside the media root after a one-shot cleanup so
// subsequent startups know the root has already been reconciled with the
// content-addressed schema (v20+).
const legacySentinel = ".redmemo-content-addressed"

// isShardDirName reports whether a directory name matches the 2-char hex
// sharding both the old URL-hash and the new content-hash schemes use. The
// cleanup wipe only touches names that pass this filter, so a mis-pointed
// rootPath (e.g. someone typing /data instead of /data/media) cannot delete
// anything that isn't a legitimate media shard.
func isShardDirName(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := name[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// WipeLegacyRootIfNeeded removes orphaned files left behind by the pre-v20
// URL-hash media cache. Old files lived at sha256(url) paths; new files live
// at sha256(content) paths in the same directory layout, so they cannot be
// distinguished by name. Per the v20 migration this destructively clears any
// shard subdirectories once and writes a sentinel marker so the wipe never
// repeats. Anything in rootPath that does not match the shard naming
// convention is left untouched, so a misconfigured root_path cannot blow
// away unrelated data.
//
// Idempotent: subsequent calls see the sentinel and return immediately. Safe
// to run before the evictor / proxy / prefetch loops start.
func WipeLegacyRootIfNeeded(rootPath string) error {
	if rootPath == "" {
		return nil
	}
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return fmt.Errorf("ensure media root: %w", err)
	}
	sentinel := filepath.Join(rootPath, legacySentinel)
	if _, err := os.Stat(sentinel); err == nil {
		return nil
	}

	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return fmt.Errorf("scan media root: %w", err)
	}
	var (
		removed int
		kept    int
	)
	for _, e := range entries {
		if e.Name() == legacySentinel {
			continue
		}
		if !e.IsDir() || !isShardDirName(e.Name()) {
			// Not a media shard — leave anything else (config dropins, logs,
			// stray files) alone. The whole point of this guard is that a
			// mis-pointed rootPath (e.g. /data instead of /data/media) sees
			// no shard-shaped entries and wipes nothing.
			kept++
			continue
		}
		p := filepath.Join(rootPath, e.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove legacy %s: %w", p, err)
		}
		removed++
	}

	if err := os.WriteFile(sentinel, []byte("v20\n"), 0644); err != nil {
		return fmt.Errorf("write sentinel: %w", err)
	}
	switch {
	case removed > 0:
		log.Printf("media: wiped %d legacy URL-hash shard dirs from %s (v20 cleanup; %d non-shard entries preserved)", removed, rootPath, kept)
	case kept > 0:
		log.Printf("media: %s has %d non-shard entries and no shard dirs; sentinel installed without wiping anything", rootPath, kept)
	default:
		log.Printf("media: media root %s already clean; sentinel installed", rootPath)
	}
	return nil
}
