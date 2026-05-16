package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
)

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	h.serveSearch(w, r, "")
}

func (h *Handler) handleSubSearch(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	h.serveSearch(w, r, sub)
}

func (h *Handler) serveSearch(w http.ResponseWriter, r *http.Request, sub string) {
	prefs := h.readPreferences(r)
	query := r.URL.Query().Get("q")
	sort := r.URL.Query().Get("sort")
	t := r.URL.Query().Get("t")
	after := r.URL.Query().Get("after")
	urlPath := r.URL.Path

	// "Load More" button issues partial=1 requests: instead of a full page it
	// fetches the next 3 posts and returns just the post-list HTML fragment.
	if r.URL.Query().Get("partial") == "1" {
		h.serveSearchMore(w, r, sub, query, sort, t, after, prefs)
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
	if !degrade {
		restrictSR := sub != ""
		posts, subs, nextAfter, err := h.redditCli.FetchSearch(r.Context(), query, sub, sort, t, after, restrictSR, 5)
		h.recordUpstream(r.Context())
		if err == nil {
			go h.archiver.ArchivePosts(posts, sub, "search")

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
					Query:      query,
					Sort:       sort,
					Timeframe:  t,
					// After carries the cursor for the *next* page returned by
					// Reddit, so the "Load More" button knows where to resume.
					After:      nextAfter,
					RestrictSR: restrictSR,
				},
				Sub:                sub,
				NoPosts:            len(posts) == 0,
				AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
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

	// 3. Archive search (offline fallback)
	if query != "" {
		stored, _ := h.postStore.Search(query, 25)
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
				Sub:                sub,
				NoPosts:            len(posts) == 0,
				AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
				IsOffline:          reason == "",
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
func (h *Handler) serveSearchMore(w http.ResponseWriter, r *http.Request, sub, query, sort, t, after string, prefs reddit.Preferences) {
	if query == "" {
		http.Error(w, "missing query", http.StatusBadRequest)
		return
	}

	// HR gate / OAuth quota — same four-level chain entry as serveSearch.
	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("X-Degraded", reason)
		w.WriteHeader(http.StatusOK)
		return
	}

	restrictSR := sub != ""
	posts, _, nextAfter, err := h.redditCli.FetchSearch(r.Context(), query, sub, sort, t, after, restrictSR, 3)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: search more %q: %v", query, err)
		w.Header().Set("X-Degraded", "upstream_error")
		w.WriteHeader(http.StatusOK)
		return
	}

	go h.archiver.ArchivePosts(posts, sub, "search")

	w.Header().Set("X-Next-After", nextAfter)
	w.Header().Set("X-Source", "fallback")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderSearchPostList(w, posts, prefs); err != nil {
		log.Printf("handler: render search more: %v", err)
	}
}

func (h *Handler) backgroundArchiveSearch(query, sub, sort, t, after string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	restrictSR := sub != ""
	var posts []reddit.Post
	var err error

	if h.oauthPool.HasAvailableTokens() {
		posts, _, _, err = h.redditCli.FetchSearch(ctx, query, sub, sort, t, after, restrictSR, 5)
	} else {
		posts, _, _, err = h.publicCli.FetchSearch(ctx, query, sub, sort, t, after, restrictSR, 5)
	}
	h.recordUpstream(ctx)
	if err != nil {
		log.Printf("background archive search %q: %v", query, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "search")
}
