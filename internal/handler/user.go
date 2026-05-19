package handler

import (
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

	// 1. Cache — language-prefixed, see handlePost.
	cacheKey := prefs.Lang + ":" + urlPath + "?" + r.URL.RawQuery
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderUserFallback(w, r, name, listing, sort, after, urlPath, prefs) {
			return
		}
	}

	// 3. No archive for users — redirect
	h.redirectFuckReddit(w, r, r.URL.Path, reason)
}

func (h *Handler) backgroundArchiveUser(name, listing, sort, after string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var posts []reddit.Post
	var err error

	if h.oauthHolder.HasAvailableTokens() {
		_, posts, _, err = h.redditCli.FetchUser(ctx, name, listing, sort, after)
	} else {
		_, posts, _, err = h.publicCli.FetchUser(ctx, name, listing, sort, after)
	}
	h.recordUpstream(ctx)
	if err != nil {
		log.Printf("background archive user %s: %v", name, err)
		return
	}
	h.archiver.ArchivePosts(posts, "", "user_listing")
}

func (h *Handler) renderUserFallback(w http.ResponseWriter, r *http.Request, name, listing, sort, after, urlPath string, prefs reddit.Preferences) bool {
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
			Version:   "0.1.0",
		},
		User:               user,
		Posts:              posts,
		Listing:            listing,
		Sort:               [2]string{sort, r.URL.Query().Get("t")},
		NoPosts:            len(posts) == 0,
		AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderUser(w, data); err != nil {
		log.Printf("handler: render user: %v", err)
	}
	return true
}
