package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/searchquery"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	h.serveSearch(w, r, "")
}

func (h *Handler) handleSubSearch(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	h.serveSearch(w, r, sub)
}

// parsedToArchiveOpts maps a parsed query onto a PostgreSQL archive query. It is
// shared by the offline /search fallback and the /archive local search so both
// honour the same e621-style constraint syntax.
func parsedToArchiveOpts(p searchquery.Parsed) store.ArchiveSearchOpts {
	opts := store.ArchiveSearchOpts{
		Query:     p.TextQuery(),
		After:     p.After,
		Before:    p.Before,
		Media:     p.MediaType,
		Author:    p.Author,
		Flair:     p.Flair,
		WhiteSubs: p.WhiteSubs,
		BlackSubs: p.BlackSubs,
	}
	switch p.Rating {
	case "nsfw":
		opts.NSFW = "nsfw"
	case "safe":
		opts.NSFW = "sfw"
	}
	if p.Score != nil {
		v := p.Score.Val
		opts.Score = &v
		opts.ScoreOp = p.Score.SQLOp()
	}
	if p.Comments != nil {
		v := p.Comments.Val
		opts.Comments = &v
		opts.CommentsOp = p.Comments.SQLOp()
	}
	return opts
}

// filterPostsLocal enforces the constraints Reddit's search API can't express
// (score, comments, date range) over live results before display.
//
// Media type is deliberately NOT enforced here: Reddit's search can't be asked
// to return a specific media kind, so forcing a type:video/image filter over the
// live results would strip out most of a page and routinely leave it empty. We
// let every post through and silently drop the media-type constraint on live
// search — no user-facing notice. (The offline archive fallback still filters by
// media type via parsedToArchiveOpts, where the local DB has the data to do so.)
func filterPostsLocal(q searchquery.Parsed, posts []reddit.Post) []reddit.Post {
	if !q.HasLocalFilter() {
		return posts
	}
	out := make([]reddit.Post, 0, len(posts))
	for _, p := range posts {
		if q.Score != nil {
			n, err := strconv.Atoi(p.Score[1])
			if err != nil || !q.Score.Match(n) {
				continue
			}
		}
		if q.Comments != nil {
			n, err := strconv.Atoi(p.Comments[1])
			if err != nil || !q.Comments.Match(n) {
				continue
			}
		}
		if q.After != nil && int64(p.CreatedTS) < q.After.Unix() {
			continue
		}
		if q.Before != nil && int64(p.CreatedTS) > q.Before.Unix() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// primaryMediaURL returns the post's main cached media URL — the post media, or
// the first gallery image — in the same formatted form serveRandomMedia uses for
// its residency check. Empty when the post carries no media.
func primaryMediaURL(p *reddit.Post) string {
	if p.Media.URL != "" {
		return p.Media.URL
	}
	if len(p.Gallery) > 0 {
		return p.Gallery[0].URL
	}
	return ""
}

// matchCacheScore reports whether a post's primary cached media satisfies the
// `score:` constraint (the media cache eviction score, not the Reddit post
// score). A post whose media is not resident in the cache never matches — the
// eviction score is defined only for files genuinely on disk. Callers guard on
// nc != nil and h.mediaProxy != nil before calling.
func (h *Handler) matchCacheScore(nc *searchquery.NumConstraint, p *reddit.Post) bool {
	mediaURL := primaryMediaURL(p)
	if mediaURL == "" {
		return false
	}
	score, resident := h.mediaProxy.MediaScore(reddit.UnformatURL(mediaURL))
	if !resident {
		return false
	}
	return nc.MatchFloat(score)
}

// buildSearchQuery parses the raw box text and folds any legacy /r/<sub>/search
// path scope into an explicit whitelist constraint, then returns the parsed form
// plus the upstream Reddit `q` string. The redlib-style implicit restrict_sr is
// gone — scoping is now visible and editable in the query itself.
func buildSearchQuery(raw, sub string) (searchquery.Parsed, string) {
	p := searchquery.Parse(raw)
	if sub != "" {
		p.WhiteSubs = addLegacySub(p.WhiteSubs, sub)
	}
	return p, p.RedditQuery()
}

func addLegacySub(subs []string, sub string) []string {
	name := strings.ToLower(sub)
	for _, s := range subs {
		if s == name {
			return subs
		}
	}
	return append([]string{name}, subs...)
}

func (h *Handler) serveSearch(w http.ResponseWriter, r *http.Request, sub string) {
	prefs := h.readPreferences(r)
	query := r.URL.Query().Get("q")
	sort := r.URL.Query().Get("sort")
	t := r.URL.Query().Get("t")
	after := r.URL.Query().Get("after")
	urlPath := r.URL.Path

	parsed, redditQ := buildSearchQuery(query, sub)

	// "Load More" button issues partial=1 requests. Two flavours:
	//   - upstream: the search page in normal mode pages through Reddit via
	//     after-cursor (serveSearchMore).
	//   - local: the offline / upstream_disabled page scrolls the local archive
	//     via offset (serveSearchArchivePartial), driven by infiniteScroll.js.
	// We pick by looking at the request — `offset` ⇒ local, otherwise upstream.
	if r.URL.Query().Get("partial") == "1" {
		if r.URL.Query().Get("offset") != "" {
			h.serveSearchArchivePartial(w, r, parsed, prefs)
			return
		}
		h.serveSearchMore(w, r, parsed, redditQ, query, sort, t, after, prefs)
		return
	}

	cacheKey := prefs.Lang + ":" + urlPath + "?" + r.URL.RawQuery

	// 1. Cache
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota
	degrade, reason := h.shouldDegrade(r.Context())
	var upstreamErr error
	if !degrade && redditQ != "" {
		posts, subs, nextAfter, err := h.redditCli.FetchSearch(r.Context(), redditQ, "", sort, t, after, 5)
		h.recordUpstream(r.Context())
		if err == nil {
			go h.archiver.ArchivePosts(posts, sub, "search")
			posts = filterPostsLocal(parsed, posts)

			data := render.SearchPageData{
				BasePage: render.BasePage{
					URL:       urlPath,
					Prefs:     prefs,
					BrandName: h.cfg.Render.BrandName,
					Version:   "0.1.0",
				},
				Posts:      posts,
				Subreddits: subs,
				Params: reddit.SearchParams{
					Query: query,
					Sort:  sort,
					// After carries the cursor for the *next* page returned by
					// Reddit, so the "Load More" button knows where to resume.
					After:     nextAfter,
					Timeframe: t,
				},
				Sub:     sub,
				NoPosts: len(posts) == 0,
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Source", "fallback")
			if err := h.renderer.RenderSearch(w, data); err != nil {
				log.Printf("handler: render search: %v", err)
			}
			return
		}
		upstreamErr = err
		log.Printf("handler: search %q: %v", query, err)
	}

	// 3. Archive search (offline fallback) — same e621 constraints, but served
	// purely from PostgreSQL.
	if redditQ != "" {
		opts := parsedToArchiveOpts(parsed)
		if prefs.ShowNSFW != "on" {
			opts.NSFW = "sfw"
		}
		opts.Sort = sort
		opts.Limit = 25
		stored, _, _ := h.postStore.ArchiveSearch(opts)
		if len(stored) > 0 {
			var posts []reddit.Post
			for _, sp := range stored {
				var p reddit.Post
				if err := json.Unmarshal(sp.JSONData, &p); err == nil {
					p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
					posts = append(posts, p)
				}
			}

			data := render.SearchPageData{
				BasePage: render.BasePage{
					URL:            urlPath,
					Prefs:          prefs,
					BrandName:      h.cfg.Render.BrandName,
					Version:        "0.1.0",
					DegradedReason: reason,
				},
				Posts: posts,
				Params: reddit.SearchParams{
					Query: query,
					Sort:  sort,
				},
				Sub:         sub,
				NoPosts:     len(posts) == 0,
				IsOffline:   reason == "",
				IsLocalOnly: true,
				PageSize:    25,
				Interval:    prefs.ScrollInterval,
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Source", "archive")
			if err := h.renderer.RenderSearch(w, data); err != nil {
				log.Printf("handler: render search from archive: %v", err)
			}
			return
		}
	}

	// 4. Nothing available.
	//
	// An HR cooldown / quota-exhausted degrade goes to /fuckreddit, whose
	// countdown page is built for exactly those reset-window states.
	//
	// A genuine upstream failure (bad request, network error, Reddit 4xx/5xx)
	// is NOT a reset-window state: redirecting there with an empty reason
	// renders the misleading green "All right" page and swallows the real
	// cause. Surface the actual error instead.
	if reason != "" {
		// preserve query string so the upstream link keeps the search terms.
		h.redirectFuckReddit(w, r, r.URL.RequestURI(), reason)
		return
	}
	if upstreamErr != nil {
		h.renderer.RenderError(w, prefs.Lang, "Search request failed", http.StatusBadGateway, upstreamErr.Error())
		return
	}
	h.redirectFuckReddit(w, r, r.URL.RequestURI(), reason)
}

// serveSearchMore handles partial=1 "Load More" requests. It cascades through
// the same HR gate as a normal search: if the HR layer or OAuth quota says to
// degrade, it returns an empty body with an X-Degraded header so the button
// can stop and explain why. Otherwise it fetches the next 3 posts upstream and
// returns only the post-list HTML fragment, with the next-page cursor in the
// X-Next-After header.
func (h *Handler) serveSearchMore(w http.ResponseWriter, r *http.Request, parsed searchquery.Parsed, redditQ, query, sort, t, after string, prefs reddit.Preferences) {
	if redditQ == "" {
		http.Error(w, "missing query", http.StatusBadRequest)
		return
	}

	// HR gate / OAuth quota — same four-level chain entry as serveSearch.
	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("X-Degraded", reason)
		w.WriteHeader(http.StatusOK)
		return
	}

	posts, _, nextAfter, err := h.redditCli.FetchSearch(r.Context(), redditQ, "", sort, t, after, 3)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: search more %q: %v", query, err)
		w.Header().Set("X-Degraded", "upstream_error")
		w.WriteHeader(http.StatusOK)
		return
	}

	go h.archiver.ArchivePosts(posts, "", "search")
	posts = filterPostsLocal(parsed, posts)

	w.Header().Set("X-Next-After", nextAfter)
	w.Header().Set("X-Source", "fallback")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderSearchPostList(w, posts, prefs); err != nil {
		log.Printf("handler: render search more: %v", err)
	}
}

// serveSearchArchivePartial paginates the offline-archive search results that
// back the local-only /search page. It runs the same e621-style query the full
// page rendered, but at the requested `offset`, and emits just the post-list
// fragment so infiniteScroll.js can append it. Never touches Reddit.
func (h *Handler) serveSearchArchivePartial(w http.ResponseWriter, r *http.Request, parsed searchquery.Parsed, prefs reddit.Preferences) {
	if h.postStore == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	opts := parsedToArchiveOpts(parsed)
	if prefs.ShowNSFW != "on" {
		opts.NSFW = "sfw"
	}
	opts.Sort = r.URL.Query().Get("sort")
	opts.Limit = 25
	opts.Offset = offset
	stored, _, err := h.postStore.ArchiveSearch(opts)
	if err != nil {
		log.Printf("handler: search archive partial: %v", err)
	}

	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
			posts = append(posts, p)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Source", "archive")
	if len(posts) == 0 {
		return
	}
	if err := h.renderer.RenderSearchPostList(w, posts, prefs); err != nil {
		log.Printf("handler: render search archive partial: %v", err)
	}
}

func (h *Handler) backgroundArchiveSearch(query, sub, sort, t, after string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, redditQ := buildSearchQuery(query, sub)
	if redditQ == "" {
		return
	}
	var posts []reddit.Post
	var err error

	if h.oauthHolder.HasAvailableTokens() {
		posts, _, _, err = h.redditCli.FetchSearch(ctx, redditQ, "", sort, t, after, 5)
	} else {
		posts, _, _, err = h.publicCli.FetchSearch(ctx, redditQ, "", sort, t, after, 5)
	}
	h.recordUpstream(ctx)
	if err != nil {
		log.Printf("background archive search %q: %v", query, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "search")
}
