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

	cacheKey := urlPath + "?" + r.URL.RawQuery

	// 1. Cache
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		restrictSR := sub != ""
		posts, subs, _, err := h.redditCli.FetchSearch(r.Context(), query, sub, sort, t, after, restrictSR, 10)
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
					After:      after,
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

	// 4. Nothing available — preserve query string so upstream link keeps the
	// user's search terms.
	h.redirectFuckReddit(w, r, r.URL.RequestURI(), reason)
}

func (h *Handler) backgroundArchiveSearch(query, sub, sort, t, after string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	restrictSR := sub != ""
	var posts []reddit.Post
	var err error

	if h.oauthPool.HasAvailableTokens() {
		posts, _, _, err = h.redditCli.FetchSearch(ctx, query, sub, sort, t, after, restrictSR, 25)
	} else {
		posts, _, _, err = h.publicCli.FetchSearch(ctx, query, sub, sort, t, after, restrictSR, 25)
	}
	h.recordUpstream(ctx)
	if err != nil {
		log.Printf("background archive search %q: %v", query, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "search")
}
