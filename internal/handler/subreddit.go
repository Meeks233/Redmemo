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
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) handleFrontPage(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "new"
	}

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	var subs []string
	mode := prefs.FrontPageSubsMode
	if prefs.FrontPageSubs != "" && prefs.FrontPageSubs != "all" {
		for _, s := range strings.Split(prefs.FrontPageSubs, "+") {
			s = strings.TrimSpace(s)
			if s != "" {
				subs = append(subs, s)
			}
		}
	}

	const limit = 5
	stored, err := h.postStore.ListHomepage(sort, limit, offset, subs, mode)
	if err != nil {
		log.Printf("handler: homepage db query (%s): %v", sort, err)
	}

	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			posts = append(posts, p)
		}
	}

	if r.URL.Query().Get("partial") == "1" {
		h.renderHomepagePartial(w, posts, prefs)
		return
	}

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Posts:              posts,
		HomepageSort:       sort,
		NoPosts:            len(posts) == 0,
		AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
		HasOAuth:           h.oauthPool.HasAvailableTokens(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render homepage: %v", err)
	}
}

func (h *Handler) renderHomepagePartial(w http.ResponseWriter, posts []reddit.Post, prefs reddit.Preferences) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if len(posts) == 0 {
		return
	}
	if err := h.renderer.RenderPostList(w, posts, prefs); err != nil {
		log.Printf("handler: render homepage partial: %v", err)
	}
}

func (h *Handler) handleSubreddit(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := h.readPreferences(r)
	h.serveSubreddit(w, r, sub, prefs.PostSort, prefs, 25)
}

func (h *Handler) handleSubredditSort(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	sort := r.PathValue("sort")
	prefs := h.readPreferences(r)
	h.serveSubreddit(w, r, sub, sort, prefs, 25)
}

func (h *Handler) serveSubreddit(w http.ResponseWriter, r *http.Request, sub, sort string, prefs reddit.Preferences, limit int) {
	urlPath := r.URL.Path
	after := r.URL.Query().Get("after")
	var diag []string

	// Level 1: Cache
	if cached, _ := h.cache.GetHTML(r.Context(), urlPath+"?after="+after); cached != nil {
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
		if h.renderSubredditFallback(w, r, sub, sort, after, prefs, limit) {
			return
		}
		diag = append(diag, "L2 OAuth: fetch failed")
	} else {
		diag = append(diag, "L2 OAuth: no tokens available")
	}

	// Level 3: Own OAuth fallback (if not tried above)
	if !triedOAuth {
		if !h.ratelimit.CanRequestFallback(r.Context()) {
			diag = append(diag, "L3 OAuth fallback: rate limited locally")
		} else if h.renderSubredditFallback(w, r, sub, sort, after, prefs, limit) {
			return
		} else {
			diag = append(diag, "L3 OAuth fallback: fetch failed")
		}
	} else {
		diag = append(diag, "L3 OAuth fallback: skipped (already tried at L2)")
	}

	// Level 4: Archive
	posts, _ := h.postStore.ListBySubreddit(sub, limit, 0)
	if len(posts) > 0 {
		h.renderSubredditFromArchive(w, r, sub, posts, prefs)
		return
	}
	diag = append(diag, "L4 Archive: no archived posts for r/"+sub)

	// Level 5: Error page
	go h.oauthPool.SpawnTokenIfNeeded(context.Background())
	log.Printf("handler: all levels failed for /r/%s: %v", sub, diag)
	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests, diag...)
}

func (h *Handler) backgroundArchiveSubreddit(sub, sort, after string) {
	existing, _ := h.postStore.ListBySubreddit(sub, 1, 0)
	if len(existing) > 0 && time.Since(existing[0].LastUpdated) < 10*time.Minute {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	posts, _, _, err := h.fetchSubreddit(ctx, sub, sort, after, 25)
	if err != nil {
		log.Printf("background archive sub %s: %v", sub, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "background")

	subInfo, err := h.fetchSubredditAbout(ctx, sub)
	if err == nil {
		h.archiver.ArchiveSubreddit(&subInfo)
	}
}

func (h *Handler) renderSubredditFallback(w http.ResponseWriter, r *http.Request, sub, sort, after string, prefs reddit.Preferences, limit int) bool {
	if sort == "" {
		sort = "hot"
	}

	posts, before, afterCursor, err := h.redditCli.FetchSubreddit(r.Context(), sub, sort, after, limit)
	if err != nil {
		log.Printf("handler: fallback fetch subreddit %s: %v", sub, err)
		return false
	}

	subInfo, _ := h.redditCli.FetchSubredditAbout(r.Context(), sub)

	go func() {
		h.archiver.ArchivePosts(posts, sub, "oauth_fallback")
		h.archiver.ArchiveSubreddit(&subInfo)
	}()

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sub:                subInfo,
		Posts:              posts,
		Sort:               [2]string{sort, r.URL.Query().Get("t")},
		Ends:               [2]string{before, afterCursor},
		NoPosts:            len(posts) == 0,
		AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
		HasOAuth:           h.oauthPool.HasAvailableTokens(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit: %v", err)
	}
	return true
}

func (h *Handler) renderSubredditFromArchive(w http.ResponseWriter, r *http.Request, sub string, stored []*store.StoredPost, prefs reddit.Preferences) {
	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			posts = append(posts, p)
		}
	}

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Posts:    posts,
		NoPosts:  len(posts) == 0,
		HasOAuth: h.oauthPool.HasAvailableTokens(),
	}
	data.Sub.Name = sub

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit from archive: %v", err)
	}
}
