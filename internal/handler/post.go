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
	var diag []string
	diag = append(diag, "L1 Cache: MISS")

	// Level 2: Own OAuth (if tokens available, prioritize over redlib)
	triedOAuth := false
	if h.oauthPool.HasAvailableTokens() {
		triedOAuth = true
		if h.renderPostFallback(w, r, sub, id, commentSort, prefs) {
			return
		}
		diag = append(diag, "L2 OAuth: fetch failed")
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
			h.cache.PutHTML(r.Context(), cacheKey, body, 5*time.Minute)

			go h.backgroundArchivePost(sub, id, urlPath, commentSort, body)

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
		} else if h.renderPostFallback(w, r, sub, id, commentSort, prefs) {
			return
		} else {
			diag = append(diag, "L4 OAuth fallback: fetch failed")
		}
	} else {
		diag = append(diag, "L4 OAuth fallback: skipped (already tried at L2)")
	}

	// Level 5: Archive
	storedPost, _ := h.postStore.Get(urlPath)
	if storedPost != nil {
		h.renderPostFromArchive(w, r, storedPost, prefs, commentSort)
		return
	}
	diag = append(diag, "L5 Archive: no archived post for "+urlPath)

	// Level 6: Error + background spawn
	go h.oauthPool.SpawnTokenIfNeeded(context.Background())
	log.Printf("handler: all levels failed for %s: %v", urlPath, diag)
	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests, diag...)
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
