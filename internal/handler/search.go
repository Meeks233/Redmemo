package handler

import (
	"encoding/json"
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

	// Level 1: Cache
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// Level 2: Redlib proxy
	if h.cfg.Redlib.Enabled && h.ratelimit.CanRequestRedlib() {
		resp, body, err := h.proxy.Forward(r)
		if err == nil && !proxy.IsRateLimited(resp.StatusCode, body) && !proxy.IsServerError(resp.StatusCode, body) {
			h.ratelimit.Increment()
			body = h.rebrand(body)
			h.cache.PutHTML(r.Context(), cacheKey, body, 3*time.Minute)

			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
		if err == nil && proxy.IsRateLimited(resp.StatusCode, body) {
			h.ratelimit.OnRedlibRateLimited()
		}
	}

	// Level 3: Own OAuth fallback
	if h.ratelimit.CanRequestFallback(r.Context()) {
		restrictSR := sub != ""
		posts, subs, _, err := h.redditCli.FetchSearch(r.Context(), query, sub, sort, t, after, restrictSR)
		if err != nil {
			log.Printf("handler: fallback search %q: %v", query, err)
			h.renderer.RenderError(w, "搜索失败: "+err.Error(), http.StatusBadGateway)
			return
		}

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

	// Level 4: Archive search
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
				Sub:     sub,
				NoPosts: len(posts) == 0,
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Source", "archive")
			if err := h.renderer.RenderSearch(w, data); err != nil {
				log.Printf("handler: render search from archive: %v", err)
			}
			return
		}
	}

	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests)
}
