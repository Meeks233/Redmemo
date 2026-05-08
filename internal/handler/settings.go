package handler

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/proxy"
	"github.com/redmemo/redmemo/internal/render"
)

const (
	cookieMaxAge       = 52 * 7 * 24 * 60 * 60 // 52 weeks in seconds
	maxCookieValueLen  = 4000                   // safe limit per cookie
	healthCacheDur     = 30 * time.Second
)

var prefCookieNames = []string{
	"theme", "front_page", "layout", "wide",
	"blur_spoiler", "show_nsfw", "blur_nsfw",
	"hide_hls_notification", "video_quality",
	"hide_sidebar_and_summary", "use_hls",
	"autoplay_videos", "fixed_navbar",
	"disable_visit_reddit_confirmation",
	"comment_sort", "post_sort",
	"hide_awards", "hide_score", "remove_default_feeds",
}

// redlibHealth caches a lightweight probe result.
type redlibHealth struct {
	mu      sync.RWMutex
	alive   bool
	checkedAt time.Time
}

func (h *Handler) isRedlibAlive(r *http.Request) bool {
	if !h.cfg.Redlib.Enabled || h.proxy == nil {
		return false
	}

	h.healthMu.RLock()
	if time.Since(h.healthCheckedAt) < healthCacheDur {
		alive := h.healthAlive
		h.healthMu.RUnlock()
		return alive
	}
	h.healthMu.RUnlock()

	// Probe with a lightweight request to /settings (small page)
	probeReq, err := http.NewRequestWithContext(r.Context(), "GET", "/", nil)
	if err != nil {
		return false
	}
	probeReq.URL.Path = "/"
	resp, body, err := h.proxy.Forward(probeReq)
	alive := err == nil && resp != nil && resp.StatusCode < 500 && !proxy.IsRateLimited(resp.StatusCode, body)

	h.healthMu.Lock()
	h.healthAlive = alive
	h.healthCheckedAt = time.Now()
	h.healthMu.Unlock()

	return alive
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	// If redlib is alive, proxy the settings page
	if h.isRedlibAlive(r) {
		resp, body, err := h.proxy.Forward(r)
		if err == nil && resp.StatusCode == 200 && !proxy.IsRateLimited(resp.StatusCode, body) {
			body = h.rewriteMedia(h.rebrand(body))
			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
	}

	// Fallback: serve our own settings page
	prefs := readPreferences(r)
	data := render.SettingsPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderer.RenderSettings(w, data)
}

func (h *Handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	for _, name := range prefCookieNames {
		value := r.FormValue(name)
		if value != "" {
			setCookiePref(w, name, value)
		} else {
			clearCookiePref(w, name)
		}
	}

	// Handle subscriptions and filters (preserve existing unless explicitly set)
	if r.Form.Has("subscriptions") {
		setListCookie(w, "subscriptions", strings.Split(r.FormValue("subscriptions"), "+"))
	}
	if r.Form.Has("filters") {
		setListCookie(w, "filters", strings.Split(r.FormValue("filters"), "+"))
	}

	// If redlib is alive, forward the POST to keep it in sync
	if h.isRedlibAlive(r) {
		if _, _, err := h.proxy.Forward(r); err != nil {
			log.Printf("settings: failed to forward POST to redlib: %v", err)
		}
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleSettingsRestore restores preferences from URL query parameters.
// GET /settings/restore?theme=dark&layout=compact&subscriptions=golang+rust
func (h *Handler) handleSettingsRestore(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	for _, name := range prefCookieNames {
		if v := query.Get(name); v != "" {
			setCookiePref(w, name, v)
		}
	}

	// Restore subscriptions — clear old numbered cookies first
	if subs := query.Get("subscriptions"); subs != "" {
		clearNumberedCookies(w, "subscriptions")
		setListCookie(w, "subscriptions", strings.Split(subs, "+"))
	}
	if filters := query.Get("filters"); filters != "" {
		clearNumberedCookies(w, "filters")
		setListCookie(w, "filters", strings.Split(filters, "+"))
	}

	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (h *Handler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := readPreferences(r)
	subs := prefs.Subscriptions

	for _, s := range subs {
		if strings.EqualFold(s, sub) {
			http.Redirect(w, r, "/r/"+sub, http.StatusSeeOther)
			return
		}
	}
	subs = append(subs, sub)
	setListCookie(w, "subscriptions", subs)
	http.Redirect(w, r, "/r/"+sub, http.StatusSeeOther)
}

func (h *Handler) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := readPreferences(r)
	var filtered []string
	for _, s := range prefs.Subscriptions {
		if !strings.EqualFold(s, sub) {
			filtered = append(filtered, s)
		}
	}
	setListCookie(w, "subscriptions", filtered)
	http.Redirect(w, r, "/r/"+sub, http.StatusSeeOther)
}

func (h *Handler) handleFilter(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := readPreferences(r)
	filters := prefs.Filters

	for _, f := range filters {
		if strings.EqualFold(f, sub) {
			http.Redirect(w, r, "/r/"+sub, http.StatusSeeOther)
			return
		}
	}
	filters = append(filters, sub)
	setListCookie(w, "filters", filters)
	http.Redirect(w, r, "/r/"+sub, http.StatusSeeOther)
}

func (h *Handler) handleUnfilter(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	prefs := readPreferences(r)
	var filtered []string
	for _, f := range prefs.Filters {
		if !strings.EqualFold(f, sub) {
			filtered = append(filtered, f)
		}
	}
	setListCookie(w, "filters", filtered)
	http.Redirect(w, r, "/r/"+sub, http.StatusSeeOther)
}

// setCookiePref sets a single preference cookie in redlib-compatible format.
func setCookiePref(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookiePref removes a preference cookie.
func clearCookiePref(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:   name,
		Path:   "/",
		MaxAge: -1,
	})
}

// setListCookie writes a +delimited list, splitting across numbered cookies
// if the value exceeds cookie size limits (matching redlib's behavior).
func setListCookie(w http.ResponseWriter, baseName string, items []string) {
	if len(items) == 0 {
		clearCookiePref(w, baseName)
		return
	}

	value := strings.Join(items, "+")

	if len(value) <= maxCookieValueLen {
		setCookiePref(w, baseName, value)
		return
	}

	// Split across numbered cookies
	idx := 0
	for len(value) > 0 {
		chunk := value
		if len(chunk) > maxCookieValueLen {
			// Cut at a + boundary to avoid splitting a subreddit name
			cutAt := strings.LastIndex(chunk[:maxCookieValueLen], "+")
			if cutAt <= 0 {
				cutAt = maxCookieValueLen
			}
			chunk = value[:cutAt]
			value = value[cutAt+1:] // skip the +
		} else {
			value = ""
		}

		name := baseName
		if idx > 0 {
			name = baseName + string(rune('0'+idx))
		}
		setCookiePref(w, name, chunk)
		idx++
	}
}

// clearNumberedCookies removes base + numbered cookies (base, base1, base2, ..., base9).
func clearNumberedCookies(w http.ResponseWriter, baseName string) {
	clearCookiePref(w, baseName)
	for i := 1; i <= 9; i++ {
		clearCookiePref(w, baseName+string(rune('0'+i)))
	}
}
