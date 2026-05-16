package media

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/redmemo/redmemo/internal/store"
)

func TestIsMuxableVideoSegment(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"DASH_720.mp4", true},
		{"DASH_1080.mp4", true},
		{"CMAF_480.mp4", true},
		{"CMAF_96.mp4", true},
		{"https://v.redd.it/abc123/DASH_720.mp4", true},
		{"https://v.redd.it/abc123/DASH_1080.mp4?source=fallback", true},
		{"https://v.redd.it/abc123/CMAF_240.mp4?a=1&b=2", true},
		// audio segments are NOT video segments — no bare \d+ after the prefix
		{"DASH_AUDIO_128.mp4", false},
		{"CMAF_AUDIO_64.mp4", false},
		{"DASH_audio.mp4", false},
		// wrong container / playlist
		{"DASH_720.webm", false},
		{"HLSPlaylist.m3u8", false},
		{"DASH_720.mp4.part", false},
		// case sensitive
		{"dash_720.mp4", false},
		// missing prefix / digits
		{"720.mp4", false},
		{"DASH_.mp4", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsMuxableVideoSegment(tc.path); got != tc.want {
			t.Errorf("IsMuxableVideoSegment(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestIsMuxableVRedditURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://v.redd.it/abc123/DASH_720.mp4", true},
		{"https://v.redd.it/abc123/DASH_720.mp4?source=fallback", true},
		// right segment shape but not a v.redd.it host
		{"https://example.com/x/DASH_720.mp4", false},
		// v.redd.it host but not a muxable segment
		{"https://v.redd.it/abc123/HLSPlaylist.m3u8", false},
		{"https://v.redd.it/abc123/DASH_AUDIO_128.mp4", false},
		// plain image
		{"https://i.redd.it/photo.jpg", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isMuxableVRedditURL(tc.url); got != tc.want {
			t.Errorf("isMuxableVRedditURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestAudioCandidates(t *testing.T) {
	got := audioCandidates("https://v.redd.it/abc123/DASH_720.mp4?source=fallback")
	want := []string{
		"https://v.redd.it/abc123/CMAF_AUDIO_128.mp4",
		"https://v.redd.it/abc123/CMAF_AUDIO_64.mp4",
		"https://v.redd.it/abc123/DASH_AUDIO_128.mp4",
		"https://v.redd.it/abc123/DASH_AUDIO_64.mp4",
		"https://v.redd.it/abc123/DASH_audio.mp4",
		"https://v.redd.it/abc123/audio",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAudioCandidates_NoSlash(t *testing.T) {
	if got := audioCandidates("noslashhere"); got != nil {
		t.Errorf("expected nil for a URL with no slash, got %v", got)
	}
}

func TestMuxCacheKey(t *testing.T) {
	url := "https://v.redd.it/abc/DASH_720.mp4"
	key := muxCacheKey(url)
	if key != "muxed:"+url {
		t.Errorf("muxCacheKey = %q, want %q", key, "muxed:"+url)
	}
}

func TestFileOnDisk(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "real.mp4")
	if err := os.WriteFile(existing, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "gone.mp4")

	t.Run("nil meta", func(t *testing.T) {
		if fileOnDisk(nil) {
			t.Error("nil meta should report false")
		}
	})
	t.Run("nil file path", func(t *testing.T) {
		if fileOnDisk(&store.MediaMeta{}) {
			t.Error("meta with nil FilePath should report false")
		}
	})
	t.Run("file present", func(t *testing.T) {
		if !fileOnDisk(&store.MediaMeta{FilePath: &existing}) {
			t.Error("existing file should report true")
		}
	})
	t.Run("file missing", func(t *testing.T) {
		if fileOnDisk(&store.MediaMeta{FilePath: &missing}) {
			t.Error("missing file should report false")
		}
	})
}

func TestMoveOrCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")
	content := []byte("hello mux world")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatal(err)
	}

	if err := moveOrCopy(src, dst); err != nil {
		t.Fatalf("moveOrCopy: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst content = %q, want %q", got, content)
	}
}

func TestMoveOrCopy_MissingSource(t *testing.T) {
	dir := t.TempDir()
	err := moveOrCopy(filepath.Join(dir, "nope.bin"), filepath.Join(dir, "out.bin"))
	if err == nil {
		t.Error("expected an error when the source does not exist")
	}
}

func TestPublishToCache(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "staging.mp4")
	out := filepath.Join(dir, "final.mp4")
	content := []byte("muxed video bytes")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatal(err)
	}

	if err := publishToCache(src, out); err != nil {
		t.Fatalf("publishToCache: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("published content = %q, want %q", got, content)
	}
	// The .part staging file must not survive a successful publish.
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file should be gone after publish, stat err = %v", err)
	}
}
