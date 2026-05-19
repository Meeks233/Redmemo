package media

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
	"github.com/redmemo/redmemo/internal/useragent"
)

type Proxy struct {
	rootPath   string
	useNginx   bool
	mediaStore *store.MediaIndexStore
	cache      *cache.Cache
	httpClient httpDoer
	uaPool     *useragent.Pool
}

// httpDoer is the subset of tls_client.HttpClient the media proxy depends on,
// narrowed so tests can inject a plain fhttp client.
type httpDoer interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}

func NewProxy(cfg config.MediaConfig, mediaStore *store.MediaIndexStore, c *cache.Cache, uaPool *useragent.Pool) *Proxy {
	return &Proxy{
		rootPath:   cfg.RootPath,
		mediaStore: mediaStore,
		cache:      c,
		// Reddit's media CDNs (v.redd.it / i.redd.it) increasingly stall or
		// reset connections whose TLS handshake doesn't look like a browser's.
		// Use the same uTLS-spoofed transport every other Reddit-facing client
		// uses — without it v.redd.it video/audio segment fetches hang until
		// they time out. The 3-minute ceiling covers large 1080p clips; the old
		// 60s cap aborted them mid-download even when the mux context allowed
		// longer.
		httpClient: transport.NewSpoofedClient(3 * time.Minute),
		uaPool:     uaPool,
	}
}

func (p *Proxy) ServeMedia(w http.ResponseWriter, r *http.Request) {
	originalURL := html.UnescapeString(r.URL.Query().Get("url"))
	if originalURL == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}

	// ensureCached serves a disk hit instantly and otherwise fetches the media
	// through the deduplicated, concurrency-capped path — so a feed full of
	// uncached posts can't burst dozens of identical CDN fetches, and an
	// on-demand request never races the prefetch L2 layer at the same file.
	meta, err := p.ensureCached(r.Context(), originalURL)
	if err != nil {
		log.Printf("media: serve failed for %s: %v", originalURL, err)
		p.serveUnavailable(w, r, originalURL)
		return
	}

	p.cache.RecordMediaAccess(r.Context(), originalURL)
	p.serve(w, r, meta, false)
}

// serveUnavailable handles a media request that cannot be satisfied right now
// (upstream blocked, rate-limited, or returning a non-media body). For a real
// <img> request — identified by Sec-Fetch-Dest — it answers 503 so the element
// fires an `error` event: imageReload.js then shows an animated spinner and,
// once /api/media_status flips to "ready", reloads just that image in place
// without a page refresh. A background fetch is kicked so that flip can happen
// on its own. Non-image requests (direct navigation, video range streaming)
// keep the previous behaviour — stream upstream / fall back to the inline
// spinner SVG.
func (p *Proxy) serveUnavailable(w http.ResponseWriter, r *http.Request, originalURL string) {
	if r.Header.Get("Sec-Fetch-Dest") == "image" {
		p.startBackgroundDownload(originalURL)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "media not ready", http.StatusServiceUnavailable)
		return
	}
	p.reverseProxy(w, r, originalURL, false)
}

// isNonMediaMIME reports whether a Content-Type is something that must never be
// cached or served as post media — an HTML/JSON/redirect stub Reddit hands back
// when the real asset is blocked. Anything that looks like image/video/audio
// (or an unlabelled octet-stream) is allowed through.
func isNonMediaMIME(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch {
	case m == "":
		return false
	case strings.HasPrefix(m, "image/"),
		strings.HasPrefix(m, "video/"),
		strings.HasPrefix(m, "audio/"),
		m == "application/octet-stream":
		return false
	default:
		return true
	}
}

// purge drops the media_index row (and its on-disk file) for originalURL.
func (p *Proxy) purge(originalURL string) {
	fp, err := p.mediaStore.Delete(originalURL)
	if err != nil {
		log.Printf("media: purge poisoned row for %s: %v", originalURL, err)
		return
	}
	if fp != nil {
		os.Remove(*fp)
	}
}

// fetchSem caps how many media downloads run concurrently. On-demand <img>
// requests, the imageReload background path, and the prefetch L2 layer all
// acquire it, so a feed full of uncached posts cannot burst dozens of
// simultaneous CDN fetches at Reddit.
var fetchSem = make(chan struct{}, 6)

// dlCall is one in-flight single-flight media download. Concurrent callers for
// the same URL — e.g. an on-demand <img> request and the prefetch L2 layer —
// share the leader's result instead of racing two writers at one cache path.
type dlCall struct {
	done chan struct{}
	meta *store.MediaMeta
	err  error
}

var (
	dlMu       sync.Mutex
	dlInflight = map[string]*dlCall{}
)

// cachedMedia returns the media_index row for originalURL only when a valid
// (non-poisoned) media file is actually on disk; otherwise nil. A poisoned row
// — an HTML error page Reddit served while the asset was blocked — is purged
// in passing so the caller re-fetches the genuine file.
func (p *Proxy) cachedMedia(originalURL string) *store.MediaMeta {
	meta, err := p.mediaStore.Resolve(originalURL)
	if err != nil || meta == nil {
		return nil
	}
	if isNonMediaMIME(meta.MIMEType) {
		p.purge(originalURL)
		return nil
	}
	if meta.FilePath == nil {
		return nil
	}
	if _, err := os.Stat(*meta.FilePath); err != nil {
		return nil
	}
	return meta
}

// IsCached reports whether a valid media file for originalURL is already on
// disk. The prefetch L2 layer uses it to cancel a media task the on-demand
// path has already satisfied.
func (p *Proxy) IsCached(originalURL string) bool {
	if isMuxableVRedditURL(originalURL) {
		m, _ := p.mediaStore.Resolve(muxCacheKey(originalURL))
		return fileOnDisk(m)
	}
	return p.cachedMedia(originalURL) != nil
}

// IsFetching reports whether a download for originalURL is in flight right now
// — typically an on-demand (foreground) fetch. The prefetch L2 layer uses it
// to freeze its own duplicate task and let the on-demand fetch win.
func (p *Proxy) IsFetching(originalURL string) bool {
	if isMuxableVRedditURL(originalURL) {
		key := muxCacheKey(originalURL)
		muxInflightMu.Lock()
		_, ok := muxInflight[key]
		muxInflightMu.Unlock()
		return ok
	}
	dlMu.Lock()
	_, ok := dlInflight[originalURL]
	dlMu.Unlock()
	return ok
}

// ensureCached returns a valid cached media file for originalURL, fetching it
// — deduplicated and concurrency-capped — only when it is not already on disk.
func (p *Proxy) ensureCached(ctx context.Context, originalURL string) (*store.MediaMeta, error) {
	if meta := p.cachedMedia(originalURL); meta != nil {
		return meta, nil
	}
	return p.fetchOnce(ctx, originalURL)
}

// fetchOnce runs Download for originalURL at most once across concurrent
// callers. The leader fetches on a detached context — a client disconnecting
// mid-download must not abort a fetch other waiters (and the cache) depend on;
// waiters honor their own ctx so a gone caller doesn't block on the fetch.
func (p *Proxy) fetchOnce(ctx context.Context, originalURL string) (*store.MediaMeta, error) {
	dlMu.Lock()
	if call, ok := dlInflight[originalURL]; ok {
		dlMu.Unlock()
		select {
		case <-call.done:
			return call.meta, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &dlCall{done: make(chan struct{})}
	dlInflight[originalURL] = call
	dlMu.Unlock()

	// A sibling caller may have cached it between our miss and taking the lead.
	if meta := p.cachedMedia(originalURL); meta != nil {
		call.meta = meta
	} else {
		fetchSem <- struct{}{}
		workCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		call.meta, call.err = p.Download(workCtx, originalURL)
		cancel()
		<-fetchSem
	}

	dlMu.Lock()
	delete(dlInflight, originalURL)
	dlMu.Unlock()
	close(call.done)
	return call.meta, call.err
}

// startBackgroundDownload caches originalURL off the request path. The
// single-flight in fetchOnce collapses many imageReload.js pollers (and the
// ServeMedia 503 path) onto one fetch.
func (p *Proxy) startBackgroundDownload(originalURL string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if _, err := p.ensureCached(ctx, originalURL); err != nil {
			log.Printf("media: background download failed for %s: %v", originalURL, err)
		}
	}()
}

// MediaStatus reports whether the proxied media at originalURL is cached and
// ready to serve, for the page's imageReload.js poller. "ready" means a valid
// media file is on disk; "pending" means a background fetch has been kicked
// (or is still running) and the poller should check again shortly.
func (p *Proxy) MediaStatus(originalURL string) string {
	if p.cachedMedia(originalURL) != nil {
		return "ready"
	}
	p.startBackgroundDownload(originalURL)
	return "pending"
}

// loaderSVG is an animated spinner served in place of an empty/broken image
// when the upstream fetch is blocked, rate-limited, or otherwise unavailable.
// SMIL animation runs even inside <img> contexts where scripts can't.
const loaderSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide lucide-loader-icon lucide-loader"><path d="M12 2v4"/><path d="m16.2 7.8 2.9-2.9"/><path d="M18 12h4"/><path d="m16.2 16.2 2.9 2.9"/><path d="M12 18v4"/><path d="m4.9 19.1 2.9-2.9"/><path d="M2 12h4"/><path d="m4.9 4.9 2.9 2.9"/><animateTransform attributeName="transform" attributeType="XML" type="rotate" from="0 12 12" to="360 12 12" dur="1s" repeatCount="indefinite"/></svg>`

func serveLoader(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(loaderSVG)))
	if status == 0 {
		status = http.StatusAccepted
	}
	w.WriteHeader(status)
	io.WriteString(w, loaderSVG)
}

// serve writes a cached media file. noStore marks the response uncacheable —
// used for the silent stand-in of a video whose audio is still being muxed, so
// a page reload re-requests and picks up the finished audio copy.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, meta *store.MediaMeta, noStore bool) {
	w.Header().Set("Content-Type", meta.MIMEType)
	if noStore {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}

	if p.useNginx {
		w.Header().Set("X-Accel-Redirect", NginxPath(meta.Hash))
		return
	}

	if meta.FilePath != nil {
		http.ServeFile(w, r, *meta.FilePath)
	}
}

func (p *Proxy) Download(ctx context.Context, originalURL string) (*store.MediaMeta, error) {
	req, err := fhttp.NewRequestWithContext(ctx, "GET", originalURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", p.uaPool.Get())
	transport.ApplyHeaderOrder(req)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	// Reddit sometimes answers a blocked/rate-limited media request with a 200
	// carrying an HTML error or login page. Caching that as a "media file"
	// poisons the row and breaks the image permanently — reject it so the next
	// request retries from scratch.
	if isNonMediaMIME(resp.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("upstream returned non-media content-type %q", resp.Header.Get("Content-Type"))
	}

	hash := HashURL(originalURL)
	filePath := HashToPath(p.rootPath, hash)

	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	// Write to a staging file and rename — the publish is atomic, so a
	// concurrent reader serving a prior copy of this path never sees a torn
	// file (single-flight already keeps two writers off the same path).
	partPath := filePath + ".part"
	f, err := os.Create(partPath)
	if err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}

	size, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(partPath)
		return nil, fmt.Errorf("write file: %w", err)
	}
	if err := os.Rename(partPath, filePath); err != nil {
		os.Remove(partPath)
		return nil, fmt.Errorf("publish file: %w", err)
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	meta := &store.MediaMeta{
		OriginalURL: originalURL,
		Hash:        hash,
		FilePath:    &filePath,
		MIMEType:    mimeType,
		FileSize:    size,
	}
	if err := p.mediaStore.Save(meta); err != nil {
		return nil, fmt.Errorf("save index: %w", err)
	}

	return meta, nil
}

// DownloadMedia caches a media URL for background callers (prefetch L2,
// archive). A muxable v.redd.it DASH segment is routed through the audio-mux
// pipeline so the cache holds the audio version — never a silent video-only
// file that would later be served soundless.
func (p *Proxy) DownloadMedia(ctx context.Context, originalURL string) error {
	if isMuxableVRedditURL(originalURL) {
		key := muxCacheKey(originalURL)
		if meta, _ := p.mediaStore.Resolve(key); meta != nil && meta.FilePath != nil {
			return nil // already muxed (or has an emergency copy)
		}
		_, err := p.muxOnce(ctx, originalURL, key, false)
		return err
	}
	_, err := p.ensureCached(ctx, originalURL)
	return err
}

// reverseProxy streams targetURL straight through to the client. noStore
// strips upstream caching headers and marks the response uncacheable.
func (p *Proxy) reverseProxy(w http.ResponseWriter, r *http.Request, targetURL string, noStore bool) {
	req, err := fhttp.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		serveLoader(w, http.StatusAccepted)
		return
	}
	req.Header.Set("User-Agent", p.uaPool.Get())

	for _, h := range []string{"Range", "If-Modified-Since", "Cache-Control"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	transport.ApplyHeaderOrder(req)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		serveLoader(w, http.StatusAccepted)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		serveLoader(w, http.StatusAccepted)
		return
	}

	for k, vs := range resp.Header {
		if noStore {
			switch http.CanonicalHeaderKey(k) {
			case "Cache-Control", "Expires", "Etag", "Last-Modified", "Age":
				continue
			}
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if noStore {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
