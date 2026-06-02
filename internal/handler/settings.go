package handler

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/searchquery"
)

const cookieMaxAge = 52 * 7 * 24 * 60 * 60 // 52 weeks in seconds

var settingsKeys = []string{
	"theme", "lang", "front_page_subs", "layout", "wide",
	"blur_spoiler", "show_nsfw", "show_local_nsfw_subs", "blur_nsfw",
	"hide_sidebar_and_summary",
	"autoplay_videos", "fixed_navbar",
	"disable_visit_reddit_confirmation",
	"comment_sort", "post_sort",
	"hide_awards", "hide_score", "remove_default_feeds",
	"fetch_sub_about",
	"enable_debug", "enable_natural_prefetch", "prefetch_subs",
	"prefetch_threshold", "scroll_interval", "lazy_media",
	"video_quality", "mute_all_videos", "mute_nsfw_videos",
	"auto_theme_day", "auto_theme_night",
	"disable_initiative_upstream_access",
	"settings_token_ttl",
}

// allowedSettingsTokenTTL is the whitelist of valid /settings auth-cookie
// lifetimes in minutes. Anything outside this set is dropped on save and the
// stored default ("10") stands. Capped at 60 by design — longer-lived ephemeral
// tokens defeat the lockout/TOTP gate's threat model.
var allowedSettingsTokenTTL = map[string]bool{
	"5": true, "10": true, "15": true, "30": true, "60": true,
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	var postCount, subCount int64
	var subStats []render.SubredditStatView
	if h.postStore != nil {
		postCount, _ = h.postStore.Count()
		subCount, _ = h.postStore.SubredditCount()
		if stats, err := h.postStore.SubredditStats(10, 10); err == nil {
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

	// prefetch_subs holds a query in the unified search grammar; the configured
	// subs are its sub: includes. Used only to surface per-sub stats below.
	var prefetchSubs []string
	if h.settingsStore != nil {
		if v, ok, _ := h.settingsStore.Get("prefetch_subs"); ok && v != "" {
			prefetchSubs = searchquery.Parse(v).WhiteSubs
		}
	}

	if h.postStore != nil && len(prefetchSubs) > 0 {
		statSet := make(map[string]bool, len(subStats))
		for _, s := range subStats {
			statSet[s.Name] = true
		}
		for _, name := range prefetchSubs {
			if !statSet[name] {
				cnt, _ := h.postStore.CountBySubreddit(name, false)
				subStats = append(subStats, render.SubredditStatView{
					Name:      name,
					PostCount: cnt,
				})
			}
		}
	}

	var archivedSubs []string
	if h.postStore != nil {
		archivedSubs, _ = h.postStore.DistinctSubreddits()
	}

	var liveSubs []string
	if h.subStatusStore != nil {
		liveSubs, _ = h.subStatusStore.ListLive()
	}

	selectedCounts := make(map[string]int)
	var selectedNames []string
	if prefs.FrontPageSubs != "" && prefs.FrontPageSubs != "all" {
		fp := searchquery.Parse(prefs.FrontPageSubs)
		selectedNames = append(selectedNames, fp.WhiteSubs...)
		selectedNames = append(selectedNames, fp.BlackSubs...)
	}
	selectedNames = append(selectedNames, prefetchSubs...)
	for _, n := range selectedNames {
		selectedCounts[n] = 0
	}
	if h.postStore != nil && len(selectedNames) > 0 {
		if counts, err := h.postStore.SubredditCounts(selectedNames); err == nil {
			for k, v := range counts {
				selectedCounts[k] = v
			}
		}
	}

	// Echo back the backend's "accepted" forms so the inputs always show exactly
	// what the server honors — the homepage feed keeps the full canonical query
	// (sub: scope plus any author/media/score/comments/date/rating constraints),
	// NP keeps the plain a+b+c crawl list. The page renders these verbatim; there
	// is no client-side reconstruction.
	var frontPageQuery string
	if prefs.FrontPageSubs != "" && prefs.FrontPageSubs != "all" {
		frontPageQuery = searchquery.Parse(prefs.FrontPageSubs).Canonical()
	}
	prefetchQuery := searchquery.JoinSubs(prefetchSubs)

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
		FrontPageQuery: frontPageQuery,
		PrefetchQuery:  prefetchQuery,
		SubredditStats: subStats,
		ArchivedSubs:   archivedSubs,
		LiveSubs:       liveSubs,
		SelectedCounts: selectedCounts,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderer.RenderSettings(w, data)
}

func (h *Handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	updates := make(map[string]string)
	for _, key := range settingsKeys {
		if vals, ok := r.Form[key]; ok && len(vals) > 0 {
			updates[key] = vals[len(vals)-1]
		}
	}

	// Backend is the single source of truth for the homepage filter: parse the
	// submitted query, drop locally-known-dead include subs (keep syntactically
	// valid excludes), and re-serialize the WHOLE query to its canonical form so
	// every honored constraint round-trips. An empty result means "show all".
	if v, ok := updates["front_page_subs"]; ok {
		p := searchquery.Parse(v)
		p.WhiteSubs = h.filterUsableSubs(p.WhiteSubs)
		p.BlackSubs = h.filterValidSubs(p.BlackSubs)
		updates["front_page_subs"] = p.Canonical()
	}

	// NP crawl list uses the simple a+b+c format. Validate every name, drop the
	// known-dead, and store as a sub: clause so the scheduler's unified-grammar
	// parser still picks them up. An empty list disables crawling.
	if v, ok := updates["prefetch_subs"]; ok {
		names := h.filterUsableSubs(searchquery.ParseSubList(v))
		if len(names) == 0 {
			updates["prefetch_subs"] = ""
		} else {
			updates["prefetch_subs"] = "sub:" + searchquery.JoinSubs(names)
		}
	}

	if v, ok := updates["prefetch_threshold"]; ok {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 99 {
			delete(updates, "prefetch_threshold")
		} else {
			updates["prefetch_threshold"] = strconv.Itoa(n)
		}
	}

	// Video quality must be "source" or a known rendition height; reject
	// anything else so the stored value always maps to a real DASH ladder step.
	if v, ok := updates["video_quality"]; ok {
		if _, valid := reddit.VideoQualityHeights[v]; !valid && v != "source" {
			delete(updates, "video_quality")
		}
	}

	// The auto theme's day/night picks must name a real, selectable palette
	// (never "auto"/"system", which carry no fixed palette of their own); reject
	// anything else so the woken-theme CSS always resolves to an embedded sheet.
	for _, key := range []string{"auto_theme_day", "auto_theme_night"} {
		if v, ok := updates[key]; ok && !render.IsSelectableTheme(v) {
			delete(updates, key)
		}
	}

	if v, ok := updates["settings_token_ttl"]; ok && !allowedSettingsTokenTTL[v] {
		delete(updates, "settings_token_ttl")
	}

	if v, ok := updates["scroll_interval"]; ok {
		if n, err := strconv.Atoi(v); err != nil || n <= 0 {
			delete(updates, "scroll_interval")
		} else {
			updates["scroll_interval"] = strconv.Itoa(n)
		}
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

	if lang := updates["lang"]; lang != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     "lang",
			Value:    lang,
			Path:     "/",
			MaxAge:   cookieMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// filterValidSubs keeps only names with a well-formed subreddit syntax.
func (h *Handler) filterValidSubs(names []string) []string {
	var out []string
	for _, n := range names {
		if validSubName.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

// filterUsableSubs validates name syntax and then consults the local
// subreddit_status table, dropping any sub already known to be dead/private/
// quarantined. It deliberately does NOT probe upstream — that stays an explicit,
// per-sub action via /api/probe-sub — so a routine save neither hammers Reddit
// for every name nor blindly trusts unverified input. A DB error is non-fatal:
// we keep the user's syntactically valid list rather than silently discarding it.
func (h *Handler) filterUsableSubs(names []string) []string {
	clean := h.filterValidSubs(names)
	if len(clean) == 0 || h.subStatusStore == nil {
		return clean
	}
	statusMap, err := h.subStatusStore.GetStatusMap(clean)
	if err != nil {
		return clean
	}
	low := make(map[string]string, len(statusMap))
	for k, v := range statusMap {
		low[strings.ToLower(k)] = v
	}
	var out []string
	for _, n := range clean {
		switch low[strings.ToLower(n)] {
		case "dead", "private", "quarantined":
			// locally known-bad: drop
		default:
			out = append(out, n)
		}
	}
	return out
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
