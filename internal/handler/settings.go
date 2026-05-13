package handler

import (
	"fmt"
	"log"
	"net/http"

	"github.com/redmemo/redmemo/internal/render"
)

const cookieMaxAge = 52 * 7 * 24 * 60 * 60 // 52 weeks in seconds

var settingsKeys = []string{
	"theme", "front_page", "front_page_subs", "front_page_subs_mode", "layout", "wide",
	"blur_spoiler", "show_nsfw", "blur_nsfw",
	"hide_hls_notification", "video_quality",
	"hide_sidebar_and_summary", "use_hls",
	"autoplay_videos", "fixed_navbar",
	"disable_visit_reddit_confirmation",
	"comment_sort", "post_sort",
	"hide_awards", "hide_score", "remove_default_feeds",
	"enable_debug", "enable_natural_prefetch", "prefetch_subs",
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	var postCount, subCount int64
	var subStats []render.SubredditStatView
	if h.postStore != nil {
		postCount, _ = h.postStore.Count()
		subCount, _ = h.postStore.SubredditCount()
		if stats, err := h.postStore.SubredditStats(10, 20); err == nil {
			for _, s := range stats {
				subStats = append(subStats, render.SubredditStatView{
					Name:      s.Name,
					PostCount: s.PostCount,
				})
			}
		}
	}

	var mediaCount, mediaSize int64
	if h.mediaStore != nil {
		mediaCount, mediaSize, _ = h.mediaStore.Stats()
	}

	var prefetchSubs []string
	for _, ps := range h.cfg.Prefetch.Subreddits {
		prefetchSubs = append(prefetchSubs, ps.Name)
	}

	var archivedSubs []string
	if h.postStore != nil {
		archivedSubs, _ = h.postStore.DistinctSubreddits()
	}

	data := render.SettingsPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		PostCount:      postCount,
		SubredditCount: subCount,
		MediaCount:     mediaCount,
		MediaSize:      formatBytes(mediaSize),
		OAuthEnabled:   len(h.cfg.OAuth.Tokens) > 0,
		PrefetchSubs:   prefetchSubs,
		SubredditStats: subStats,
		ArchivedSubs:   archivedSubs,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderer.RenderSettings(w, data)
}

func (h *Handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	updates := make(map[string]string)
	for _, key := range settingsKeys {
		if r.Form.Has(key) {
			updates[key] = r.FormValue(key)
		}
	}

	if cb := r.FormValue("show_all_subs"); cb == "on" {
		updates["front_page_subs"] = "all"
		updates["front_page_subs_mode"] = "whitelist"
	}

	if mode, ok := updates["front_page_subs_mode"]; ok && mode != "whitelist" && mode != "blacklist" {
		updates["front_page_subs_mode"] = "whitelist"
	}

	if len(updates) > 0 {
		if h.settingsStore != nil {
			if err := h.settingsStore.SetBatch(updates, "user"); err != nil {
				log.Printf("[settings] failed to save: %v", err)
				http.Error(w, "failed to save settings", http.StatusInternalServerError)
				return
			}
		}
		for k, v := range updates {
			h.siteDefaults[k] = v
		}
	}

	if theme := updates["theme"]; theme != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     "theme",
			Value:    theme,
			Path:     "/",
			MaxAge:   cookieMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
