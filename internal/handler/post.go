package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	h.servePost(w, r, r.PathValue("sub"), r.PathValue("id"))
}

func (h *Handler) handleUserPost(w http.ResponseWriter, r *http.Request) {
	h.servePost(w, r, "u_"+r.PathValue("name"), r.PathValue("id"))
}

func (h *Handler) servePost(w http.ResponseWriter, r *http.Request, sub, id string) {
	prefs := h.readPreferences(r)
	urlPath := r.URL.Path
	commentSort := r.URL.Query().Get("sort")
	if commentSort == "" {
		commentSort = prefs.CommentSort
	}

	// 1. Cache — keyed by UI language so a zh visitor never receives the
	// cached zh-neutral page rendered for an en visitor (and vice versa).
	cacheKey := prefs.Lang + ":" + urlPath
	if commentSort != "" {
		cacheKey += "?sort=" + commentSort
	}
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// 2. HR gate / OAuth quota
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderPostFallback(w, r, sub, id, commentSort, prefs) {
			return
		}
	}

	// 3. Archive fallback. offline=true only when upstream actually failed
	// (reason==""); when degraded, only the amber degraded banner shows.
	storedPost, _ := h.postStore.Get(urlPath)
	if storedPost != nil {
		h.renderPostFromArchive(w, r, storedPost, prefs, commentSort, reason == "", reason)
		return
	}

	// 4. Nothing available
	h.redirectFuckReddit(w, r, r.URL.Path, reason)
}

func (h *Handler) backgroundArchivePost(sub, id, urlPath, commentSort string, htmlSnapshot []byte) {
	existing, _ := h.postStore.Get(urlPath)
	if existing != nil && time.Since(existing.LastUpdated) < 10*time.Minute {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	post, comments, err := h.fetchPost(ctx, sub, id, commentSort)
	if err != nil {
		log.Printf("background archive post %s/%s: %v", sub, id, err)
		return
	}
	h.archiver.ArchivePost(&post, sub, "background")
	h.archiver.ArchiveComments(post.Permalink, comments)

	if len(htmlSnapshot) > 0 {
		permalink := post.Permalink
		if permalink == "" {
			permalink = urlPath
		}
		if err := h.postStore.SaveHTML(permalink, htmlSnapshot); err != nil {
			log.Printf("background save html %s: %v", permalink, err)
		}
	}
}

func (h *Handler) handleRefreshPost(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	id := r.PathValue("id")
	commentSort := r.URL.Query().Get("sort")

	// User-triggered refresh is HR-gated like any foreground request.
	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Reason", reason)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"ok":false,"error":"degraded","reason":"%s"}`, reason)
		return
	}

	post, comments, err := h.redditCli.FetchPost(r.Context(), sub, id, commentSort)
	h.recordUpstream(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"ok":false,"error":"fetch failed: %s"}`, err.Error())
		return
	}

	go func() {
		h.archiver.ArchivePost(&post, sub, "manual_refresh")
		h.archiver.ArchiveComments(post.Permalink, comments)
	}()

	urlPath := "/r/" + sub + "/comments/" + id
	// HTML cache entries are language-prefixed; drop every language variant.
	for _, lang := range render.SupportedLangs {
		h.cache.InvalidateHTML(r.Context(), lang+":"+urlPath)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

func (h *Handler) renderPostFallback(w http.ResponseWriter, r *http.Request, sub, id, commentSort string, prefs reddit.Preferences) bool {
	post, comments, err := h.redditCli.FetchPost(r.Context(), sub, id, commentSort)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: fallback fetch post %s/%s: %v", sub, id, err)
		return false
	}

	go func() {
		h.archiver.ArchivePost(&post, sub, "oauth_fallback")
		h.archiver.ArchiveComments(post.Permalink, comments)
	}()

	if sp, _ := h.postStore.Get(post.Permalink); sp != nil {
		post.ArchivedRelTime, post.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
	}

	data := render.PostPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Post:            post,
		Comments:        comments,
		Sort:            commentSort,
		URLWithoutQuery: r.URL.Path,
		HasOAuth:        h.oauthPool.HasAvailableTokens(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderPost(w, data); err != nil {
		log.Printf("handler: render post: %v", err)
	}
	return true
}

func (h *Handler) renderPostFromArchive(w http.ResponseWriter, r *http.Request, sp *store.StoredPost, prefs reddit.Preferences, commentSort string, offline bool, degradedReason string) {
	var post reddit.Post
	if err := json.Unmarshal(sp.JSONData, &post); err != nil {
		h.renderer.RenderError(w, prefs.Lang, "存档数据解析失败", http.StatusInternalServerError)
		return
	}
	post.ArchivedRelTime, post.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))

	var comments []reddit.Comment
	stored, _ := h.commentStore.GetLatest(sp.URLPath)
	if stored != nil {
		json.Unmarshal(stored.JSONData, &comments)
	}

	data := render.PostPageData{
		BasePage: render.BasePage{
			URL:            r.URL.Path,
			Prefs:          prefs,
			BrandName:      h.cfg.Render.BrandName,
			Version:        "0.1.0",
			DegradedReason: degradedReason,
		},
		Post:            post,
		Comments:        comments,
		Sort:            commentSort,
		URLWithoutQuery: r.URL.Path,
		HasOAuth:        h.oauthPool.HasAvailableTokens(),
		IsOffline:       offline,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderPost(w, data); err != nil {
		log.Printf("handler: render post from archive: %v", err)
	}
}
