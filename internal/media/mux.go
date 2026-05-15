package media

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/redmemo/redmemo/internal/store"
)

var (
	ffmpegPathOnce sync.Once
	ffmpegPath     string

	muxableSegment = regexp.MustCompile(`^(?:DASH|CMAF)_\d+\.mp4$`)
)

func findFfmpeg() string {
	ffmpegPathOnce.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = p
		}
	})
	return ffmpegPath
}

// IsMuxableVideoSegment reports whether a v.redd.it path points at a DASH/CMAF
// video-only segment whose audio lives in a sibling DASH_AUDIO_*.mp4 file.
func IsMuxableVideoSegment(path string) bool {
	seg := path
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	if j := strings.Index(seg, "?"); j >= 0 {
		seg = seg[:j]
	}
	return muxableSegment.MatchString(seg)
}

func audioCandidates(videoURL string) []string {
	clean := videoURL
	if i := strings.Index(clean, "?"); i >= 0 {
		clean = clean[:i]
	}
	idx := strings.LastIndex(clean, "/")
	if idx < 0 {
		return nil
	}
	base := clean[:idx+1]
	// Reddit serves audio under both naming schemes depending on encoder
	// generation. Newer uploads (2025+) use CMAF_AUDIO_*; older ones use
	// DASH_AUDIO_*. Probe both — the wrong family returns 403.
	return []string{
		base + "CMAF_AUDIO_128.mp4",
		base + "CMAF_AUDIO_64.mp4",
		base + "DASH_AUDIO_128.mp4",
		base + "DASH_AUDIO_64.mp4",
		base + "DASH_audio.mp4",
		base + "audio",
	}
}

func muxCacheKey(videoURL string) string {
	return "muxed:" + videoURL
}

// ServeMuxed serves a v.redd.it DASH video with its audio track muxed in.
// The verdict is persisted in media_index.audio_state so subsequent requests
// know whether to skip the mux probe entirely.
func (p *Proxy) ServeMuxed(w http.ResponseWriter, r *http.Request, videoURL string) {
	key := muxCacheKey(videoURL)

	meta, _ := p.mediaStore.Resolve(key)
	if meta != nil && meta.FilePath != nil {
		if _, statErr := os.Stat(*meta.FilePath); statErr == nil {
			p.cache.RecordMediaAccess(r.Context(), key)
			p.serve(w, r, meta)
			return
		}
	}

	// Known-silent video with the cached file evicted — re-download the raw
	// video only, no audio probe, no ffmpeg.
	if meta != nil && meta.AudioState != nil && *meta.AudioState == "silent" {
		newMeta, err := p.downloadSilent(r.Context(), videoURL, key)
		if err != nil {
			log.Printf("media: silent refetch failed for %s: %v", videoURL, err)
			p.reverseProxy(w, r, videoURL)
			return
		}
		p.serve(w, r, newMeta)
		return
	}

	newMeta, err := p.downloadMuxed(r.Context(), videoURL, key)
	if err != nil {
		log.Printf("media: mux failed for %s: %v", videoURL, err)
		p.reverseProxy(w, r, videoURL)
		return
	}
	p.serve(w, r, newMeta)
}

// downloadMuxed runs the full mux flow. Only writes audio_state when the
// outcome is conclusive (verified silent, or successful mux). ffmpeg errors,
// missing ffmpeg, and transient network failures all return error WITHOUT
// writing any cache row, so the next request can retry.
func (p *Proxy) downloadMuxed(ctx context.Context, videoURL, key string) (*store.MediaMeta, error) {
	tmpDir, err := os.MkdirTemp("", "redmemo-mux-")
	if err != nil {
		return nil, fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	videoTmp := filepath.Join(tmpDir, "v.mp4")
	if err := p.downloadTo(ctx, videoURL, videoTmp); err != nil {
		return nil, fmt.Errorf("download video: %w", err)
	}

	audioTmp, audioConfirmedAbsent, err := p.probeAudio(ctx, videoURL, tmpDir)
	if err != nil {
		return nil, fmt.Errorf("probe audio: %w", err)
	}

	hash := HashURL(key)
	outPath := HashToPath(p.rootPath, hash)
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	var verdict string
	if audioConfirmedAbsent {
		// Reddit returned 4xx for every audio candidate — this video genuinely
		// has no audio track. Cache the silent video and record the verdict so
		// future requests skip the probe.
		if err := moveOrCopy(videoTmp, outPath); err != nil {
			return nil, fmt.Errorf("save silent video: %w", err)
		}
		verdict = "silent"
	} else {
		// We have an audio file; ffmpeg must be available to mux.
		if findFfmpeg() == "" {
			return nil, fmt.Errorf("ffmpeg not installed; cannot mux audio")
		}
		cmd := exec.CommandContext(ctx, findFfmpeg(),
			"-y", "-loglevel", "error",
			"-i", videoTmp, "-i", audioTmp,
			"-c", "copy",
			"-map", "0:v:0", "-map", "1:a:0",
			"-movflags", "+faststart",
			"-f", "mp4",
			outPath)
		if out, ferr := cmd.CombinedOutput(); ferr != nil {
			os.Remove(outPath)
			return nil, fmt.Errorf("ffmpeg: %w: %s", ferr, strings.TrimSpace(string(out)))
		}
		verdict = "has_audio"
	}

	info, err := os.Stat(outPath)
	if err != nil {
		return nil, fmt.Errorf("stat output: %w", err)
	}

	meta := &store.MediaMeta{
		OriginalURL: key,
		Hash:        hash,
		FilePath:    &outPath,
		MIMEType:    "video/mp4",
		FileSize:    info.Size(),
	}
	if err := p.mediaStore.Save(meta); err != nil {
		return nil, fmt.Errorf("save index: %w", err)
	}
	if err := p.mediaStore.SetAudioState(key, verdict); err != nil {
		log.Printf("media: set audio_state for %s: %v", key, err)
	}
	meta.AudioState = &verdict
	return meta, nil
}

// downloadSilent fetches just the raw video (no audio probe, no ffmpeg) for
// videos we've previously confirmed have no audio track.
func (p *Proxy) downloadSilent(ctx context.Context, videoURL, key string) (*store.MediaMeta, error) {
	hash := HashURL(key)
	outPath := HashToPath(p.rootPath, hash)
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	if err := p.downloadTo(ctx, videoURL, outPath); err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	silent := "silent"
	meta := &store.MediaMeta{
		OriginalURL: key,
		Hash:        hash,
		FilePath:    &outPath,
		MIMEType:    "video/mp4",
		FileSize:    info.Size(),
		AudioState:  &silent,
	}
	if err := p.mediaStore.Save(meta); err != nil {
		return nil, fmt.Errorf("save index: %w", err)
	}
	// audio_state survives the Save (UPDATE doesn't touch it), but re-assert
	// in case this row was just inserted fresh.
	_ = p.mediaStore.SetAudioState(key, "silent")
	return meta, nil
}

// probeAudio tries each candidate audio URL in turn.
//   - Returns (path, false, nil) when a candidate downloads successfully.
//   - Returns ("", true, nil) when every candidate responded with a definitive
//     4xx — the video has no audio track.
//   - Returns ("", false, err) on a transient failure (5xx, network) so the
//     caller can bail without poisoning the cache.
func (p *Proxy) probeAudio(ctx context.Context, videoURL, tmpDir string) (string, bool, error) {
	var lastTransient error
	allDefinitelyMissing := true

	for _, candidate := range audioCandidates(videoURL) {
		dst := filepath.Join(tmpDir, "a.mp4")
		status, err := p.downloadToWithStatus(ctx, candidate, dst)
		if err == nil {
			return dst, false, nil
		}
		if status >= 400 && status < 500 {
			continue // definitively absent, try next candidate
		}
		allDefinitelyMissing = false
		lastTransient = err
	}

	if allDefinitelyMissing {
		return "", true, nil
	}
	return "", false, lastTransient
}

// downloadToWithStatus is like downloadTo but also reports the HTTP status
// code, so the caller can distinguish 4xx (definitively absent) from 5xx /
// network errors (transient).
func (p *Proxy) downloadToWithStatus(ctx context.Context, url, dst string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", p.uaPool.Get())

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return resp.StatusCode, err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func (p *Proxy) downloadTo(ctx context.Context, url, dst string) error {
	_, err := p.downloadToWithStatus(ctx, url, dst)
	return err
}

func moveOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
