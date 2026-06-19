package handler

import (
	"bytes"
	"log"
	"net/http"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
)

func (h *Handler) handleUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	listing := r.PathValue("listing")
	prefs := h.readPreferences(r)
	sort := r.URL.Query().Get("sort")
	after := r.URL.Query().Get("after")
	urlPath := r.URL.Path
	cacheKey := htmlCacheKey(urlPath, r.URL.RawQuery, prefs)

	// 1. Cache — keyed by full prefs fingerprint (see handlePost).
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderUserFallback(w, r, name, listing, sort, after, urlPath, prefs, cacheKey) {
			return
		}
	}

	// 3. No archive for users
	h.serveDegradeMiss(w, r, reason)
}

func (h *Handler) renderUserFallback(w http.ResponseWriter, r *http.Request, name, listing, sort, after, urlPath string, prefs reddit.Preferences, cacheKey string) bool {
	user, posts, _, err := h.redditCli.FetchUser(r.Context(), name, listing, sort, after)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: fallback fetch user %s: %v", name, err)
		return false
	}

	go h.archiver.ArchivePosts(posts, "", "user_listing")

	data := render.UserPageData{
		BasePage: render.BasePage{
			URL:       urlPath,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   render.Version,
		},
		User:    user,
		Posts:   posts,
		Listing: listing,
		Sort:    [2]string{sort, r.URL.Query().Get("t")},
		NoPosts: len(posts) == 0,
	}

	var buf bytes.Buffer
	if err := h.renderer.RenderUser(&buf, data); err != nil {
		log.Printf("handler: render user: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Source", "fallback")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
	return true
}
