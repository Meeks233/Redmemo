package handler

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
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
			http.Redirect(w, r, strings.TrimRight(path, "/")+r.URL.RawQuery, http.StatusMovedPermanently)
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

func readPreferences(r *http.Request) reddit.Preferences {
	p := reddit.Preferences{
		FixedNavbar: "on",
	}

	cookieStr := func(name string) string {
		c, err := r.Cookie(name)
		if err != nil {
			return ""
		}
		return c.Value
	}

	p.Theme = cookieStr("theme")
	p.FrontPage = cookieStr("front_page")
	p.Layout = cookieStr("layout")
	p.Wide = cookieStr("wide")
	p.BlurSpoiler = cookieStr("blur_spoiler")
	p.ShowNSFW = cookieStr("show_nsfw")
	p.BlurNSFW = cookieStr("blur_nsfw")
	p.HideHLSNotification = cookieStr("hide_hls_notification")
	p.VideoQuality = cookieStr("video_quality")
	p.HideSidebarAndSummary = cookieStr("hide_sidebar_and_summary")
	p.UseHLS = cookieStr("use_hls")
	p.AutoplayVideos = cookieStr("autoplay_videos")
	p.CommentSort = cookieStr("comment_sort")
	p.PostSort = cookieStr("post_sort")
	p.HideAwards = cookieStr("hide_awards")
	p.HideScore = cookieStr("hide_score")
	p.RemoveDefaultFeeds = cookieStr("remove_default_feeds")
	p.DisableVisitRedditConfirmation = cookieStr("disable_visit_reddit_confirmation")

	if v := cookieStr("fixed_navbar"); v != "" {
		p.FixedNavbar = v
	}

	p.Subscriptions = readMultiCookie(r, "subscriptions")
	p.Filters = readMultiCookie(r, "filters")

	return p
}

// readMultiCookie reads a +delimited list that may be split across numbered
// cookies (e.g. subscriptions, subscriptions1, subscriptions2, ...) to match
// redlib's cookie format for large subscription/filter lists.
func readMultiCookie(r *http.Request, baseName string) []string {
	var parts []string
	for i := 0; ; i++ {
		name := baseName
		if i > 0 {
			name = baseName + fmt.Sprintf("%d", i)
		}
		c, err := r.Cookie(name)
		if err != nil || c.Value == "" {
			if i == 0 {
				i++ // try subscriptions1 even if subscriptions is missing
				continue
			}
			break
		}
		parts = append(parts, c.Value)
	}
	if len(parts) == 0 {
		return nil
	}
	joined := strings.Join(parts, "+")
	return strings.Split(joined, "+")
}
