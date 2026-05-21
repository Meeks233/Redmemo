package media

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHashToPath(t *testing.T) {
	hash := "3a8f2bc4e7deadbeef"
	root := filepath.FromSlash("/data/media")
	got := HashToPath(root, hash)
	want := filepath.Join(root, "3a", hash)
	if got != want {
		t.Errorf("HashToPath = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, hash) {
		t.Errorf("path should end with full hash, got %q", got)
	}
}

func TestNginxPath(t *testing.T) {
	hash := "3a8f2bc4e7deadbeef"
	got := NginxPath(hash)
	want := "/media/3a/3a8f2bc4e7deadbeef"
	if got != want {
		t.Errorf("NginxPath(%q) = %q, want %q", hash, got, want)
	}
}
