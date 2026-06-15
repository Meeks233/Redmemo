package handler

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"time"

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

// backgroundArchiveUser fetches a user's listing out-of-band and archives the
// posts. It owns its own context (decoupled from the request) and never touches
// the HTTP response, so it is safe to launch as a goroutine to keep growing the
// archive on the authenticated path.
//
// It passes through the same global degrade gate as live traffic: when HR is in
// cooldown or OAuth quota is exhausted it stands down instead of fetching. There
// is deliberately no publicCli fallback — this work consumes quota and emitting
// an unauthenticated request from the session IP is a stealth tell we won't
// accept (see prefetch scheduler's "No fallback" note). The public client is
// reserved for media resources only.
func (h *Handler) backgroundArchiveUser(name, listing, sort, after string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if degrade, _ := h.shouldDegrade(ctx); degrade {
		return
	}

	_, posts, _, err := h.redditCli.FetchUser(ctx, name, listing, sort, after)
	h.recordUpstream(ctx)
	if err != nil {
		log.Printf("background archive user %s: %v", name, err)
		return
	}
	if len(posts) > 0 {
		h.archiver.ArchivePosts(posts, "", "user_listing")
	}
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
