package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		After:     p.ArchiveAfter(), // honors date:week / date:month timeframe
		Before:    p.Before,
		Media:     p.MediaTypes,
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
// `cache_score:` constraint (the media cache eviction score, not the Reddit post
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
	after := r.URL.Query().Get("after")
	before := r.URL.Query().Get("before")
	urlPath := r.URL.Path

	parsed, redditQ := buildSearchQuery(query, sub)
	// sort / timeframe used to live in their own <form id="search_sort">; they
	// now spell `sort:` and `date:<keyword>` inside the query box. Fall back
	// to the legacy URL params so old bookmarks and the "Load More" partial
	// cursor (which still echoes data-sort/data-t) keep working. SortForSearch
	// translates the user's word into one /search.json accepts (e.g. sort:hot
	// → relevance) instead of silently dropping it.
	sort := parsed.SortForSearch()
	if sort == "" {
		sort = r.URL.Query().Get("sort")
	}
	t := parsed.Timeframe
	if t == "" {
		t = r.URL.Query().Get("t")
	}

	// "Load More" button issues partial=1 requests. Two flavours:
	//   - upstream: the search page in normal mode pages through Reddit via
	//     after-cursor (serveSearchMore).
	//   - local: the offline / upstream_disabled page scrolls the local archive
	//     via offset (serveSearchArchivePartial), driven by infiniteScroll.js.
	// We pick by looking at the request — `offset` ⇒ local, otherwise upstream.
	// Mint or read the search-session ID before any sub-handler dispatches —
	// every search path (live, load-more, archive partial) needs it for the
	// cross-page seen-titles dedup. The cookie has to land on the response
	// before WriteHeader, so we do it here at the entry point.
	sid := h.ensureSearchSID(w, r)
	sessKey := searchSessionKey(sid, query, sort, t)

	if r.URL.Query().Get("partial") == "1" {
		if r.URL.Query().Get("offset") != "" {
			h.serveSearchArchivePartial(w, r, parsed, prefs, sessKey)
			return
		}
		h.serveSearchMore(w, r, parsed, redditQ, query, sort, t, after, before, prefs, sessKey)
		return
	}

	// HTML cache key includes sid so cached entries don't leak across
	// sessions — a different user's seen-titles must not influence what
	// this user is shown. Same user revisiting the same page (cursor) sees
	// the same cached result, which stays consistent with the snapshot of
	// `sessKey` taken at original render time.
	cacheKey := htmlCacheKey(urlPath, r.URL.RawQuery+"&sid="+sid, prefs)

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
		posts, subs, _, nextAfter, err := h.redditCli.FetchSearch(r.Context(), redditQ, "", sort, t, after, before, pageLimitFromPrefs(prefs))
		h.recordUpstream(r.Context())
		if err == nil {
			go h.archiver.ArchivePosts(posts, sub, "search")
			// In-page fold (same-title clusters collapse to one survivor with
			// a +N badge), then cross-page fold (drop any title that was
			// already presented earlier in this session). Order matters: the
			// session set tracks survivors, so we must fold first.
			posts = reddit.FoldReposts(posts)
			seen := h.loadSeenTitles(r.Context(), sessKey)
			posts = applySessionDedup(posts, seen, after)
			h.saveSeenTitles(r.Context(), sessKey, seen)
			// No client-side post-filter: Reddit's `q` already enforces every
			// API-expressible constraint (subreddit/author/flair/rating + free
			// text). Anything the API can't express (score/comments/date/media)
			// is silently dropped on the live path so the page isn't gutted.
			// The search box echo also drops those tokens so the user doesn't
			// see tags that had no effect on the upstream request.
			displayQuery := parsed.LiveDisplayQuery()

			data := render.SearchPageData{
				BasePage: render.BasePage{
					URL:       urlPath,
					Prefs:     prefs,
					BrandName: h.cfg.Render.BrandName,
					Version:   render.Version,
				},
				Posts:      posts,
				Subreddits: subs,
				Params: reddit.SearchParams{
					Query:     displayQuery,
					Sort:      sort,
					After:     nextAfter,
					Before:    after,
					Timeframe: t,
				},
				Sub:     sub,
				NoPosts: len(posts) == 0,
				// Reddit returns null `before` even after a forward-paginated
				// request, so the Prev cursor is the incoming `?after=` (the
				// last-post-id from the page the user came from) — same trick
				// as the subreddit page (and redlib upstream).
				//
				// Backward pagination needs the mirror flip: a request with
				// `?before=X` and no `?after=` would otherwise emit empty Prev
				// AND Next cursors (after="" → Ends[0]=""; nextAfter often ""
				// for backward fetches → Ends[1]=""), erasing both pagination
				// buttons. In that case derive Prev from posts[0] (one further
				// step back) and Next from the incoming `?before=X` (returns
				// the viewer to the page they came from).
				Ends: searchPageEnds(after, before, nextAfter, posts),
			}

			var buf bytes.Buffer
			if err := h.renderer.RenderSearch(&buf, data); err != nil {
				log.Printf("handler: render search: %v", err)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Source", "fallback")
			w.Write(buf.Bytes())
			h.cacheHTMLAsync(cacheKey, buf.Bytes())
			return
		}
		upstreamErr = err
		log.Printf("handler: search %q: %v", query, err)
	}

	// 3. Archive search (offline fallback) — same e621 constraints, but served
	// purely from PostgreSQL. Gated on the raw query rather than redditQ: a
	// constraint-only box like `type:video score>100` yields an empty redditQ
	// (Reddit's `q` can't express those tags) yet the archive can satisfy it.
	if query != "" {
		opts := parsedToArchiveOpts(parsed)
		if prefs.ShowNSFW != "on" {
			opts.NSFW = "sfw"
		}
		opts.Sort = parsed.SortForArchive()
		if opts.Sort == "" {
			opts.Sort = sort
		}
		opts.Limit = 25
		stored, _, asErr := h.postStore.ArchiveSearch(opts)
		if asErr != nil {
			// Don't let a DB failure masquerade as an empty archive — log it so the
			// fault is diagnosable instead of silently degrading to "nothing found".
			log.Printf("search: archive search failed for %q: %v", query, asErr)
		}
		if len(stored) > 0 {
			var posts []reddit.Post
			for _, sp := range stored {
				var p reddit.Post
				if err := json.Unmarshal(sp.JSONData, &p); err == nil {
					p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
					p.RepostCount = sp.RepostCount
					posts = append(posts, p)
				}
			}
			// SQL DISTINCT ON already folded within this page; apply session
			// dedup so paginating into the archive doesn't replay clusters
			// the user already saw on a prior live or archive page.
			seenArchive := h.loadSeenTitles(r.Context(), sessKey)
			posts = applySessionDedup(posts, seenArchive, "archive:0")
			h.saveSeenTitles(r.Context(), sessKey, seenArchive)
			h.markLocalComments(posts)

			data := render.SearchPageData{
				BasePage: render.BasePage{
					URL:            urlPath,
					Prefs:          prefs,
					BrandName:      h.cfg.Render.BrandName,
					Version:        render.Version,
					DegradedReason: reason,
				},
				Posts: posts,
				Params: reddit.SearchParams{
					Query: query,
					Sort:  sort,
				},
				Sub:     sub,
				NoPosts: len(posts) == 0,
				// "Offline" reflects a *failed* upstream attempt, not any
				// path that fell through to the archive. A constraint-only
				// box (e.g. `type:video score>100`) skips upstream entirely
				// because RedditQuery is empty — that's not an outage.
				IsOffline:   reason == "" && upstreamErr != nil,
				IsLocalOnly: true,
				PageSize:    25,
				Interval:    prefs.ScrollInterval,
			}

			var buf bytes.Buffer
			if err := h.renderer.RenderSearch(&buf, data); err != nil {
				log.Printf("handler: render search from archive: %v", err)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Source", "archive")
			w.Write(buf.Bytes())
			h.cacheHTMLAsync(cacheKey, buf.Bytes())
			return
		}
	}

	// 4. Nothing available.
	//
	// upstream_disabled is a *permanent* operator choice — Reddit is off the
	// table for the foreseeable future. Bouncing to /fuckreddit's "wait for the
	// window to reset" page is the wrong UX; instead render the local-only
	// search page with empty results so the user keeps the search box and can
	// refine the query against the archive.
	//
	// An HR cooldown / quota-exhausted degrade goes to /fuckreddit, whose
	// countdown page is built for exactly those reset-window states.
	//
	// A genuine upstream failure (bad request, network error, Reddit 4xx/5xx)
	// is NOT a reset-window state: redirecting there with an empty reason
	// renders the misleading green "All right" page and swallows the real
	// cause. Surface the actual error instead.
	if reason == "upstream_disabled" {
		data := render.SearchPageData{
			BasePage: render.BasePage{
				URL:            urlPath,
				Prefs:          prefs,
				BrandName:      h.cfg.Render.BrandName,
				Version:        render.Version,
				DegradedReason: reason,
			},
			Params: reddit.SearchParams{
				Query: query,
				Sort:  sort,
			},
			Sub:         sub,
			NoPosts:     true,
			IsLocalOnly: true,
			PageSize:    25,
			Interval:    prefs.ScrollInterval,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Source", "archive")
		if err := h.renderer.RenderSearch(w, data); err != nil {
			log.Printf("handler: render empty archive search: %v", err)
		}
		return
	}
	if reason != "" {
		// preserve query string so the upstream link keeps the search terms.
		h.redirectFuckReddit(w, r, r.URL.RequestURI(), reason)
		return
	}
	if upstreamErr != nil {
		h.renderer.RenderError(w, prefs.Lang, "Search request failed", http.StatusBadGateway, upstreamErr.Error())
		return
	}
	// No degrade, no upstream error, and either the box was empty or its tags
	// were all dropped by RedditQuery (e.g. `type:video score>100`). Render an
	// empty results page rather than bouncing to /fuckreddit's misleading
	// "All right" countdown.
	displayQuery := query
	if redditQ == "" && query != "" {
		displayQuery = parsed.LiveDisplayQuery()
	}
	data := render.SearchPageData{
		BasePage: render.BasePage{
			URL:       urlPath,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   render.Version,
		},
		Params: reddit.SearchParams{
			Query:     displayQuery,
			Sort:      sort,
			Timeframe: t,
		},
		Sub:     sub,
		NoPosts: true,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderSearch(w, data); err != nil {
		log.Printf("handler: render empty search: %v", err)
	}
}

// serveSearchMore handles partial=1 "Load More" requests. It cascades through
// the same HR gate as a normal search: if the HR layer or OAuth quota says to
// degrade, it returns an empty body with an X-Degraded header so the button
// can stop and explain why. Otherwise it fetches the next 3 posts upstream and
// returns only the post-list HTML fragment, with the next-page cursor in the
// X-Next-After header.
func (h *Handler) serveSearchMore(w http.ResponseWriter, r *http.Request, parsed searchquery.Parsed, redditQ, query, sort, t, after, before string, prefs reddit.Preferences, sessKey string) {
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

	posts, _, _, nextAfter, err := h.redditCli.FetchSearch(r.Context(), redditQ, "", sort, t, after, before, pageLimitFromPrefs(prefs))
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: search more %q: %v", query, err)
		w.Header().Set("X-Degraded", "upstream_error")
		w.WriteHeader(http.StatusOK)
		return
	}

	go h.archiver.ArchivePosts(posts, "", "search")
	// Live "Load More" page: same policy as the first page — in-page fold
	// then cross-page session dedup.
	posts = reddit.FoldReposts(posts)
	seen := h.loadSeenTitles(r.Context(), sessKey)
	posts = applySessionDedup(posts, seen, after)
	h.saveSeenTitles(r.Context(), sessKey, seen)

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
func (h *Handler) serveSearchArchivePartial(w http.ResponseWriter, r *http.Request, parsed searchquery.Parsed, prefs reddit.Preferences, sessKey string) {
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
	opts.Sort = parsed.SortForArchive()
	if opts.Sort == "" {
		opts.Sort = r.URL.Query().Get("sort")
	}
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
			p.RepostCount = sp.RepostCount
			posts = append(posts, p)
		}
	}
	// Session dedup — the SQL DISTINCT ON already collapsed clusters within
	// this page; the session layer suppresses any cluster the user already
	// saw on an earlier infinite-scroll batch.
	seen := h.loadSeenTitles(r.Context(), sessKey)
	posts = applySessionDedup(posts, seen, fmt.Sprintf("archive:%d", offset))
	h.saveSeenTitles(r.Context(), sessKey, seen)
	h.markLocalComments(posts)

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
		posts, _, _, _, err = h.redditCli.FetchSearch(ctx, redditQ, "", sort, t, after, "", 5)
	} else {
		posts, _, _, _, err = h.publicCli.FetchSearch(ctx, redditQ, "", sort, t, after, "", 5)
	}
	h.recordUpstream(ctx)
	if err != nil {
		log.Printf("background archive search %q: %v", query, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "search")
}

// searchPageEnds picks the [prev, next] cursors the search template renders
// as Prev/Next links. Reddit's `before` field is unreliable on a forward
// fetch, so the canonical Prev anchor for a forward page is the incoming
// `?after=`; the canonical Next anchor is the API's returned afterCursor.
//
// Backward pagination is the awkward case: when the user arrived with
// `?before=X` and no `?after=`, the naive [after, nextAfter] tuple is empty
// on BOTH ends and the whole pagination footer vanishes. The fix derives the
// Prev from the first post on this page (so clicking Prev steps one batch
// further back) and the Next from the literal `?before=X` the viewer arrived
// with (so clicking Next returns them to the page they came from). Post IDs
// arrive bare (e.g. "10hprl2") — Reddit's cursor wants the `t3_` thing-kind
// prefix.
func searchPageEnds(after, before, nextAfter string, posts []reddit.Post) [2]string {
	if before != "" && after == "" {
		var prev string
		if len(posts) > 0 {
			prev = "t3_" + posts[0].ID
		}
		next := nextAfter
		if next == "" {
			next = before
		}
		return [2]string{prev, next}
	}
	return [2]string{after, nextAfter}
}
