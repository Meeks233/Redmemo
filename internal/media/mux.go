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
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
)

var (
	ffmpegPathOnce sync.Once
	ffmpegPath     string

	muxableSegment = regexp.MustCompile(`^(?:DASH|CMAF)_\d+\.mp4$`)

	muxKeyPrefix = "muxed:"

	// Audio-probe retry budget. Transient CDN failures get audioProbeMaxAttempts
	// tries spaced audioProbeRetryDelay apart before the mux attempt gives up
	// and parks the entry 'failed' for the L5 layer to re-attempt.
	audioProbeRetryDelay = 1 * time.Second

	// muxRetryCooldown throttles re-attempts on a still-broken video so a
	// popular clip doesn't storm ffmpeg. A user view inside this window reuses
	// the emergency silent copy without launching a fresh attempt.
	muxRetryCooldown = 2 * time.Minute

	// muxInflight deduplicates concurrent mux work per video URL. A browser
	// opens several requests for one <video> (preload probe, playback, range
	// seeks), often on separate connections; without dedup each would launch
	// its own ffmpeg writing the same output path, and a request serving that
	// file while a sibling ffmpeg rewrites it ships a half-written, audio-less
	// MP4. Callers all wait on the single shared result instead.
	muxInflightMu sync.Mutex
	muxInflight   = map[string]*muxCall{}
)

type muxCall struct {
	done chan struct{}
	meta *store.MediaMeta
	err  error
}

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

// isMuxableVRedditURL reports whether url points at a v.redd.it DASH/CMAF
// video-only segment that must go through the audio-mux pipeline rather than
// being cached verbatim as a silent file.
func isMuxableVRedditURL(rawURL string) bool {
	return strings.Contains(rawURL, "v.redd.it/") && IsMuxableVideoSegment(rawURL)
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

const (
	// audioProbeMaxAttempts is how many times one mux attempt retries the
	// audio probe before giving up.
	audioProbeMaxAttempts = 3
	// audioAbandonThreshold is the failed-mux-attempt count at which a video
	// moves from 'failed' (L5 keeps retrying) to 'abandoned' (L5 gives up):
	// one initial attempt plus three L5/user retries.
	audioAbandonThreshold = 4
)

func muxCacheKey(videoURL string) string {
	return muxKeyPrefix + videoURL
}

// fileOnDisk reports whether meta points at a cache file that currently exists.
// ServeMuxed and AudioStatus share this gate so a "ready" status can never run
// ahead of a file ServeMuxed would actually serve — e.g. after the evictor has
// reclaimed a muxed file whose row still reads 'has_audio'.
func fileOnDisk(meta *store.MediaMeta) bool {
	if meta == nil || meta.FilePath == nil {
		return false
	}
	_, err := os.Stat(*meta.FilePath)
	return err == nil
}

// ServeMuxed serves a v.redd.it DASH video. When the audio-muxed file is
// already cached it is served directly with a long-lived cache. Otherwise the
// silent video is served immediately with Cache-Control: no-store — so a later
// page reload re-requests and upgrades to audio — while the mux runs in the
// background. The page's audioSync.js surfaces that progress to the viewer.
func (p *Proxy) ServeMuxed(w http.ResponseWriter, r *http.Request, videoURL string) {
	key := muxCacheKey(videoURL)

	meta, _ := p.mediaStore.Resolve(key)
	state := ""
	if meta != nil && meta.AudioState != nil {
		state = *meta.AudioState
	}

	// Conclusive cached result ('has_audio' or 'silent') — serve it as-is.
	if (state == "has_audio" || state == "silent") && fileOnDisk(meta) {
		p.cache.RecordMediaAccess(r.Context(), key)
		p.serve(w, r, meta, false)
		return
	}

	// Audio not ready. Kick the mux in the background and serve the silent
	// video right now so playback is never blocked. no-store lets a reload
	// pick up the muxed copy once it lands.
	p.startBackgroundMux(videoURL, key, meta)

	if fileOnDisk(meta) {
		// Emergency silent copy already on disk ('failed'/'abandoned').
		p.cache.RecordMediaAccess(r.Context(), key)
		p.serve(w, r, meta, true)
		return
	}
	// Nothing cached yet — stream the silent video-only segment live.
	p.reverseProxy(w, r, videoURL, true)
}

// AudioStatus reports the mux state of a v.redd.it video for the audioSync.js
// poller and, when audio is not yet available, kicks the background mux.
// Returns "ready" (audio muxed in), "silent" (no audio track exists), or
// "pending" (mux in progress / queued).
//
// "ready" is only returned once the muxed file is recorded as 'has_audio' AND
// physically on disk — the exact gate ServeMuxed uses to serve it. This keeps
// the viewer's "reload to view" prompt from racing ahead of a servable file
// (e.g. while ffmpeg is still finishing, the audio_state write is mid-flight,
// or the evictor has just reclaimed the file).
func (p *Proxy) AudioStatus(videoURL string) string {
	key := muxCacheKey(videoURL)
	meta, _ := p.mediaStore.Resolve(key)
	state := ""
	if meta != nil && meta.AudioState != nil {
		state = *meta.AudioState
	}
	switch state {
	case "has_audio":
		if fileOnDisk(meta) {
			return "ready"
		}
		// Recorded as muxed but the file is gone (evicted, or the row write
		// landed before the file) — re-mux and keep the viewer waiting.
		p.startBackgroundMux(videoURL, key, meta)
		return "pending"
	case "silent":
		return "silent"
	default:
		p.startBackgroundMux(videoURL, key, meta)
		return "pending"
	}
}

// muxSem caps concurrent background mux jobs so a page full of fresh videos
// doesn't spawn dozens of simultaneous ffmpeg processes.
var muxSem = make(chan struct{}, 4)

// startBackgroundMux runs the mux for videoURL off the request path. It is a
// no-op when a mux for this video is already in flight, or when the row was
// retried within muxRetryCooldown. An 'abandoned' video is first revived so
// the L5 layer re-enrols it with a fresh budget.
func (p *Proxy) startBackgroundMux(videoURL, key string, meta *store.MediaMeta) {
	if meta != nil && meta.LastAudioAttemptAt != nil &&
		time.Since(*meta.LastAudioAttemptAt) < muxRetryCooldown {
		return
	}
	muxInflightMu.Lock()
	_, inflight := muxInflight[key]
	muxInflightMu.Unlock()
	if inflight {
		return
	}
	state := ""
	if meta != nil && meta.AudioState != nil {
		state = *meta.AudioState
	}
	// A known-silent video whose cached file was evicted only needs the raw
	// video re-fetched — skip the pointless audio re-probe.
	silent := state == "silent"
	go func() {
		muxSem <- struct{}{}
		defer func() { <-muxSem }()
		if state == "abandoned" {
			if err := p.mediaStore.ReviveAudio(key); err != nil {
				log.Printf("media: revive audio for %s: %v", key, err)
			}
		}
		if _, err := p.muxOnce(context.Background(), videoURL, key, silent); err != nil {
			log.Printf("media: background mux for %s: %v", videoURL, err)
		}
	}()
}

// muxOnce runs downloadMuxed/downloadSilent for videoURL at most once across
// concurrent callers. The leader's work runs on a detached context: a client
// disconnecting mid-mux must not abort the download/ffmpeg that other waiters
// (and the on-disk cache) depend on. Waiters, however, honor their own ctx so
// a gone caller doesn't block on an unrelated request's mux.
func (p *Proxy) muxOnce(ctx context.Context, videoURL, key string, silent bool) (*store.MediaMeta, error) {
	muxInflightMu.Lock()
	if call, ok := muxInflight[key]; ok {
		muxInflightMu.Unlock()
		select {
		case <-call.done:
			return call.meta, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &muxCall{done: make(chan struct{})}
	muxInflight[key] = call
	muxInflightMu.Unlock()

	workCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if silent {
		call.meta, call.err = p.downloadSilent(workCtx, videoURL, key)
	} else {
		call.meta, call.err = p.downloadMuxed(workCtx, videoURL, key)
	}
	cancel()

	muxInflightMu.Lock()
	delete(muxInflight, key)
	muxInflightMu.Unlock()
	close(call.done)
	return call.meta, call.err
}

// downloadMuxed runs the full mux flow. A conclusive outcome ('silent' or
// 'has_audio') caches the final file and writes the verdict. Audio probing
// that stays transiently broken past its retry budget instead caches the
// video-only file as an emergency silent copy and parks the row 'failed'
// (or 'abandoned' once retries are exhausted) — it returns that copy with no
// error, so the viewer gets instant playback while L5 retries the audio.
// ffmpeg errors, missing ffmpeg, and video-download failures write no row at
// all, so the next request retries from scratch.
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

	hash := HashURL(key)
	outPath := HashToPath(p.rootPath, hash)
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	// Transient audio-CDN failures (5xx, network blips) are common. Retry the
	// whole probe a few times, spaced >=1s apart, before giving up.
	var audioTmp string
	var audioConfirmedAbsent bool
	probeFailed := false
	for attempt := 1; ; attempt++ {
		audioTmp, audioConfirmedAbsent, err = p.probeAudio(ctx, videoURL, tmpDir)
		if err == nil {
			break
		}
		if attempt >= audioProbeMaxAttempts {
			probeFailed = true
			break
		}
		log.Printf("media: audio probe attempt %d/%d failed for %s: %v; retrying in %s",
			attempt, audioProbeMaxAttempts, videoURL, err, audioProbeRetryDelay)
		select {
		case <-time.After(audioProbeRetryDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if probeFailed {
		// Audio CDN stayed broken through the retry budget. Cache the
		// video-only file as an emergency silent copy so requests stop hitting
		// Reddit live, and record the failure so L5 (and user-triggered
		// retries) can try again later.
		if err := publishToCache(videoTmp, outPath); err != nil {
			return nil, fmt.Errorf("cache emergency silent: %w", err)
		}
		meta, err := p.saveMuxedFile(key, hash, outPath)
		if err != nil {
			return nil, err
		}
		verdict, rErr := p.mediaStore.RecordAudioFailure(key, audioAbandonThreshold)
		if rErr != nil {
			log.Printf("media: record audio failure for %s: %v", key, rErr)
			verdict = "failed"
		}
		meta.AudioState = &verdict
		log.Printf("media: audio mux failed for %s after %d probe retries -> %s (serving silent emergency copy)",
			videoURL, audioProbeMaxAttempts, verdict)
		return meta, nil
	}

	var verdict string
	if audioConfirmedAbsent {
		// Reddit returned 4xx for every audio candidate — this video genuinely
		// has no audio track. Cache the silent video and record the verdict so
		// future requests skip the probe.
		if err := publishToCache(videoTmp, outPath); err != nil {
			return nil, fmt.Errorf("save silent video: %w", err)
		}
		verdict = "silent"
	} else {
		// We have an audio file; ffmpeg must be available to mux.
		if findFfmpeg() == "" {
			return nil, fmt.Errorf("ffmpeg not installed; cannot mux audio")
		}
		// ffmpeg writes a staging file in the cache directory; the final
		// rename is atomic, so a request serving a prior (emergency) copy of
		// this same path never observes a torn file.
		partPath := outPath + ".part"
		cmd := exec.CommandContext(ctx, findFfmpeg(),
			"-y", "-loglevel", "error",
			"-i", videoTmp, "-i", audioTmp,
			"-c", "copy",
			"-map", "0:v:0", "-map", "1:a:0",
			"-movflags", "+faststart",
			"-f", "mp4",
			partPath)
		if out, ferr := cmd.CombinedOutput(); ferr != nil {
			os.Remove(partPath)
			return nil, fmt.Errorf("ffmpeg: %w: %s", ferr, strings.TrimSpace(string(out)))
		}
		if err := os.Rename(partPath, outPath); err != nil {
			os.Remove(partPath)
			return nil, fmt.Errorf("publish muxed file: %w", err)
		}
		verdict = "has_audio"
	}

	meta, err := p.saveMuxedFile(key, hash, outPath)
	if err != nil {
		return nil, err
	}
	if err := p.mediaStore.SetAudioState(key, verdict); err != nil {
		log.Printf("media: set audio_state for %s: %v", key, err)
	}
	meta.AudioState = &verdict
	// Auto-fix the DB: a legacy plain (non-muxed) row for the same raw URL is
	// a silent video-only file that prefetch/archive cached before the mux
	// pipeline existed. Now that the muxed copy supersedes it, drop it.
	p.dropLegacyPlainRow(videoURL)
	return meta, nil
}

// dropLegacyPlainRow removes the non-muxed media_index row (and its file) for
// a raw v.redd.it URL, if one exists. The muxed row keyed muxed:<url> is the
// authoritative copy.
func (p *Proxy) dropLegacyPlainRow(rawURL string) {
	fp, err := p.mediaStore.Delete(rawURL)
	if err != nil {
		log.Printf("media: drop legacy plain row for %s: %v", rawURL, err)
		return
	}
	if fp != nil {
		os.Remove(*fp)
	}
}

// SweepSupersededPlainRows deletes every legacy silent video-only cache entry
// (row + file) whose video already has a conclusive muxed: copy, keeping only
// the final muxed version. Safe to run on every startup — idempotent and a
// no-op once nothing is left to clean.
func (p *Proxy) SweepSupersededPlainRows() {
	paths, err := p.mediaStore.DeleteSupersededPlainRows()
	if err != nil {
		log.Printf("media: sweep superseded plain rows: %v", err)
		return
	}
	for _, fp := range paths {
		if err := os.Remove(fp); err != nil && !os.IsNotExist(err) {
			log.Printf("media: remove superseded file %s: %v", fp, err)
		}
	}
	if len(paths) > 0 {
		log.Printf("media: swept %d superseded silent video-only cache entries", len(paths))
	}
}

// saveMuxedFile stats the file freshly installed at outPath and upserts its
// media_index row.
func (p *Proxy) saveMuxedFile(key, hash, outPath string) (*store.MediaMeta, error) {
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
	return meta, nil
}

// publishToCache atomically installs srcPath's content at outPath. srcPath may
// live on another filesystem, so it is first copied to a staging file inside
// outPath's own directory; the final rename is atomic on the cache volume — a
// request serving a previous version of outPath never sees a torn file.
func publishToCache(srcPath, outPath string) error {
	partPath := outPath + ".part"
	if err := moveOrCopy(srcPath, partPath); err != nil {
		return err
	}
	if err := os.Rename(partPath, outPath); err != nil {
		os.Remove(partPath)
		return err
	}
	return nil
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
	req, err := fhttp.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", p.uaPool.Get())
	transport.ApplyHeaderOrder(req)

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

// ListFailedAudio returns the v.redd.it video URLs whose audio mux has been
// parked as 'failed', oldest first (first-come-first-served). Consumed by the
// prefetch L5 layer to drive background remux retries.
func (p *Proxy) ListFailedAudio(limit int) ([]string, error) {
	keys, err := p.mediaStore.ListAudioFailed(limit)
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(keys))
	for _, k := range keys {
		urls = append(urls, strings.TrimPrefix(k, muxKeyPrefix))
	}
	return urls, nil
}

// RetryMuxAudio re-attempts the mux for a video parked as 'failed'. It is a
// no-op ("skipped") when the row has since been resolved elsewhere, has been
// abandoned, or was attempted within the cooldown — that last case is how a
// user's own view "cancels" a still-queued L5 retry. Outcomes: "recovered"
// (audio is back), "failed" (still no audio), "skipped".
func (p *Proxy) RetryMuxAudio(ctx context.Context, videoURL string) (string, error) {
	key := muxCacheKey(videoURL)
	meta, err := p.mediaStore.Resolve(key)
	if err != nil {
		return "", err
	}
	if meta == nil || meta.AudioState == nil || *meta.AudioState != "failed" {
		return "skipped", nil
	}
	if meta.LastAudioAttemptAt != nil && time.Since(*meta.LastAudioAttemptAt) < muxRetryCooldown {
		return "skipped", nil
	}
	newMeta, err := p.muxOnce(ctx, videoURL, key, false)
	if err != nil {
		return "", err
	}
	if newMeta.AudioState != nil && *newMeta.AudioState == "has_audio" {
		return "recovered", nil
	}
	return "failed", nil
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
