package media

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// HashToPath returns the disk path for a hex content hash, sharding by the
// first two hex characters so one directory doesn't grow unbounded.
func HashToPath(rootPath, hash string) string {
	return filepath.Join(rootPath, hash[:2], hash)
}

// NginxPath returns the X-Accel-Redirect path for a hex content hash. Mirrors
// HashToPath's sharding.
func NginxPath(hash string) string {
	return "/media/" + hash[:2] + "/" + hash
}

// publishContent hashes a closed staging file, computes its content-addressed
// final path under rootPath, and atomically moves it there. If identical bytes
// are already cached at that path (another URL fetched them first), the
// staging file is removed instead — disk dedup is structural. Caller must
// have already closed any open writer on stagingPath.
//
// The lock callback (typically (*store.MediaIndexStore).LockHash) is taken on
// the computed hash just before the file lands at its content-addressed home
// and its release is returned to the caller, which MUST hold it across the
// subsequent Save and then call it exactly once (a deferred call is simplest).
// This serializes the publish against the evictor — which takes the same
// per-hash lock around os.Remove + MarkEvicted — so a re-download can never
// re-create a file in the window between the evictor's remove and its row-NULL,
// which would otherwise strand a present file behind a file_path=NULL row.
// On any error the returned release is nil and must not be called.
func publishContent(stagingPath, rootPath string, lock func(string) func()) (hash string, finalPath string, release func(), err error) {
	f, err := os.Open(stagingPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("open staging: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		f.Close()
		return "", "", nil, fmt.Errorf("hash staging: %w", err)
	}
	f.Close()

	hash = hex.EncodeToString(hasher.Sum(nil))
	finalPath = HashToPath(rootPath, hash)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return "", "", nil, fmt.Errorf("mkdir shard: %w", err)
	}

	release = lock(hash)
	if _, statErr := os.Stat(finalPath); statErr == nil {
		os.Remove(stagingPath)
		return hash, finalPath, release, nil
	}
	// moveOrCopy tries an atomic rename first and falls back to copy when
	// staging and dest live on different filesystems (mux's tmpDir is in the
	// system temp, which may not share a volume with rootPath). The remove is
	// a no-op when rename consumed the source.
	if err := moveOrCopy(stagingPath, finalPath); err != nil {
		release()
		return "", "", nil, fmt.Errorf("publish: %w", err)
	}
	os.Remove(stagingPath)
	return hash, finalPath, release, nil
}
