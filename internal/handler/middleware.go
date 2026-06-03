package handler

import (
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
)

func (h *Handler) applyMiddleware(next http.Handler) http.Handler {
	return pathNormalize(logging(recovery(next)))
}

func pathNormalize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Remove double slashes
		for strings.Contains(path, "//") {
			path = strings.ReplaceAll(path, "//", "/")
		}

		// Remove trailing slash (except root)
		if len(path) > 1 && strings.HasSuffix(path, "/") {
			target := strings.TrimRight(path, "/")
			if target == "" {
				target = "/"
			}
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}

		r.URL.Path = path
		next.ServeHTTP(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic: %v\n%s", err, debug.Stack())
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}


var prefDefaults = map[string]string{
	"front_page":      "default",
	"front_page_subs":      "all",
	"layout":        "card",
	"wide":          "off",
	"blur_spoiler":  "on",
	"show_nsfw":     "on",
	"show_local_nsfw_subs": "off",
	"blur_nsfw":     "on",
	"autoplay_videos":                 "on",
	"fixed_navbar":                    "on",
	"hide_sidebar_and_summary":        "off",
	"hide_awards":                     "off",
	"hide_score":                      "off",
	"remove_default_feeds":            "off",
	"fetch_sub_about":                 "off",
	"disable_visit_reddit_confirmation": "off",
	"comment_sort": "new",
	"post_sort":    "new",
	"enable_debug":            "off",
	"enable_natural_prefetch": "off",
	"prefetch_subs":           "",
	"prefetch_threshold":      "50",
	"scroll_interval":         "2",
	"lazy_media":              "on",
	"video_quality":           "source",
	"mute_all_videos":         "off",
	"mute_nsfw_videos":        "on",
	"disable_initiative_upstream_access": "on",
	"settings_token_ttl":      "10",
	"page_limit":              "5",
	"auto_theme_day":          "light",
	"auto_theme_night":        "black",
	"lang":                    render.DefaultLang,
}

func (h *Handler) readPreferences(r *http.Request) reddit.Preferences {
	pref := func(name string) string {
		if v := h.siteDefaults[name]; v != "" {
			return v
		}
		return prefDefaults[name]
	}

	p := reddit.Preferences{}

	if c, err := r.Cookie("theme"); err == nil {
		p.Theme = c.Value
	} else {
		p.Theme = pref("theme")
	}
	p.AutoThemeDay = pref("auto_theme_day")
	p.AutoThemeNight = pref("auto_theme_night")

	// Language: an explicit `lang` cookie wins; otherwise resolve from the
	// Accept-Language header, falling back to the configured default. The
	// result is always a SupportedLangs value so the HTML cache key stays
	// discrete.
	if c, err := r.Cookie("lang"); err == nil {
		p.Lang = render.ResolveLang(c.Value, r.Header.Get("Accept-Language"))
	} else {
		p.Lang = render.ResolveLang("", r.Header.Get("Accept-Language"))
	}

	p.FrontPage = pref("front_page")
	p.FrontPageSubs = pref("front_page_subs")
	p.Layout = pref("layout")
	p.Wide = pref("wide")
	p.BlurSpoiler = pref("blur_spoiler")
	p.ShowNSFW = pref("show_nsfw")
	p.ShowLocalNSFWSubs = pref("show_local_nsfw_subs")
	p.BlurNSFW = pref("blur_nsfw")
	p.HideSidebarAndSummary = pref("hide_sidebar_and_summary")
	p.AutoplayVideos = pref("autoplay_videos")
	p.CommentSort = pref("comment_sort")
	p.PostSort = pref("post_sort")
	p.HideAwards = pref("hide_awards")
	p.HideScore = pref("hide_score")
	p.RemoveDefaultFeeds = pref("remove_default_feeds")
	p.FetchSubAbout = pref("fetch_sub_about")
	p.DisableVisitRedditConfirmation = pref("disable_visit_reddit_confirmation")
	p.FixedNavbar = pref("fixed_navbar")
	p.EnableDebug = pref("enable_debug")
	p.EnableNaturalPrefetch = pref("enable_natural_prefetch")
	p.PrefetchSubs = pref("prefetch_subs")
	p.PrefetchThreshold = pref("prefetch_threshold")
	p.ScrollInterval = pref("scroll_interval")
	p.LazyMedia = pref("lazy_media")
	p.VideoQuality = pref("video_quality")
	p.MuteAllVideos = pref("mute_all_videos")
	p.MuteNSFWVideos = pref("mute_nsfw_videos")
	p.DisableInitiativeUpstreamAccess = pref("disable_initiative_upstream_access")
	p.SettingsTokenTTL = pref("settings_token_ttl")
	p.PageLimit = pref("page_limit")

	p.AvailableThemes = render.AvailableThemes()

	return p
}

