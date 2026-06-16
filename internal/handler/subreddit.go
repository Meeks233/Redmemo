package handler

import (
	"bytes"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/searchquery"
	"github.com/redmemo/redmemo/internal/store"
)

const (
	// partialEntryTTL is how long an idle client IP stays tracked before it is
	// eligible for sweeping; partialSweepInterval is the minimum gap between
	// sweeps. Together they bound partialThrottle's memory under long uptime.
	partialEntryTTL      = 10 * time.Minute
	partialSweepInterval = 5 * time.Minute
)

// partialThrottle rate-limits infinite-scroll partial requests per client IP.
// Stale entries are swept opportunistically inside allow(), so the map can
// never grow without bound regardless of how long the process runs.
type partialThrottle struct {
	mu        sync.Mutex
	seen      map[string]time.Time
	lastSweep time.Time
}

var partialReq = &partialThrottle{seen: make(map[string]time.Time)}

// allow reports whether a request from ip is permitted given the caller's
// minimum spacing, recording the request time when it is.
func (t *partialThrottle) allow(ip string, minGap time.Duration) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	if now.Sub(t.lastSweep) > partialSweepInterval {
		for k, last := range t.seen {
			if now.Sub(last) > partialEntryTTL {
				delete(t.seen, k)
			}
		}
		t.lastSweep = now
	}

	if last, ok := t.seen[ip]; ok && now.Sub(last) < minGap {
		return false
	}
	t.seen[ip] = now
	return true
}

// remoteIP returns the request's source IP without the ephemeral port. Keying
// the throttle on the bare IP keeps it stable across the client's separate TCP
// connections (each of which carries a different RemoteAddr port). Reverse-
// proxy headers are NOT consulted here — the lockout-relevant IP goes through
// (*Handler).clientIP, which only honors X-Forwarded-For when the immediate
// source is one of cfg.Server.TrustedProxyCIDRs.
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// clientIP returns the per-request identifier used by the /settings auth gate
// lockout (and any other per-IP rate logic). When the direct peer's IP matches
// a trusted proxy CIDR, the left-most entry of X-Forwarded-For wins; otherwise
// the direct peer's IP is authoritative. The "left-most" choice mirrors what
// nginx/caddy/cloudflare emit and is the only entry the trusted proxy itself
// observed — every entry after it is attacker-controlled and ignored.
func (h *Handler) clientIP(r *http.Request) string {
	src := remoteIP(r)
	if !h.isTrustedProxy(src) {
		// Misconfig warning (one-shot, no false positives): a forwarded header is
		// present yet no proxy is trusted, so every request collapses to the proxy
		// IP and shares one lockout bucket (DoS + diluted brute-force protection).
		// Guarded so the warning never fires for direct deployments that emit no
		// such header, and logs at most once regardless of request volume.
		if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
			warnUntrustedForwardedHeader()
		}
		return src
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return src
	}
	if i := strings.IndexByte(xff, ','); i >= 0 {
		xff = xff[:i]
	}
	xff = strings.TrimSpace(xff)
	if xff == "" {
		return src
	}
	return xff
}

// untrustedForwardedWarn fires the proxy-misconfig warning at most once for the
// process lifetime, so a high-traffic instance can't flood the log.
var untrustedForwardedWarn sync.Once

func warnUntrustedForwardedHeader() {
	untrustedForwardedWarn.Do(func() {
		log.Print("auth: received X-Forwarded-For but no TrustedProxyCIDRs configured; per-IP lockout keyed on proxy IP — set Server.TrustedProxyCIDRs")
	})
}

func (h *Handler) isTrustedProxy(ip string) bool {
	if h.cfg == nil || len(h.cfg.Server.TrustedProxyCIDRs) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, c := range h.cfg.Server.TrustedProxyCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if net.ParseIP(c).Equal(parsed) {
				return true
			}
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

func (h *Handler) handleFrontPage(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	// The homepage is defined purely by its filter query: a blank one (the box
	// cleared, or input that canonicalised to nothing usable — see
	// blankIfNoAlnum) means "no homepage", so we skip it and redirect to the
	// archive hub. A non-empty query (including the default "all") renders the
	// feed below.
	if strings.TrimSpace(prefs.FrontPageSubs) == "" {
		http.Redirect(w, r, "/archive", http.StatusFound)
		return
	}

	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "new"
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	// The homepage filter IS the global unified search grammar: its stored value
	// is parsed into the same archive query the /search and /archive boxes use, so
	// the feed honours every constraint — sub: scope, author, media type,
	// score/comments thresholds, date bounds and NSFW rating — not just subs. An
	// empty filter (or "all") leaves opts zero, matching every archived post.
	const limit = 5
	var opts store.ArchiveSearchOpts
	if prefs.FrontPageSubs != "" && prefs.FrontPageSubs != "all" {
		opts = parsedToArchiveOpts(searchquery.Parse(prefs.FrontPageSubs))
	}
	if prefs.ShowNSFW != "on" {
		opts.NSFW = "sfw"
	}
	opts.Limit = limit
	opts.Offset = offset
	stored, err := h.postStore.ListHomepage(sort, opts)
	if err != nil {
		log.Printf("handler: homepage db query (%s): %v", sort, err)
	}

	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
			posts = append(posts, p)
		}
	}

	if r.URL.Query().Get("partial") == "1" {
		interval := 2
		if n, err := strconv.Atoi(prefs.ScrollInterval); err == nil && n > 0 {
			interval = n
		}
		if !partialReq.allow(h.clientIP(r), time.Duration(interval)*time.Second) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		h.renderHomepagePartial(w, posts, prefs)
		return
	}

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   render.Version,
		},
		Posts:        posts,
		HomepageSort: sort,
		NoPosts:      len(posts) == 0,
		HasOAuth:     h.oauthHolder.HasAvailableTokens(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render homepage: %v", err)
	}
}

func (h *Handler) renderHomepagePartial(w http.ResponseWriter, posts []reddit.Post, prefs reddit.Preferences) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(posts) == 0 {
		return
	}
	if err := h.renderer.RenderPostList(w, posts, prefs); err != nil {
		log.Printf("handler: render homepage partial: %v", err)
	}
}

// pageLimitFromPrefs parses the user's page_limit setting back into an int,
// clamped to [5, 100]. Empty/invalid values fall back to the 50-post default —
// matching what NormalizeSettings would have refused to persist anyway.
// Reddit's OAuth quota is per-request, not per-item, so a single limit=50
// request costs the same as limit=5 — we may as well harvest more per call.
func pageLimitFromPrefs(prefs reddit.Preferences) int {
	n, err := strconv.Atoi(prefs.PageLimit)
	if err != nil || n < 5 || n > 100 {
		return 50
	}
	return n
}

func (h *Handler) handleSubreddit(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := h.readPreferences(r)
	h.serveSubreddit(w, r, sub, prefs.PostSort, prefs, pageLimitFromPrefs(prefs))
}

func (h *Handler) handleSubredditSort(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	sort := r.PathValue("sort")
	prefs := h.readPreferences(r)
	h.serveSubreddit(w, r, sub, sort, prefs, pageLimitFromPrefs(prefs))
}

func (h *Handler) serveSubreddit(w http.ResponseWriter, r *http.Request, sub, sort string, prefs reddit.Preferences, limit int) {
	// Operator has pinned the instance to cache-only mode: send /r/{sub}
	// visitors to the equivalent archive route instead of attempting upstream
	// and falling back. Skips the cache/HR gate/archive chain entirely so the
	// URL surface advertises the archive truthfully.
	if h.siteDefault("disable_initiative_upstream_access") == "on" {
		http.Redirect(w, r, "/archive/r/"+sub, http.StatusFound)
		return
	}

	urlPath := r.URL.Path
	after := r.URL.Query().Get("after")
	before := r.URL.Query().Get("before")
	cacheKey := htmlCacheKey(urlPath, "after="+after+"&before="+before, prefs)

	// 1. Cache — keyed by full prefs fingerprint so theme/NSFW/page_limit/lang
	// changes never bleed across visitors. See handlePost for the long form.
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// `t=` (Reddit's relative timeframe) is only meaningful for top/
	// controversial; for other sorts it's silently ignored upstream. Source
	// from the URL param so /r/X/top?t=week works without the query box.
	t := r.URL.Query().Get("t")

	// 2. HR gate / OAuth quota. On degrade, skip upstream and fall through.
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderSubredditFallback(w, r, sub, sort, t, after, before, prefs, limit, cacheKey) {
			return
		}
	}

	// 3. Archive fallback. Distinguish "truly offline" (upstream failed,
	// reason==""→show offline banner) from "deliberately degraded" (HR /
	// quota, reason!=""→show only degraded banner, not the offline one).
	posts, _ := h.postStore.ListBySubreddit(sub, limit, 0, prefs.ShowNSFW != "on")
	if len(posts) > 0 {
		h.renderSubredditFromArchive(w, r, sub, posts, prefs, reason == "", reason, cacheKey)
		return
	}

	// 4. Nothing available
	h.serveDegradeMiss(w, r, reason)
}

func (h *Handler) renderSubredditFallback(w http.ResponseWriter, r *http.Request, sub, sort, t, after, before string, prefs reddit.Preferences, limit int, cacheKey string) bool {
	if sort == "" {
		sort = "hot"
	}

	// Coalesce concurrent identical fetches: only one upstream call burns
	// quota even if N visitors hit the same /r/sub/sort?after=… at once.
	// recordUpstream + archiver + MarkLive only fire in the leader's closure
	// so they happen once per real Reddit hit, not once per merged caller.
	type subFetchResult struct {
		posts  []reddit.Post
		before string
		after  string
		err    error
	}
	flightKey := "sub|" + sub + "|" + sort + "|" + t + "|" + after + "|" + before + "|" + strconv.Itoa(limit)
	raw, _, _ := h.upstreamFlight.Do(flightKey, func() (any, error) {
		posts, beforeCursor, afterCursor, err := h.redditCli.FetchSubreddit(r.Context(), sub, sort, t, after, before, limit)
		h.recordUpstream(r.Context())
		res := &subFetchResult{posts: posts, before: beforeCursor, after: afterCursor, err: err}
		if err != nil {
			if h.subStatusStore != nil {
				h.subStatusStore.RecordFailure(sub, err.Error())
			}
			return res, nil
		}
		go func() {
			h.archiver.ArchivePosts(posts, sub, "oauth_fallback")
			if h.subStatusStore != nil {
				h.subStatusStore.MarkLive(sub)
			}
		}()
		return res, nil
	})
	res := raw.(*subFetchResult)
	if res.err != nil {
		log.Printf("handler: fallback fetch subreddit %s: %v", sub, res.err)
		return false
	}
	posts, afterCursor := res.posts, res.after
	// Reddit usually returns a null `before` even after a forward-paginated
	// request, so res.before is unreliable for the Prev link. Mirror redlib:
	// the Prev cursor for *this* page is whatever the user arrived with as
	// `?after=` — i.e. the id of the last post on the page they came from.
	// Clicking Prev navigates to `?before=<that>` so Reddit returns the prior
	// page anchored just before that post.
	prevCursor := after

	// Active visit: cached if fresh, else fetch + persist (60-day TTL).
	// Gated by the fetch_sub_about preference — when off (default), the HR
	// layer is cache-only and never triggers an upstream about request.
	// The background icon/about prefetch path (internal/prefetch/icon.go)
	// is independent of this setting. Kept outside the singleflight because
	// activeAbout is per-user prefs.
	activeAbout := prefs.FetchSubAbout == "on"
	subInfo, _ := h.fetchSubredditAbout(r.Context(), sub, activeAbout)

	if subInfo.Name != "" {
		go h.archiver.ArchiveSubreddit(&subInfo)
	}

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   render.Version,
		},
		Sub:      subInfo,
		Posts:    posts,
		Sort:     [2]string{sort, t},
		Ends:     [2]string{prevCursor, afterCursor},
		NoPosts:  len(posts) == 0,
		HasOAuth: h.oauthHolder.HasAvailableTokens(),
	}

	var buf bytes.Buffer
	if err := h.renderer.RenderSubreddit(&buf, data); err != nil {
		log.Printf("handler: render subreddit: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
	return true
}

func (h *Handler) renderSubredditFromArchive(w http.ResponseWriter, r *http.Request, sub string, stored []*store.StoredPost, prefs reddit.Preferences, offline bool, degradedReason string, cacheKey string) {
	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
			posts = append(posts, p)
		}
	}

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:            r.URL.Path,
			Prefs:          prefs,
			BrandName:      h.cfg.Render.BrandName,
			Version:        render.Version,
			DegradedReason: degradedReason,
		},
		Posts:     posts,
		NoPosts:   len(posts) == 0,
		HasOAuth:  h.oauthHolder.HasAvailableTokens(),
		IsOffline: offline,
	}
	data.Sub.Name = sub

	var buf bytes.Buffer
	if err := h.renderer.RenderSubreddit(&buf, data); err != nil {
		log.Printf("handler: render subreddit from archive: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
}
