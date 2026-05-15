package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

var partialLastReq sync.Map

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
			p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
			posts = append(posts, p)
		}
	}

	if r.URL.Query().Get("partial") == "1" {
		interval := 2
		if n, err := strconv.Atoi(prefs.ScrollInterval); err == nil && n > 0 {
			interval = n
		}
		ip := r.RemoteAddr
		now := time.Now()
		if last, ok := partialLastReq.Load(ip); ok {
			if now.Sub(last.(time.Time)) < time.Duration(interval)*time.Second {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
		partialLastReq.Store(ip, now)
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

	// 1. Cache
	if cached, _ := h.cache.GetHTML(r.Context(), urlPath+"?after="+after); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota. On degrade, skip upstream and fall through.
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderSubredditFallback(w, r, sub, sort, after, prefs, limit) {
			return
		}
	}

	// 3. Archive fallback. Distinguish "truly offline" (upstream failed,
	// reason==""→show offline banner) from "deliberately degraded" (HR /
	// quota, reason!=""→show only degraded banner, not the offline one).
	posts, _ := h.postStore.ListBySubreddit(sub, limit, 0)
	if len(posts) > 0 {
		h.renderSubredditFromArchive(w, r, sub, posts, prefs, reason == "", reason)
		return
	}

	// 4. Nothing available
	h.redirectFuckReddit(w, r, r.URL.Path, reason)
}

func (h *Handler) backgroundArchiveSubreddit(sub, sort, after string) {
	existing, _ := h.postStore.ListBySubreddit(sub, 1, 0)
	if len(existing) > 0 && time.Since(existing[0].LastUpdated) < 10*time.Minute {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	posts, _, _, err := h.fetchSubreddit(ctx, sub, sort, after, 5)
	if err != nil {
		log.Printf("background archive sub %s: %v", sub, err)
		return
	}
	h.archiver.ArchivePosts(posts, sub, "background")

	// About is never fetched from background paths — only on active user
	// visits (see fetchSubredditAbout). Read whatever is cached and persist
	// it to the subreddit archive if available.
	subInfo, _ := h.fetchSubredditAbout(ctx, sub, false)
	if subInfo.Name != "" {
		h.archiver.ArchiveSubreddit(&subInfo)
	}
}

func (h *Handler) renderSubredditFallback(w http.ResponseWriter, r *http.Request, sub, sort, after string, prefs reddit.Preferences, limit int) bool {
	if sort == "" {
		sort = "hot"
	}

	posts, before, afterCursor, err := h.redditCli.FetchSubreddit(r.Context(), sub, sort, after, limit)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: fallback fetch subreddit %s: %v", sub, err)
		if h.subStatusStore != nil {
			h.subStatusStore.RecordFailure(sub, err.Error())
		}
		return false
	}

	// Active visit: cached if fresh, else fetch + persist (60-day TTL).
	// Gated by the fetch_sub_about preference — when off (default), the HR
	// layer is cache-only and never triggers an upstream about request.
	// The background icon/about prefetch path (internal/prefetch/icon.go)
	// is independent of this setting.
	activeAbout := prefs.FetchSubAbout == "on"
	subInfo, _ := h.fetchSubredditAbout(r.Context(), sub, activeAbout)

	go func() {
		h.archiver.ArchivePosts(posts, sub, "oauth_fallback")
		if subInfo.Name != "" {
			h.archiver.ArchiveSubreddit(&subInfo)
		}
		if h.subStatusStore != nil {
			h.subStatusStore.MarkLive(sub)
		}
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

func (h *Handler) renderSubredditFromArchive(w http.ResponseWriter, r *http.Request, sub string, stored []*store.StoredPost, prefs reddit.Preferences, offline bool, degradedReason string) {
	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
			posts = append(posts, p)
		}
	}

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:            r.URL.Path,
			Prefs:          prefs,
			BrandName:      h.cfg.Render.BrandName,
			Version:        "0.1.0",
			DegradedReason: degradedReason,
		},
		Posts:     posts,
		NoPosts:   len(posts) == 0,
		HasOAuth:  h.oauthPool.HasAvailableTokens(),
		IsOffline: offline,
	}
	data.Sub.Name = sub

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit from archive: %v", err)
	}
}
