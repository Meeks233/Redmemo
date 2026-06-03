package handler

import (
	"bytes"
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
	t := newReqTimer()
	prefs := h.readPreferences(r)
	urlPath := r.URL.Path
	commentSort := r.URL.Query().Get("sort")
	if commentSort == "" {
		commentSort = prefs.CommentSort
	}

	// 1. Cache — keyed by full prefs fingerprint so a zh visitor never receives
	// the page rendered for an en visitor, nor a theme/NSFW variant they didn't
	// pick. htmlCacheKey appends an FNV tag over every Preferences field.
	rawQuery := ""
	if commentSort != "" {
		rawQuery = "sort=" + commentSort
	}
	cacheKey := htmlCacheKey(urlPath, rawQuery, prefs)
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		t.mark("cache")
		t.writeHeader(w)
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}
	t.mark("cache")

	// 2. HR gate / OAuth quota
	degrade, reason := h.shouldDegrade(r.Context())
	if !degrade {
		if h.renderPostFallback(w, r, sub, id, commentSort, prefs, t, cacheKey) {
			return
		}
	}

	// 3. Archive fallback. offline=true only when upstream actually failed
	// (reason==""); when degraded, only the amber degraded banner shows.
	storedPost, _ := h.postStore.Get(urlPath)
	if storedPost != nil {
		h.renderPostFromArchive(w, r, storedPost, prefs, commentSort, reason == "", reason, t, cacheKey)
		return
	}

	// 4. Nothing available
	h.serveDegradeMiss(w, r, reason)
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
	// HTML cache keys now embed a prefs fingerprint; drop every variant under
	// this URL path in one SCAN rather than enumerating known languages.
	if err := h.cache.InvalidateHTMLPrefix(r.Context(), urlPath); err != nil {
		log.Printf("handler: invalidate html prefix %s: %v", urlPath, err)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

func (h *Handler) renderPostFallback(w http.ResponseWriter, r *http.Request, sub, id, commentSort string, prefs reddit.Preferences, t *reqTimer, cacheKey string) bool {
	// Coalesce concurrent identical post fetches so a viral post under
	// simultaneous load only burns one OAuth quota unit. recordUpstream and
	// the archiver spawn live inside the leader closure so they fire once
	// per real Reddit hit, not once per merged caller.
	type postFetchResult struct {
		post     reddit.Post
		comments []reddit.Comment
		err      error
	}
	flightKey := "post|" + sub + "|" + id + "|" + commentSort
	raw, _, _ := h.upstreamFlight.Do(flightKey, func() (any, error) {
		post, comments, err := h.redditCli.FetchPost(r.Context(), sub, id, commentSort)
		h.recordUpstream(r.Context())
		res := &postFetchResult{post: post, comments: comments, err: err}
		if err != nil {
			return res, nil
		}
		go func() {
			h.archiver.ArchivePost(&post, sub, "oauth_fallback")
			h.archiver.ArchiveComments(post.Permalink, comments)
		}()
		return res, nil
	})
	res := raw.(*postFetchResult)
	t.mark("upstream")
	if res.err != nil {
		log.Printf("handler: fallback fetch post %s/%s: %v", sub, id, res.err)
		return false
	}
	post, comments := res.post, res.comments

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
		HasOAuth:        h.oauthHolder.HasAvailableTokens(),
	}

	var buf bytes.Buffer
	if err := h.renderer.RenderPost(&buf, data); err != nil {
		log.Printf("handler: render post: %v", err)
	}
	t.mark("render")

	t.writeHeader(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
	return true
}

func (h *Handler) renderPostFromArchive(w http.ResponseWriter, r *http.Request, sp *store.StoredPost, prefs reddit.Preferences, commentSort string, offline bool, degradedReason string, t *reqTimer, cacheKey string) {
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
	t.mark("archive-decode")

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
		HasOAuth:        h.oauthHolder.HasAvailableTokens(),
		IsOffline:       offline,
	}

	var buf bytes.Buffer
	if err := h.renderer.RenderPost(&buf, data); err != nil {
		log.Printf("handler: render post from archive: %v", err)
	}
	t.mark("render")

	t.writeHeader(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
}
