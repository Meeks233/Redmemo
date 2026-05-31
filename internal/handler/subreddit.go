package handler

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
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

// clientIP returns the request's source IP without the ephemeral port. Keying
// the throttle on the bare IP keeps it stable across the client's separate TCP
// connections (each of which carries a different RemoteAddr port).
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (h *Handler) handleFrontPage(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	// When the homepage is removed, redirect to the archive hub instead.
	if prefs.RemoveDefaultFeeds == "on" {
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
		if !partialReq.allow(clientIP(r), time.Duration(interval)*time.Second) {
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
			Version:   "0.1.0",
		},
		Posts:              posts,
		HomepageSort:       sort,
		NoPosts:            len(posts) == 0,
		HasOAuth:           h.oauthHolder.HasAvailableTokens(),
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

func (h *Handler) handleSubreddit(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := h.readPreferences(r)
	h.serveSubreddit(w, r, sub, prefs.PostSort, prefs, 25)
}

func (h *Handler) handleSubredditSort(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	sort := r.PathValue("sort")
	prefs := h.readPreferences(r)
	h.serveSubreddit(w, r, sub, sort, prefs, 25)
}

func (h *Handler) serveSubreddit(w http.ResponseWriter, r *http.Request, sub, sort string, prefs reddit.Preferences, limit int) {
	urlPath := r.URL.Path
	after := r.URL.Query().Get("after")

	// 1. Cache — language-prefixed, see handlePost.
	if cached, _ := h.cache.GetHTML(r.Context(), prefs.Lang+":"+urlPath+"?after="+after); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota. On degrade, skip upstream and fall through.
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderSubredditFallback(w, r, sub, sort, after, prefs, limit) {
			return
		}
	}

	// 3. Archive fallback. Distinguish "truly offline" (upstream failed,
	// reason==""→show offline banner) from "deliberately degraded" (HR /
	// quota, reason!=""→show only degraded banner, not the offline one).
	posts, _ := h.postStore.ListBySubreddit(sub, limit, 0, prefs.ShowNSFW != "on")
	if len(posts) > 0 {
		h.renderSubredditFromArchive(w, r, sub, posts, prefs, reason == "", reason)
		return
	}

	// 4. Nothing available
	h.redirectFuckReddit(w, r, r.URL.Path, reason)
}

func (h *Handler) renderSubredditFallback(w http.ResponseWriter, r *http.Request, sub, sort, after string, prefs reddit.Preferences, limit int) bool {
	if sort == "" {
		sort = "hot"
	}

	posts, before, afterCursor, err := h.redditCli.FetchSubreddit(r.Context(), sub, sort, after, limit)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: fallback fetch subreddit %s: %v", sub, err)
		if h.subStatusStore != nil {
			h.subStatusStore.RecordFailure(sub, err.Error())
		}
		return false
	}

	// Active visit: cached if fresh, else fetch + persist (60-day TTL).
	// Gated by the fetch_sub_about preference — when off (default), the HR
	// layer is cache-only and never triggers an upstream about request.
	// The background icon/about prefetch path (internal/prefetch/icon.go)
	// is independent of this setting.
	activeAbout := prefs.FetchSubAbout == "on"
	subInfo, _ := h.fetchSubredditAbout(r.Context(), sub, activeAbout)

	go func() {
		h.archiver.ArchivePosts(posts, sub, "oauth_fallback")
		if subInfo.Name != "" {
			h.archiver.ArchiveSubreddit(&subInfo)
		}
		if h.subStatusStore != nil {
			h.subStatusStore.MarkLive(sub)
		}
	}()

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sub:                subInfo,
		Posts:              posts,
		Sort:               [2]string{sort, r.URL.Query().Get("t")},
		Ends:               [2]string{before, afterCursor},
		NoPosts:            len(posts) == 0,
		HasOAuth:           h.oauthHolder.HasAvailableTokens(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit: %v", err)
	}
	return true
}

func (h *Handler) renderSubredditFromArchive(w http.ResponseWriter, r *http.Request, sub string, stored []*store.StoredPost, prefs reddit.Preferences, offline bool, degradedReason string) {
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
			Version:        "0.1.0",
			DegradedReason: degradedReason,
		},
		Posts:     posts,
		NoPosts:   len(posts) == 0,
		HasOAuth:  h.oauthHolder.HasAvailableTokens(),
		IsOffline: offline,
	}
	data.Sub.Name = sub

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit from archive: %v", err)
	}
}
