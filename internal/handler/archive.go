package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) notifyUserRequest() {
	if h.prefetcher != nil {
		h.prefetcher.NotifyUserRequest()
	}
}

func (h *Handler) fetchSubreddit(ctx context.Context, sub, sort, after string, limit int) ([]reddit.Post, string, string, error) {
	if h.oauthPool.HasAvailableTokens() {
		posts, before, after, err := h.redditCli.FetchSubreddit(ctx, sub, sort, after, limit)
		if err == nil {
			h.notifyUserRequest()
			return posts, before, after, nil
		}
	}
	return h.publicCli.FetchSubreddit(ctx, sub, sort, after, limit)
}

func (h *Handler) fetchPost(ctx context.Context, sub, id, commentSort string) (reddit.Post, []reddit.Comment, error) {
	if h.oauthPool.HasAvailableTokens() {
		post, comments, err := h.redditCli.FetchPost(ctx, sub, id, commentSort)
		if err == nil {
			h.notifyUserRequest()
			return post, comments, nil
		}
	}
	return h.publicCli.FetchPost(ctx, sub, id, commentSort)
}

const archiveHubPageSize = 5
const archivePageSize = 25

func (h *Handler) handleArchiveHub(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	var subs []render.ArchiveHubEntry
	if h.postStore != nil {
		recent, err := h.postStore.RecentlyArchivedSubs(archiveHubPageSize+1, offset)
		if err != nil {
			log.Printf("handler: archive hub: %v", err)
		}

		var names []string
		for _, rs := range recent {
			names = append(names, rs.Name)
		}

		var iconMap map[string]*store.SubIcon
		if h.subIconStore != nil && len(names) > 0 {
			iconMap, _ = h.subIconStore.GetIconMap(names)
		}

		for _, rs := range recent {
			entry := render.ArchiveHubEntry{
				Name:      rs.Name,
				PostCount: rs.PostCount,
			}
			if icon, ok := iconMap[rs.Name]; ok {
				entry.IconURL = icon.IconURL
			}
			subs = append(subs, entry)
		}
	}

	hasMore := len(subs) > archiveHubPageSize
	if hasMore {
		subs = subs[:archiveHubPageSize]
	}

	if r.URL.Query().Get("format") == "json" {
		type jsonEntry struct {
			Name      string `json:"name"`
			PostCount int64  `json:"post_count"`
			IconURL   string `json:"icon_url,omitempty"`
		}
		type jsonResp struct {
			Subs    []jsonEntry `json:"subs"`
			Offset  int         `json:"offset"`
			HasMore bool        `json:"has_more"`
		}
		entries := make([]jsonEntry, len(subs))
		for i, s := range subs {
			entries[i] = jsonEntry{
				Name:      s.Name,
				PostCount: s.PostCount,
				IconURL:   s.IconURL,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jsonResp{
			Subs:    entries,
			Offset:  offset + len(subs),
			HasMore: hasMore,
		})
		return
	}

	// Trigger passive L4 icon check
	if h.prefetcher != nil {
		go h.prefetcher.CheckIconsPassive()
	}

	data := render.ArchiveHubPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Subs:    subs,
		Offset:  offset + len(subs),
		HasMore: hasMore,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderArchiveHub(w, data); err != nil {
		log.Printf("handler: render archive hub: %v", err)
	}
}

func (h *Handler) handleArchiveSub(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	if sub == "" || !validSubName.MatchString(sub) {
		http.NotFound(w, r)
		return
	}

	if !h.isArchivableSub(sub) {
		http.NotFound(w, r)
		return
	}

	prefs := h.readPreferences(r)

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}

	total, _ := h.postStore.CountBySubreddit(sub)
	totalPages := int((total + archivePageSize - 1) / archivePageSize)
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * archivePageSize
	stored, err := h.postStore.ListBySubreddit(sub, archivePageSize, offset)
	if err != nil {
		log.Printf("handler: archive list %s: %v", sub, err)
	}

	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			posts = append(posts, p)
		}
	}

	data := render.ArchivePageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sub:                sub,
		Posts:              posts,
		TotalPosts:         total,
		Page:               page,
		TotalPages:         totalPages,
		AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
		HasPrev:            page > 1,
		HasNext:            page < totalPages,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderArchive(w, data); err != nil {
		log.Printf("handler: render archive %s: %v", sub, err)
	}
}

func (h *Handler) isArchivableSub(sub string) bool {
	if h.postStore != nil {
		if count, err := h.postStore.CountBySubreddit(sub); err == nil && count > 0 {
			return true
		}
	}
	if h.settingsStore != nil {
		if v, ok, _ := h.settingsStore.Get("prefetch_subs"); ok && v != "" {
			for _, s := range strings.Split(v, "+") {
				if strings.EqualFold(strings.TrimSpace(s), sub) {
					return true
				}
			}
		}
	}
	return false
}

func (h *Handler) fetchSubredditAbout(ctx context.Context, sub string) (reddit.Subreddit, error) {
	if h.oauthPool.HasAvailableTokens() {
		info, err := h.redditCli.FetchSubredditAbout(ctx, sub)
		if err == nil {
			h.notifyUserRequest()
			return info, nil
		}
	}
	return h.publicCli.FetchSubredditAbout(ctx, sub)
}
