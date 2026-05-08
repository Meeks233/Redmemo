package media

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHashURL_Deterministic(t *testing.T) {
	url := "https://i.redd.it/abc123.jpg"
	h1 := HashURL(url)
	h2 := HashURL(url)
	if h1 != h2 {
		t.Errorf("HashURL not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex hash, got %d chars", len(h1))
	}
}

func TestHashURL_DifferentURLs(t *testing.T) {
	h1 := HashURL("https://i.redd.it/abc.jpg")
	h2 := HashURL("https://i.redd.it/def.jpg")
	if h1 == h2 {
		t.Error("different URLs produced same hash")
	}
}

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
