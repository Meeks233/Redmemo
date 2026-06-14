package media

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

// audioTrackKeyPrefix marks the mediaStore row for a video's standalone audio
// track — the sibling DASH_AUDIO_*.mp4 cached on its own so the page's hidden
// <audio> companion can start playing audible bytes the moment they land,
// without waiting for the full video to download or the background mux to
// finish. It is intentionally distinct from muxKeyPrefix: the muxed file
// (video+audio) and the audio-only companion coexist, and the page upgrades
// from <video silent> + <audio> to the single muxed file once the mux lands.
const audioTrackKeyPrefix = "audio:"

func audioTrackKey(videoURL string) string {
	return audioTrackKeyPrefix + videoURL
}

var (
	audioTrackInflightMu sync.Mutex
	audioTrackInflight   = map[string]*dlCall{}
)

// ServeSeparateAudio serves the standalone audio track for a v.redd.it DASH
// video. Status mapping:
//   - 200 + bytes: a real audio track is on disk (cached or freshly fetched).
//   - 204 No Content: the video has no audio (or only Reddit's silent
//     placeholder). The browser <audio> element treats this as "no source"
//     and stays quiet.
//   - 503 Service Unavailable: audio CDN is transiently unreachable.
func (p *Proxy) ServeSeparateAudio(w http.ResponseWriter, r *http.Request, videoURL string) {
	// Tag this request as audio at a fresh generation so its bytes preempt
	// any in-flight video bytes for older requests at the priority gate.
	r = r.WithContext(WithPriority(r.Context(), Priority{
		Gen:  NextGen(),
		Kind: KindAudio,
		Long: r.URL.Query().Get("long") == "1",
	}))
	key := audioTrackKey(videoURL)

	// Fast path: the mux pipeline has previously concluded this video has no
	// audio — skip the probe.
	if m, _ := p.mediaStore.Resolve(muxCacheKey(videoURL)); m != nil &&
		m.AudioState != nil && *m.AudioState == "silent" {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if meta := p.cachedMedia(key); meta != nil {
		p.cache.RecordMediaAccess(r.Context(), key)
		p.serve(w, r, meta, false)
		return
	}

	meta, status, err := p.fetchSeparateAudio(r.Context(), videoURL, key)
	if err != nil {
		log.Printf("media: separate audio fetch for %s: %v", videoURL, err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "audio not ready", http.StatusServiceUnavailable)
		return
	}
	if status == "silent" || meta == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p.serve(w, r, meta, false)
}

// fetchSeparateAudio probes audio candidates for videoURL, downloads the
// successful one, and writes a media_index row keyed by key. Single-flight:
// concurrent callers (a feed of visible videos all preloading audio) share
// one fetch.
func (p *Proxy) fetchSeparateAudio(ctx context.Context, videoURL, key string) (*store.MediaMeta, string, error) {
	audioTrackInflightMu.Lock()
	if call, ok := audioTrackInflight[key]; ok {
		audioTrackInflightMu.Unlock()
		select {
		case <-call.done:
			status := ""
			if call.meta == nil && call.err == nil {
				status = "silent"
			}
			return call.meta, status, call.err
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	call := &dlCall{done: make(chan struct{})}
	audioTrackInflight[key] = call
	audioTrackInflightMu.Unlock()

	var status string
	// A sibling caller may have populated the row between the cache miss in
	// ServeSeparateAudio and our taking the lead — recheck before re-fetching.
	if meta := p.cachedMedia(key); meta != nil {
		call.meta = meta
	} else {
		fetchSem <- struct{}{}
		workCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		meta, st, err := p.downloadSeparateAudio(workCtx, videoURL, key)
		cancel()
		<-fetchSem
		call.meta = meta
		call.err = err
		status = st
	}

	audioTrackInflightMu.Lock()
	delete(audioTrackInflight, key)
	audioTrackInflightMu.Unlock()
	close(call.done)
	return call.meta, status, call.err
}

// downloadSeparateAudio probes the audio candidates for videoURL and persists
// the first real audio track found as a standalone cache entry. Returns
// (nil, "silent", nil) when every candidate definitively 4xxs (no audio
// track exists) or the served track is the silent ~4 kbps placeholder.
func (p *Proxy) downloadSeparateAudio(ctx context.Context, videoURL, key string) (*store.MediaMeta, string, error) {
	tmpDir, err := os.MkdirTemp("", "redmemo-audiotrack-")
	if err != nil {
		return nil, "", fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	audioTmp, absent, err := p.probeAudio(ctx, videoURL, tmpDir)
	if err != nil {
		return nil, "", err
	}
	if absent {
		return nil, "silent", nil
	}
	if isSilent, serr := audioIsSilentPlaceholder(audioTmp); serr == nil && isSilent {
		return nil, "silent", nil
	}
	hash, outPath, err := publishContent(audioTmp, p.rootPath)
	if err != nil {
		return nil, "", fmt.Errorf("publish audio: %w", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return nil, "", fmt.Errorf("stat audio: %w", err)
	}
	meta := &store.MediaMeta{
		OriginalURL: key,
		Hash:        hash,
		FilePath:    &outPath,
		MIMEType:    "audio/mp4",
		FileSize:    info.Size(),
	}
	if err := p.mediaStore.Save(meta); err != nil {
		return nil, "", fmt.Errorf("save index: %w", err)
	}
	return meta, "ready", nil
}
