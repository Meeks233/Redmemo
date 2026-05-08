package handler

import (
	"log"
	"net/http"
	"time"

	"github.com/redmemo/redmemo/internal/proxy"
	"github.com/redmemo/redmemo/internal/render"
)

func (h *Handler) handleUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	listing := r.PathValue("listing")
	prefs := readPreferences(r)
	sort := r.URL.Query().Get("sort")
	after := r.URL.Query().Get("after")
	urlPath := r.URL.Path

	// Level 1: Cache
	cacheKey := urlPath + "?" + r.URL.RawQuery
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
			h.cache.PutHTML(r.Context(), cacheKey, body, 5*time.Minute)

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
		user, posts, _, err := h.redditCli.FetchUser(r.Context(), name, listing, sort, after)
		if err != nil {
			log.Printf("handler: fallback fetch user %s: %v", name, err)
			h.renderer.RenderError(w, "获取用户页失败: "+err.Error(), http.StatusBadGateway)
			return
		}

		data := render.UserPageData{
			BasePage: render.BasePage{
				URL:       urlPath,
				Prefs:     prefs,
				BrandName: h.cfg.Render.BrandName,
				Version:   "0.1.0",
			},
			User:    user,
			Posts:   posts,
			Listing: listing,
			Sort:    [2]string{sort, r.URL.Query().Get("t")},
			NoPosts: len(posts) == 0,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Source", "fallback")
		if err := h.renderer.RenderUser(w, data); err != nil {
			log.Printf("handler: render user: %v", err)
		}
		return
	}

	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests)
}
