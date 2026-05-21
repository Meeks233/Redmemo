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
func publishContent(stagingPath, rootPath string) (hash string, finalPath string, err error) {
	f, err := os.Open(stagingPath)
	if err != nil {
		return "", "", fmt.Errorf("open staging: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		f.Close()
		return "", "", fmt.Errorf("hash staging: %w", err)
	}
	f.Close()

	hash = hex.EncodeToString(hasher.Sum(nil))
	finalPath = HashToPath(rootPath, hash)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return "", "", fmt.Errorf("mkdir shard: %w", err)
	}
	if _, statErr := os.Stat(finalPath); statErr == nil {
		os.Remove(stagingPath)
		return hash, finalPath, nil
	}
	// moveOrCopy tries an atomic rename first and falls back to copy when
	// staging and dest live on different filesystems (mux's tmpDir is in the
	// system temp, which may not share a volume with rootPath). The remove is
	// a no-op when rename consumed the source.
	if err := moveOrCopy(stagingPath, finalPath); err != nil {
		return "", "", fmt.Errorf("publish: %w", err)
	}
	os.Remove(stagingPath)
	return hash, finalPath, nil
}
