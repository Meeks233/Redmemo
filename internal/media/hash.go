package media

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

func HashURL(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:])
}

func HashToPath(rootPath, hash string) string {
	return filepath.Join(rootPath, hash[:2], hash)
}

func NginxPath(hash string) string {
	return "/media/" + hash[:2] + "/" + hash
}
