package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
)

type Proxy struct {
	rootPath          string
	useNginx          bool
	mediaStore        *store.MediaIndexStore
	unavailableStore  *store.MediaUnavailableStore
	cache             *cache.Cache
	httpClient        httpDoer
	userAgentFn       func() string

	// failMu guards failures, an in-memory note of URLs whose upstream fetch
	// returned a terminal 404/410. Reddit's signed preview URLs (preview.redd.it
	// / external-preview.redd.it) eventually expire and 404 forever; without
	// this gate, ensureCached + startBackgroundDownload would loop on every
	// /api/media_status poll, and imageReload.js would spin for its full
	// 5-minute MAX_POLLS budget on each image. Entries are dropped after
	// permFailureTTL so a recovered URL (re-uploaded asset on the same path)
	// is eventually retried.
	failMu   sync.Mutex
	failures map[string]time.Time

	cleanupLog *EventLog
}

const permFailureTTL = 1 * time.Hour

// httpDoer is the subset of tls_client.HttpClient the media proxy depends on,
// narrowed so tests can inject a plain fhttp client.
type httpDoer interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}

// NewProxy wires the media proxy. userAgentFn must return the User-Agent of
// the currently active OAuth session so media fetches share one identity with
// the API client; mixing a random UA pool here previously emitted multiple
// UAs from the same IP within seconds, defeating the single-identity model.
// The injected closure is expected to block during the cold-start window
// rather than fall back to a pool UA — see TokenHolder.WaitForUserAgent.
func NewProxy(cfg config.MediaConfig, mediaStore *store.MediaIndexStore, unavailableStore *store.MediaUnavailableStore, c *cache.Cache, userAgentFn func() string) *Proxy {
	return &Proxy{
		rootPath:         cfg.RootPath,
		mediaStore:       mediaStore,
		unavailableStore: unavailableStore,
		cache:            c,
		// Reddit's media CDNs (v.redd.it / i.redd.it) increasingly stall or
		// reset connections whose TLS handshake doesn't look like a browser's.
		// Use the same uTLS-spoofed TLS handshake every other Reddit-facing
		// client uses — without it v.redd.it video/audio segment fetches hang
		// until they time out. The MEDIA variant of the spoof keeps the
		// ClientHello byte-identical but inflates the HTTP/2 flow-control
		// windows to 64 MiB stream / 256 MiB connection so a single multi-MiB
		// Range response (or a viewport full of concurrent video downloads
		// sharing one h2 conn) never gets RST_STREAM'd with FLOW_CONTROL_ERROR
		// mid-body — the "1s then corrupt" symptom on direct-link/fallback
		// video URLs. The 3-minute ceiling covers large 1080p clips; the old
		// 60s cap aborted them mid-download even when the mux context allowed
		// longer.
		httpClient:  transport.NewMediaSpoofedClient(3 * time.Minute),
		userAgentFn: userAgentFn,
	}
}

func (p *Proxy) ServeMedia(w http.ResponseWriter, r *http.Request) {
	// Every freshly-arrived media request gets a new generation so it preempts
	// older in-flight downloads at the global priority gate. Images/video go in
	// at video tier; standalone audio uses ServeSeparateAudio which tags audio.
	// long=1 marks a long clip the frontend has explicitly unlocked — those
	// fall to the bottom of the gate so they never block short media (see
	// Priority.better).
	r = r.WithContext(WithPriority(r.Context(), Priority{
		Gen:  NextGen(),
		Kind: KindVideo,
		Long: r.URL.Query().Get("long") == "1",
	}))
	originalURL := html.UnescapeString(r.URL.Query().Get("url"))
	if originalURL == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}
	// SSRF guard: the upstream URL is request-supplied — anchoring to Reddit's
	// own CDN host suffixes keeps an attacker from coercing us into fetching
	// arbitrary internal services (Redis on localhost, cloud metadata, LAN
	// HTTP, etc.) and streaming their responses back. The path-based proxies
	// (/img, /vid, ...) already reconstruct their host from a fixed prefix; the
	// generic /proxy/media surface is the only one that needs this gate.
	if !isAllowedUpstreamHost(originalURL) {
		http.Error(w, "host not allowed", http.StatusBadRequest)
		return
	}

	// ensureCached serves a disk hit instantly and otherwise fetches the media
	// through the deduplicated, concurrency-capped path — so a feed full of
	// uncached posts can't burst dozens of identical CDN fetches, and an
	// on-demand request never races the prefetch L2 layer at the same file.
	meta, err := p.ensureCached(r.Context(), originalURL)
	if err != nil {
		if err == errMediaUnavailable {
			// Persistent ledger refused the fetch. Pick the placeholder by
			// state: "?" for marked-but-revivable, "X / Sorry, we missed it"
			// for terminal dead. Non-image consumers (direct nav, <video>)
			// get the same SVG body — better than a stalled player or a
			// 503 the browser surfaces as a broken-media icon.
			p.serveUnavailableMarker(w, r, p.unavailableState(originalURL))
			return
		}
		if err == errPermFailed {
			// Upstream is 404/410 forever (expired signed CDN URL,
			// external host gone). Treat as terminal so users see the
			// honest "Sorry, we missed it" placeholder instead of a
			// browser broken-image icon — same outcome as a dead ledger
			// entry, without needing 3 failures to accumulate.
			p.serveUnavailableMarker(w, r, store.StateDead)
			return
		}
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
		// Permanently-gone signed URLs: skip the background-download kick (it
		// would just re-404) so /api/media_status flips to "failed" on the
		// first poll and imageReload.js can tear its spinner down.
		if !p.isPermFailed(originalURL) {
			p.startBackgroundDownload(originalURL)
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "media not ready", http.StatusServiceUnavailable)
		return
	}
	p.reverseProxy(w, r, originalURL, false, 0)
}

// allowedUpstreamHostSuffixes pins the generic /proxy/media surface to
// Reddit-owned CDN hostnames. Suffix match (left-anchored at a dot, or exact)
// so a hostile string like "evil.com/.redd.it.evil.com" doesn't slip past.
var allowedUpstreamHostSuffixes = []string{
	"redd.it",
	"redditmedia.com",
	"redditstatic.com",
	"reddit.com",
}

func isAllowedUpstreamHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, suf := range allowedUpstreamHostSuffixes {
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
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

func (p *Proxy) SetCleanupLog(l *EventLog) { p.cleanupLog = l }

// purge drops the media_index row (and its on-disk file) for originalURL.
func (p *Proxy) purge(originalURL string) {
	fp, err := p.mediaStore.Delete(originalURL)
	if err != nil {
		log.Printf("media: purge poisoned row for %s: %v", originalURL, err)
		if p.cleanupLog != nil {
			p.cleanupLog.Addf(LevelError, "purge", "delete %s: %v", originalURL, err)
		}
		return
	}
	if fp != nil {
		os.Remove(*fp)
		if p.cleanupLog != nil {
			p.cleanupLog.Addf(LevelOK, "purge", "removed poisoned cache entry: %s", originalURL)
		}
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

// IsResident reports whether originalURL's media is recorded as physically
// present in the cache — i.e. its dynamic existence score is not the -1
// "absent" sentinel (migration v22). Unlike IsCached it trusts the score
// column (one indexed read) instead of stat()-ing the disk, so the /random
// media path can filter a whole candidate pool without a syscall per entry and
// never redirect to a post whose bytes have been evicted/deleted. The score↔
// file_path invariant (score = -1 <=> file_path IS NULL) keeps this in step
// with the disk.
func (p *Proxy) IsResident(originalURL string) bool {
	key := originalURL
	if isMuxableVRedditURL(originalURL) {
		key = muxCacheKey(originalURL)
	}
	m, err := p.mediaStore.Resolve(key)
	if err != nil || m == nil {
		return false
	}
	if isNonMediaMIME(m.MIMEType) {
		return false // poisoned HTML error page cached as media; not real bytes
	}
	return m.Score >= 0
}

// MediaScore returns the eviction score of the resident media for originalURL
// and whether that media is genuinely cached. It mirrors IsResident's key
// resolution (muxed video remapping, poisoned-MIME rejection, score↔file_path
// invariant) but hands back the numeric score so the search layer can apply a
// `cache_score:` threshold. resident is false — and the score meaningless — for
// any URL with no cache row, an evicted row (score < 0), or a poisoned row;
// callers treat a non-resident asset as not matching any cache_score: constraint.
func (p *Proxy) MediaScore(originalURL string) (score float64, resident bool) {
	key := originalURL
	if isMuxableVRedditURL(originalURL) {
		key = muxCacheKey(originalURL)
	}
	m, err := p.mediaStore.Resolve(key)
	if err != nil || m == nil {
		return -1, false
	}
	if isNonMediaMIME(m.MIMEType) {
		return -1, false
	}
	return m.Score, m.Score >= 0
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
// The media_unavailable ledger is consulted first: a URL marked unavailable
// (typically the owning post was banned and the CDN now 403s every byte
// request) short-circuits with errMediaUnavailable so the proxy stops hammering
// Reddit until the user re-opens the post (handler revives the URL then).
func (p *Proxy) ensureCached(ctx context.Context, originalURL string) (*store.MediaMeta, error) {
	if meta := p.cachedMedia(originalURL); meta != nil {
		return meta, nil
	}
	if p.isPermFailed(originalURL) {
		return nil, errPermFailed
	}
	if p.isUnavailable(originalURL) {
		return nil, errMediaUnavailable
	}
	return p.fetchOnce(ctx, originalURL)
}

var (
	errPermFailed       = fmt.Errorf("upstream permanently unavailable")
	errMediaUnavailable = fmt.Errorf("media marked unavailable in ledger")
)

// unavailableState consults the persistent ledger. Returns "alive" for the
// no-record / not-marked case (proxy proceeds normally), "unavailable" for the
// soft state (proxy refuses but a Revive may bring it back), or "dead" for the
// terminal state (proxy refuses and no Revive can rescue it). A read error
// logs and answers "alive" so a flaky DB never silently disables the entire
// media path.
func (p *Proxy) unavailableState(originalURL string) string {
	if p.unavailableStore == nil {
		return store.StateAlive
	}
	st, err := p.unavailableStore.State(originalURL)
	if err != nil {
		log.Printf("media: state lookup for %s: %v", originalURL, err)
		return store.StateAlive
	}
	return st
}

// isUnavailable is a thin convenience over unavailableState for the many call
// sites that only need a boolean. Both soft and terminal states refuse the
// fetch — only the placeholder picker cares about the distinction.
func (p *Proxy) isUnavailable(originalURL string) bool {
	return p.unavailableState(originalURL) != store.StateAlive
}

// ReviveMedia clears the "unavailable" mark on a set of raw URLs so the proxy
// will attempt them once more. Called by the post handler when a user opens
// the owning post: the assumption is that an active visit is fresh signal we
// should re-probe at most one more time before re-marking on the next failure.
// Both bare URLs (preview/i.redd.it/...) and v.redd.it segment URLs are
// accepted; the muxed:<url> alias is handled by the underlying canonical key
// derivation, so callers don't need to pre-wrap.
func (p *Proxy) ReviveMedia(rawURLs []string) {
	if p.unavailableStore == nil || len(rawURLs) == 0 {
		return
	}
	if err := p.unavailableStore.Revive(rawURLs); err != nil {
		log.Printf("media: revive %d urls: %v", len(rawURLs), err)
	}
}

// recordUnavailable bumps the failure counter on the persistent ledger, with
// the project-standard threshold of 3. status==0 covers network errors. Safe
// to call when the store is nil (tests).
func (p *Proxy) recordUnavailable(originalURL string, status int, errMsg string) {
	if p.unavailableStore == nil {
		return
	}
	if _, _, err := p.unavailableStore.RecordFailure(
		originalURL, "", status, errMsg, store.DefaultUnavailableThreshold,
	); err != nil {
		log.Printf("media: record unavailable for %s: %v", originalURL, err)
	}
}

// markPermFailed records originalURL as known-bad so subsequent ensureCached /
// MediaStatus calls fail fast instead of re-fetching a doomed CDN URL. Called
// when upstream returns 404 or 410.
func (p *Proxy) markPermFailed(originalURL string) {
	p.failMu.Lock()
	defer p.failMu.Unlock()
	if p.failures == nil {
		p.failures = make(map[string]time.Time)
	}
	p.failures[originalURL] = time.Now().Add(permFailureTTL)
}

func (p *Proxy) isPermFailed(originalURL string) bool {
	p.failMu.Lock()
	defer p.failMu.Unlock()
	exp, ok := p.failures[originalURL]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(p.failures, originalURL)
		return false
	}
	return true
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
		// 10s ceiling per on-demand image fetch: an upstream that hasn't
		// produced bytes by then is treated as unreachable, the failure
		// feeds the unavailable ledger, and the page falls back to the
		// "Sorry, we missed it" placeholder instead of leaving the
		// imageReload spinner spinning for minutes against a dead host
		// (expired external-preview signatures, gone imgur assets, etc.).
		workCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	if p.isPermFailed(originalURL) {
		// Treat permanent 404/410 as terminal "dead" so imageReload.js
		// reloads the slot and the proxy answers with the X-icon
		// "Sorry, we missed it" placeholder. Returning "failed" here
		// instead left the user staring at the browser's native broken-
		// image icon — most visible on old posts whose external-preview
		// signed URL has long since expired (e.g. embedded imgur links).
		return "dead"
	}
	// "unavailable" = soft state (question-mark placeholder, revive on visit);
	// "dead" = terminal (X-icon "Sorry, we missed it…", never retried).
	switch p.unavailableState(originalURL) {
	case store.StateUnavailable:
		return "unavailable"
	case store.StateDead:
		return "dead"
	}
	p.startBackgroundDownload(originalURL)
	return "pending"
}

// loaderSVG is an animated spinner served in place of an empty/broken image
// when the upstream fetch is blocked, rate-limited, or otherwise unavailable.
// Standalone SVG-via-<img>: page CSS doesn't apply, animations must live inside
// the document. CSS @keyframes works in <img src> context across Chromium,
// Firefox, and WebKit; SMIL is kept as a belt-and-suspenders fallback but on
// a separate sub-element so the two don't both fight for the same transform.
const loaderSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><g transform-origin="12 12"><path d="M21 12a9 9 0 1 1-6.219-8.56"/><animateTransform attributeName="transform" attributeType="XML" type="rotate" from="0 12 12" to="360 12 12" dur="1s" repeatCount="indefinite"/></g></svg>`

// placeholderCardCSS bakes the exact tokens style.css resolves for
// .media-unavailable-card (audioSync.js's <video> placeholder) into the
// SVG so a standalone SVG-served-as-<img> looks the same as the card,
// per theme. SVG-as-<img> can't inherit page CSS or read theme classes,
// so the only variants we can honour are the two `prefers-color-scheme`
// flavours — Tokyo Night (a .tokyoNight class) falls back to the dark
// defaults, matching the audioSync card on the same page.
//
//   token        dark (default)   light
//   --background #0f0f0f          #ddd
//   --highlighted #333            white
//   --accent     #d54455          #bb2b3b
//   --text       white            black
const placeholderCardCSS = `<style>
.card-bg{fill:#0f0f0f;stroke:#333}
.card-icon{stroke:#d54455;opacity:.75}
.card-title{fill:#fff}
.card-hint{fill:#fff;opacity:.7}
@media (prefers-color-scheme: light){
.card-bg{fill:#ddd;stroke:#fff}
.card-icon{stroke:#bb2b3b}
.card-title{fill:#000}
.card-hint{fill:#000;opacity:.7}
}
</style>`

// uncertainSVG (lucide circle-question-mark) is the SOFT placeholder served
// while the URL is marked but not yet dead — we tried, failed N times, but
// the user can still revive the row by reopening the post. Visually mirrors
// the .media-unavailable-card audioSync.js uses for <video>: dashed border,
// accent-coloured icon, 600-weight title, dimmer hint. Colours are baked from
// the page's theme tokens (see placeholderCardCSS) because SVG-as-<img>
// inherits no page CSS.
const uncertainSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 320 200" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Media temporarily unavailable">
` + placeholderCardCSS + `
<rect class="card-bg" x="6" y="6" width="308" height="188" rx="6" ry="6" stroke-width="1" stroke-dasharray="4 3"/>
<g class="card-icon" transform="translate(136 36) scale(2)" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
<circle cx="12" cy="12" r="10"/>
<path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/>
<path d="M12 17h.01"/>
</g>
<text class="card-title" x="160" y="148" text-anchor="middle" font-family="system-ui,-apple-system,Segoe UI,Roboto,sans-serif" font-size="16" font-weight="600">Media temporarily unavailable</text>
<text class="card-hint" x="160" y="172" text-anchor="middle" font-family="system-ui,-apple-system,Segoe UI,Roboto,sans-serif" font-size="12">Reopen the post to retry</text>
</svg>`

// deadSVG (lucide X) is the TERMINAL placeholder: ledger says dead_at IS NOT
// NULL, the URL has already burned one user-triggered retry, and we will never
// hit Reddit for it again. Same card aesthetic as uncertainSVG, with the X
// glyph and the honest "Sorry, we missed it" caption.
const deadSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 320 200" preserveAspectRatio="xMidYMid meet" role="img" aria-label="Sorry, we missed archiving this media">
` + placeholderCardCSS + `
<rect class="card-bg" x="6" y="6" width="308" height="188" rx="6" ry="6" stroke-width="1" stroke-dasharray="4 3"/>
<g class="card-icon" transform="translate(136 36) scale(2)" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
<path d="M18 6 6 18"/>
<path d="m6 6 12 12"/>
</g>
<text class="card-title" x="160" y="148" text-anchor="middle" font-family="system-ui,-apple-system,Segoe UI,Roboto,sans-serif" font-size="16" font-weight="600">Sorry, we missed it…</text>
<text class="card-hint" x="160" y="172" text-anchor="middle" font-family="system-ui,-apple-system,Segoe UI,Roboto,sans-serif" font-size="12">Reddit removed this before we could archive it</text>
</svg>`

// serveUnavailableMarker writes the right placeholder for the given ledger
// state: uncertainSVG for "marked but revivable", deadSVG for "terminal".
// 200 OK so <img> renders the SVG inline rather than triggering imageReload.js's
// spinner; "no-store" so a later Revive doesn't get masked by a cached
// placeholder. X-Media-State carries the state for client-side diagnostics.
func (p *Proxy) serveUnavailableMarker(w http.ResponseWriter, r *http.Request, state string) {
	body := uncertainSVG
	if state == store.StateDead {
		body = deadSVG
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("X-Media-State", state)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

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

// applyDownloadName sets a friendly Content-Disposition filename for video and
// GIF responses when the request carried a dl_title query parameter. The title
// is sanitized (spaces → underscores, unsafe chars dropped, length-capped) and
// joined with a URL-derived unique id and the MIME-mapped extension. Silently
// skipped for still images and for requests without dl_title — the bare proxy
// URL remains the filename in those cases.
func applyDownloadName(w http.ResponseWriter, r *http.Request, originalURL, mime string) {
	if !WantsDownloadName(mime) {
		return
	}
	title := r.URL.Query().Get("dl_title")
	if title == "" {
		return
	}
	w.Header().Set("Content-Disposition", EncodeContentDisposition(BuildDownloadFilename(title, originalURL, mime)))
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
	applyDownloadName(w, r, meta.OriginalURL, meta.MIMEType)

	if p.useNginx {
		w.Header().Set("X-Accel-Redirect", NginxPath(meta.Hash))
		return
	}

	if meta.FilePath != nil {
		http.ServeFile(w, r, *meta.FilePath)
	}
}

func (p *Proxy) Download(ctx context.Context, originalURL string) (*store.MediaMeta, error) {
	// Stream into a staging file in the media root, hashing the bytes in flight.
	// The final path is sha256(content), so the publish rename can only happen
	// once the whole body is read — until then the path is not known. The body
	// is pulled in flow-control-safe Range chunks (see streamRangedTo).
	if err := os.MkdirAll(p.rootPath, 0755); err != nil {
		return nil, fmt.Errorf("mkdir root: %w", err)
	}
	staging, err := os.CreateTemp(p.rootPath, "fetch-*.part")
	if err != nil {
		return nil, fmt.Errorf("create staging: %w", err)
	}
	stagingPath := staging.Name()
	hasher := sha256.New()
	status, hdr, size, err := p.streamRangedTo(ctx, originalURL, 0, nil, io.MultiWriter(staging, hasher), nil)
	staging.Close()
	if err != nil {
		os.Remove(stagingPath)
		// Signed Reddit CDN URLs (preview.redd.it, external-preview.redd.it)
		// 404 permanently once their HMAC s= signature expires; 410 is the
		// canonical "gone". Memo so we stop hammering them.
		if status == http.StatusNotFound || status == http.StatusGone {
			p.markPermFailed(originalURL)
		}
		// Only feed the persistent unavailable ledger when the failure
		// actually points at upstream: a 4xx/5xx response, or a transport
		// error before any response (status==0). A successful 2xx that
		// errors mid-body is OUR failure — the bandwidth limiter / 10s
		// ceiling clipped a stream that Reddit was happily serving — and
		// recording it as an availability strike entombs perfectly alive
		// videos under the "Sorry, we missed it" placeholder.
		isUpstreamFailure := status == 0 || status >= 400
		if isUpstreamFailure {
			p.recordUnavailable(originalURL, status, err.Error())
		}
		return nil, fmt.Errorf("fetch: %w", err)
	}

	// Reddit sometimes answers a blocked/rate-limited media request with a 200
	// carrying an HTML error or login page. Caching that as a "media file"
	// poisons the row and breaks the image permanently — reject it so the next
	// request retries from scratch.
	mimeType := hdr.Get("Content-Type")
	if isNonMediaMIME(mimeType) {
		os.Remove(stagingPath)
		return nil, fmt.Errorf("upstream returned non-media content-type %q", mimeType)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	filePath := HashToPath(p.rootPath, hash)
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		os.Remove(stagingPath)
		return nil, fmt.Errorf("mkdir shard: %w", err)
	}

	// Hold the per-hash publish lock across the rename and the Save below: it
	// serializes this publish against the evictor's os.Remove + MarkEvicted for
	// the same content, closing the download/evict TOCTOU (see LockHash). Held
	// to function return, which lands right after Save.
	unlock := p.mediaStore.LockHash(hash)
	defer unlock()

	// If the same bytes are already cached under a different URL, drop the
	// staging file — disk dedup is structural. Otherwise atomic-rename into
	// place; a concurrent reader of a prior copy at this path never sees a
	// torn file (single-flight keeps two writers off the same path).
	if _, statErr := os.Stat(filePath); statErr == nil {
		os.Remove(stagingPath)
	} else if err := os.Rename(stagingPath, filePath); err != nil {
		os.Remove(stagingPath)
		return nil, fmt.Errorf("publish file: %w", err)
	}

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

// mediaChunkSize bounds each Range request the proxy issues to Reddit's CDN.
// The spoofed HTTP/2 transport advertises a 16 MiB flow-control window and,
// unlike net/http, does not replenish a stream's receive window as the body is
// read — so any single response larger than that window aborts mid-stream with
// FLOW_CONTROL_ERROR (reproduced: a 22 MiB clip dies after exactly 16777216
// bytes). Pulling media in sub-window chunks sidesteps the ceiling and mirrors
// how the real Reddit app fetches DASH segments. A var (not const) so tests can
// shrink it to force the multi-chunk path on small fixtures.
var mediaChunkSize int64 = 8 << 20

// parseContentRangeTotal extracts the total length from a Content-Range value
// like "bytes 0-8388607/23489656". It returns -1 when the total is unknown
// ("*") or the header is missing/malformed.
func parseContentRangeTotal(cr string) int64 {
	i := strings.LastIndexByte(cr, '/')
	if i < 0 {
		return -1
	}
	total, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return total
}

// parseRangeStart returns the first byte offset of a client Range header such
// as "bytes=1000-" or "bytes=1000-2000". Suffix ranges ("bytes=-500") and
// anything unparseable yield 0 — the caller then serves from the start.
func parseRangeStart(h string) int64 {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0
	}
	spec := h[len(prefix):]
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	dash := strings.IndexByte(spec, '-')
	if dash <= 0 {
		return 0
	}
	start, err := strconv.ParseInt(strings.TrimSpace(spec[:dash]), 10, 64)
	if err != nil || start < 0 {
		return 0
	}
	return start
}

// getRangeWith issues a single GET for [start, start+mediaChunkSize) with the
// shared spoof identity. extra carries optional conditional headers (e.g.
// If-Modified-Since); any Range in extra is overridden by the chunk range.
func (p *Proxy) getRangeWith(ctx context.Context, url string, start int64, extra fhttp.Header) (*fhttp.Response, error) {
	req, err := fhttp.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.userAgentFn())
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+mediaChunkSize-1))
	transport.ApplyHeaderOrder(req)
	return p.httpClient.Do(req)
}

// streamRangedTo downloads url from byte offset start to w in flow-control-safe
// chunks until the content ends. It returns the first chunk's status code and
// header (so callers can read Content-Type / total size) plus the bytes
// written. A non-2xx first response returns its status with a non-nil error.
//
// Mid-body errors after the first chunk's status has been observed are retried
// in place: the caller's response headers may already be committed (see
// reverseProxy), so silently aborting at byte N would deliver a truncated body
// the browser surfaces as "corrupt mp4" 1 second into playback. Each chunk's
// own Range fetch is re-attempted streamReadRetries times before bubbling the
// error up.
func (p *Proxy) streamRangedTo(ctx context.Context, url string, start int64, extra fhttp.Header, w io.Writer, live *tokenBucket) (status int, hdr fhttp.Header, written int64, err error) {
	// Throttle every CDN byte through the global media bandwidth bucket — see
	// bwlimit.go / prio.go. Wrapping at this single chokepoint covers Download,
	// reverseProxy, and the mux audio/video probes uniformly. A nil `live`
	// gets the standard gated writer (priority-ordered, global ceiling). A
	// non-nil `live` is a temporary online-playback sample: it shares that
	// per-stream bucket and bypasses the priority gate so its 1 MB/s trickle
	// never starves the background cache-fill. release() unparks lower-priority
	// waiters once we're done (no-op for the ungated live writer).
	var lw *limitedWriter
	if live != nil {
		lw = newLiveStreamWriter(ctx, w, live)
	} else {
		lw = newLimitedWriter(ctx, w)
	}
	defer lw.release()
	w = lw
	offset := start
	total := int64(-1)
	bodyRetries := 0
	for {
		var (
			resp *fhttp.Response
			derr error
		)
		// Open one chunk's Range request, retrying transport errors (h2
		// FLOW_CONTROL_ERROR / RST_STREAM / dropped keep-alive) so a single
		// flaky connection doesn't trash a multi-chunk download.
		for attempt := 0; ; attempt++ {
			resp, derr = p.getRangeWith(ctx, url, offset, extra)
			if derr == nil || attempt >= streamReadRetries || ctx.Err() != nil {
				break
			}
			time.Sleep(streamReadRetryDelay)
		}
		if derr != nil {
			return status, hdr, written, derr
		}
		if status == 0 {
			status = resp.StatusCode
			hdr = resp.Header
			if status != http.StatusOK && status != http.StatusPartialContent {
				resp.Body.Close()
				return status, hdr, written, fmt.Errorf("status %d", status)
			}
			total = parseContentRangeTotal(resp.Header.Get("Content-Range"))
		}
		// Validate the status of EVERY chunk, not just the first (the status==0
		// block above only guards the opening chunk). A continuation chunk that
		// comes back non-2xx — typically a 416 once we step one chunk past a
		// totalless EOF (a 206 whose Content-Range carries no total) — must never
		// have its error body copied into the output: that corrupts the served,
		// and on the Download path hashed-and-cached, media. Bytes already
		// delivered are a clean end; otherwise surface the error.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			if written > 0 {
				return status, hdr, written, nil
			}
			return resp.StatusCode, resp.Header, written, fmt.Errorf("status %d", resp.StatusCode)
		}
		n, cerr := io.Copy(w, resp.Body)
		resp.Body.Close()
		written += n
		offset += n
		if cerr != nil {
			// Mid-body stream error. If we have a known total and haven't
			// reached it yet, re-issue the Range from the new offset. n bytes
			// already shipped to w are fine; the retry resumes after them.
			// Without this, a single h2 FLOW_CONTROL_ERROR / RST_STREAM hands
			// the browser a truncated mp4 that plays for 1s then shows the
			// broken-media glyph.
			if total > 0 && offset < total && bodyRetries < streamReadRetries && ctx.Err() == nil {
				bodyRetries++
				log.Printf("media: stream truncated at %d/%d for %s: %v; retrying (%d/%d)", offset, total, url, cerr, bodyRetries, streamReadRetries)
				time.Sleep(streamReadRetryDelay)
				continue
			}
			return status, hdr, written, cerr
		}
		// 200 means the server ignored Range and sent the whole body in one
		// stream; there is nothing more to fetch.
		if status == http.StatusOK {
			return status, hdr, written, nil
		}
		switch {
		case n == 0:
			return status, hdr, written, nil
		case total >= 0 && offset >= total:
			return status, hdr, written, nil
		case total < 0 && n < mediaChunkSize:
			return status, hdr, written, nil
		}
	}
}

// streamReadRetries / streamReadRetryDelay bound how aggressively
// streamRangedTo recovers from transient h2 stream errors mid-body. Two extra
// attempts per chunk covers the typical FLOW_CONTROL_ERROR / RST_STREAM blip
// without letting a permanently-broken upstream hang the request.
const (
	streamReadRetries    = 2
	streamReadRetryDelay = 250 * time.Millisecond
)

// reverseProxy streams targetURL straight through to the client, fetching it
// from the CDN in flow-control-safe chunks (see streamRangedTo) while presenting
// the client a single coherent response for its requested range. noStore strips
// upstream caching headers and marks the response uncacheable.
//
// streamRate, when > 0, caps this response to a single per-stream token bucket
// (bytes/sec) shared across the prebuffer chunk and every continuation chunk,
// and routes the bytes through the ungated live writer (no priority-gate slot).
// It is the temporary online-playback sample throttle: a continuous, bounded
// trickle that (a) never starves the background cache-fill and (b) never leaves
// the connection idle long enough for the browser to abort and reload the whole
// video. streamRate 0 streams at the full gated rate (used for non-preview
// fallbacks like serveUnavailable).
func (p *Proxy) reverseProxy(w http.ResponseWriter, r *http.Request, targetURL string, noStore bool, streamRate int) {
	start := parseRangeStart(r.Header.Get("Range"))

	// One bucket for the whole live stream — the prebuffer copy below and the
	// streamRangedTo continuation both draw from it, so the cap is enforced
	// across the entire response rather than per chunk.
	var live *tokenBucket
	if streamRate > 0 {
		live = newTokenBucket(streamRate, streamRate/2)
	}

	conditional := fhttp.Header{}
	for _, h := range []string{"If-Modified-Since", "Cache-Control"} {
		if v := r.Header.Get(h); v != "" {
			conditional.Set(h, v)
		}
	}

	// Peek the first chunk to learn status, content-type and total size before
	// committing the client's response headers.
	first, err := p.getRangeWith(r.Context(), targetURL, start, conditional)
	if err != nil {
		serveLoader(w, http.StatusAccepted)
		return
	}
	if first.StatusCode != http.StatusOK && first.StatusCode != http.StatusPartialContent {
		first.Body.Close()
		serveLoader(w, http.StatusAccepted)
		return
	}
	contentType := first.Header.Get("Content-Type")
	total := parseContentRangeTotal(first.Header.Get("Content-Range"))

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	applyDownloadName(w, r, targetURL, contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	if noStore {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		for _, h := range []string{"Cache-Control", "Expires", "Etag", "Last-Modified"} {
			if v := first.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
	}

	if r.Header.Get("Range") != "" && first.StatusCode == http.StatusPartialContent && total >= 0 {
		end := total - 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		if total >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(total-start, 10))
		}
		w.WriteHeader(http.StatusOK)
	}

	// The prebuffer chunk goes through the same per-stream cap as the rest of
	// the live stream (when throttled): an ungated, per-stream-bucketed writer.
	// A small burst (the bucket's capacity) lets the very first bytes — moov /
	// init segment — land fast so playback starts promptly, then it settles to
	// the steady rate. Unthrottled (live==nil) it writes straight to w as before.
	var firstDst io.Writer = w
	if live != nil {
		firstDst = newLiveStreamWriter(r.Context(), w, live)
	}
	n, cerr := io.Copy(firstDst, first.Body)
	first.Body.Close()
	if cerr != nil {
		return
	}
	offset := start + n
	// The first chunk already covered everything when the server sent a full
	// 200 body, a short read, or we reached the declared end.
	if first.StatusCode == http.StatusOK || n == 0 ||
		(total >= 0 && offset >= total) || (total < 0 && n < mediaChunkSize) {
		return
	}
	// Response headers are already committed; on failure the client gets a
	// truncated body. Log so the truncation isn't silent — there's no clean
	// abort path left.
	if status, _, _, err := p.streamRangedTo(r.Context(), targetURL, offset, conditional, w, live); err != nil || (status != 0 && status != http.StatusOK && status != http.StatusPartialContent) {
		log.Printf("proxy: ranged continuation failed offset=%d status=%d url=%s err=%v", offset, status, targetURL, err)
	}
}
