package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redmemo/redmemo/internal/proxy"
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
	prefs := readPreferences(r)
	query := r.URL.Query().Get("q")
	sort := r.URL.Query().Get("sort")
	t := r.URL.Query().Get("t")
	after := r.URL.Query().Get("after")
	urlPath := r.URL.Path

	cacheKey := urlPath + "?" + r.URL.RawQuery
	var diag []string

	// Level 1: Cache
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}
	diag = append(diag, "L1 Cache: MISS")

	// Level 2: Own OAuth (if tokens available, prioritize over redlib)
	triedOAuth := false
	if h.oauthPool.HasAvailableTokens() {
		triedOAuth = true
		restrictSR := sub != ""
		posts, subs, _, err := h.redditCli.FetchSearch(r.Context(), query, sub, sort, t, after, restrictSR, 10)
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
		diag = append(diag, fmt.Sprintf("L2 OAuth: %v", err))
		log.Printf("handler: fallback search %q: %v", query, err)
	} else {
		diag = append(diag, "L2 OAuth: no tokens available")
	}

	// Level 3: Redlib proxy
	if !h.cfg.Redlib.Enabled {
		diag = append(diag, "L3 Redlib: disabled in config")
	} else if !h.ratelimit.CanRequestRedlib() {
		diag = append(diag, "L3 Redlib: rate limited locally")
	} else {
		resp, body, err := h.proxy.Forward(r)
		if err != nil {
			diag = append(diag, fmt.Sprintf("L3 Redlib: proxy error: %v", err))
		} else if proxy.IsRateLimited(resp.StatusCode, body) {
			diag = append(diag, fmt.Sprintf("L3 Redlib: rate limited (HTTP %d)", resp.StatusCode))
			h.ratelimit.OnRedlibRateLimited()
			go h.oauthPool.SpawnTokenIfNeeded(context.Background())
		} else if proxy.IsServerError(resp.StatusCode, body) {
			diag = append(diag, fmt.Sprintf("L3 Redlib: server error (HTTP %d)", resp.StatusCode))
		} else {
			h.ratelimit.Increment()
			body = h.rewriteMedia(h.rebrand(body))
			h.cache.PutHTML(r.Context(), cacheKey, body, 3*time.Minute)

			go h.backgroundArchiveSearch(query, sub, sort, t, after)

			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
	}

	// Level 4: Own OAuth fallback (if not tried above)
	if !triedOAuth {
		if !h.ratelimit.CanRequestFallback(r.Context()) {
			diag = append(diag, "L4 OAuth fallback: rate limited locally")
		} else {
			restrictSR := sub != ""
			posts, subs, _, err := h.redditCli.FetchSearch(r.Context(), query, sub, sort, t, after, restrictSR, 10)
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
			diag = append(diag, fmt.Sprintf("L4 OAuth fallback: %v", err))
			log.Printf("handler: fallback search %q: %v", query, err)
		}
	} else {
		diag = append(diag, "L4 OAuth fallback: skipped (already tried at L2)")
	}

	// Level 5: Archive search (offline fallback)
	if query != "" {
		stored, _ := h.postStore.Search(query, 25)
		if len(stored) > 0 {
			var posts []reddit.Post
			for _, sp := range stored {
				var p reddit.Post
				if err := json.Unmarshal(sp.JSONData, &p); err == nil {
					posts = append(posts, p)
				}
			}

			data := render.SearchPageData{
				BasePage: render.BasePage{
					URL:       urlPath,
					Prefs:     prefs,
					BrandName: h.cfg.Render.BrandName,
					Version:   "0.1.0",
				},
				Posts: posts,
				Params: reddit.SearchParams{
					Query: query,
					Sort:  sort,
				},
				Sub:                sub,
				NoPosts:            len(posts) == 0,
				AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Source", "archive")
			if err := h.renderer.RenderSearch(w, data); err != nil {
				log.Printf("handler: render search from archive: %v", err)
			}
			return
		}
		diag = append(diag, fmt.Sprintf("L5 Archive: no results for %q", query))
	} else {
		diag = append(diag, "L5 Archive: empty query")
	}

	// Level 6: Error + background spawn
	go h.oauthPool.SpawnTokenIfNeeded(context.Background())
	log.Printf("handler: all levels failed for search %q: %v", query, diag)
	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests, diag...)
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
	if err != nil {
		log.Printf("background archive search %q: %v", query, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "search")
}
