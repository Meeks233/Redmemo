package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/redmemo/redmemo/internal/proxy"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	id := r.PathValue("id")
	prefs := readPreferences(r)
	urlPath := r.URL.Path
	commentSort := r.URL.Query().Get("sort")
	if commentSort == "" {
		commentSort = prefs.CommentSort
	}

	// Level 1: Cache
	cacheKey := urlPath
	if commentSort != "" {
		cacheKey += "?sort=" + commentSort
	}
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}

	// Level 2: Own OAuth (if tokens available, prioritize over redlib)
	triedOAuth := false
	if h.oauthPool.HasAvailableTokens() {
		triedOAuth = true
		if h.renderPostFallback(w, r, sub, id, commentSort, prefs) {
			return
		}
	}

	// Level 3: Redlib proxy
	if h.cfg.Redlib.Enabled && h.ratelimit.CanRequestRedlib() {
		resp, body, err := h.proxy.Forward(r)
		if err == nil && !proxy.IsRateLimited(resp.StatusCode, body) && !proxy.IsServerError(resp.StatusCode, body) {
			h.ratelimit.Increment()
			body = h.rewriteMedia(h.rebrand(body))
			h.cache.PutHTML(r.Context(), cacheKey, body, 5*time.Minute)

			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
		if err == nil && proxy.IsRateLimited(resp.StatusCode, body) {
			h.ratelimit.OnRedlibRateLimited()
			go h.oauthPool.SpawnTokenIfNeeded(context.Background())
		}
	}

	// Level 4: Own OAuth fallback (if not tried above)
	if !triedOAuth && h.ratelimit.CanRequestFallback(r.Context()) {
		if h.renderPostFallback(w, r, sub, id, commentSort, prefs) {
			return
		}
	}

	// Level 5: Archive
	storedPost, _ := h.postStore.Get(urlPath)
	if storedPost != nil {
		h.renderPostFromArchive(w, r, storedPost, prefs, commentSort)
		return
	}

	// Level 6: Error + background spawn
	go h.oauthPool.SpawnTokenIfNeeded(context.Background())
	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests)
}

func (h *Handler) renderPostFallback(w http.ResponseWriter, r *http.Request, sub, id, commentSort string, prefs reddit.Preferences) bool {
	post, comments, err := h.redditCli.FetchPost(r.Context(), sub, id, commentSort)
	if err != nil {
		log.Printf("handler: fallback fetch post %s/%s: %v", sub, id, err)
		return false
	}

	go func() {
		h.archiver.ArchivePost(&post, sub, "oauth_fallback")
		h.archiver.ArchiveComments(post.Permalink, comments)
	}()

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
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderPost(w, data); err != nil {
		log.Printf("handler: render post: %v", err)
	}
	return true
}

func (h *Handler) renderPostFromArchive(w http.ResponseWriter, r *http.Request, sp *store.StoredPost, prefs reddit.Preferences, commentSort string) {
	var post reddit.Post
	if err := json.Unmarshal(sp.JSONData, &post); err != nil {
		h.renderer.RenderError(w, "存档数据解析失败", http.StatusInternalServerError)
		return
	}

	var comments []reddit.Comment
	stored, _ := h.commentStore.GetLatest(sp.URLPath)
	if stored != nil {
		json.Unmarshal(stored.JSONData, &comments)
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
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderPost(w, data); err != nil {
		log.Printf("handler: render post from archive: %v", err)
	}
}
