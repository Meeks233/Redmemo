package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/redmemo/redmemo/internal/proxy"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) handleFrontPage(w http.ResponseWriter, r *http.Request) {
	prefs := readPreferences(r)
	frontPage := prefs.FrontPage
	if frontPage == "" {
		frontPage = "popular"
	}
	h.serveSubreddit(w, r, frontPage, prefs.PostSort, prefs)
}

func (h *Handler) handleSubreddit(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := readPreferences(r)
	h.serveSubreddit(w, r, sub, prefs.PostSort, prefs)
}

func (h *Handler) handleSubredditSort(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	sort := r.PathValue("sort")
	prefs := readPreferences(r)
	h.serveSubreddit(w, r, sub, sort, prefs)
}

func (h *Handler) serveSubreddit(w http.ResponseWriter, r *http.Request, sub, sort string, prefs reddit.Preferences) {
	urlPath := r.URL.Path
	after := r.URL.Query().Get("after")

	// Level 1: Cache
	if cached, _ := h.cache.GetHTML(r.Context(), urlPath+"?after="+after); cached != nil {
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
			h.cache.PutHTML(r.Context(), urlPath+"?after="+after, body, 5*time.Minute)

			if h.cfg.RateLimit.ArchiveOnProxy {
				go h.archiveSubreddit(sub, sort)
			}

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
		h.renderSubredditFallback(w, r, sub, sort, after, prefs)
		return
	}

	// Level 4: Archive
	posts, _ := h.postStore.ListBySubreddit(sub, 25, 0)
	if len(posts) > 0 {
		h.renderSubredditFromArchive(w, r, sub, posts, prefs)
		return
	}

	// Level 5: Rate limit page
	h.renderer.RenderError(w, "所有上游均已限流，请稍后再试", http.StatusTooManyRequests)
}

func (h *Handler) renderSubredditFallback(w http.ResponseWriter, r *http.Request, sub, sort, after string, prefs reddit.Preferences) {
	if sort == "" {
		sort = "hot"
	}

	posts, before, afterCursor, err := h.redditCli.FetchSubreddit(r.Context(), sub, sort, after, 25)
	if err != nil {
		log.Printf("handler: fallback fetch subreddit %s: %v", sub, err)
		h.renderer.RenderError(w, "获取子版块失败: "+err.Error(), http.StatusBadGateway)
		return
	}

	subInfo, _ := h.redditCli.FetchSubredditAbout(r.Context(), sub)

	go h.archiveSubredditPosts(posts, sub)

	data := render.SubredditPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sub:     subInfo,
		Posts:   posts,
		Sort:    [2]string{sort, r.URL.Query().Get("t")},
		Ends:    [2]string{before, afterCursor},
		NoPosts: len(posts) == 0,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit: %v", err)
	}
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
		Posts:   posts,
		NoPosts: len(posts) == 0,
	}
	data.Sub.Name = sub

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	if err := h.renderer.RenderSubreddit(w, data); err != nil {
		log.Printf("handler: render subreddit from archive: %v", err)
	}
}

func (h *Handler) archiveSubreddit(sub, sort string) {
	if h.redditCli == nil {
		return
	}
	posts, _, _, err := h.redditCli.FetchSubreddit(nil, sub, sort, "", 25)
	if err != nil {
		return
	}
	h.archiveSubredditPosts(posts, sub)
}

func (h *Handler) archiveSubredditPosts(posts []reddit.Post, sub string) {
	for i := range posts {
		data, err := json.Marshal(posts[i])
		if err != nil {
			continue
		}
		urlPath := posts[i].Permalink
		if urlPath == "" {
			continue
		}
		score := 0
		if posts[i].Score[1] != "" {
			json.Unmarshal([]byte(posts[i].Score[1]), &score)
		}
		h.postStore.Save(&store.StoredPost{
			URLPath:    urlPath,
			Subreddit:  sub,
			PostID:     posts[i].ID,
			Title:      posts[i].Title,
			JSONData:   data,
			Author:     posts[i].Author.Name,
			Score:      score,
			CreatedUTC: time.Unix(int64(posts[i].CreatedTS), 0),
			Source:     "redlib_proxy",
		})
	}
}
