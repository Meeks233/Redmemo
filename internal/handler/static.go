package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/render"
)

var validSubName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,20}$`)

func (h *Handler) staticHandler() http.Handler {
	return h.renderer.StaticHandler()
}

func (h *Handler) handleRedlibMedia(w http.ResponseWriter, r *http.Request) {
	cdnURL := pathToCDNURL(r.URL.Path, r.URL.RawQuery)
	if cdnURL == "" {
		http.NotFound(w, r)
		return
	}

	r.URL.RawQuery = "url=" + url.QueryEscape(cdnURL)
	h.mediaProxy.ServeMedia(w, r)
}

func pathToCDNURL(path, rawQuery string) string {
	var base string
	switch {
	case strings.HasPrefix(path, "/img/"):
		base = "https://i.redd.it/" + strings.TrimPrefix(path, "/img/")
	case strings.HasPrefix(path, "/preview/pre/"):
		base = "https://preview.redd.it/" + strings.TrimPrefix(path, "/preview/pre/")
	case strings.HasPrefix(path, "/preview/external-pre/"):
		base = "https://external-preview.redd.it/" + strings.TrimPrefix(path, "/preview/external-pre/")
	case strings.HasPrefix(path, "/thumb/a/"):
		base = "https://a.thumbs.redditmedia.com/" + strings.TrimPrefix(path, "/thumb/a/")
	case strings.HasPrefix(path, "/thumb/b/"):
		base = "https://b.thumbs.redditmedia.com/" + strings.TrimPrefix(path, "/thumb/b/")
	case strings.HasPrefix(path, "/emoji/"):
		base = "https://emoji.redditmedia.com/" + strings.TrimPrefix(path, "/emoji/")
	default:
		return ""
	}
	if rawQuery != "" {
		base += "?" + rawQuery
	}
	return base
}

func (h *Handler) handleVideoProxy(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	var upstream string
	switch {
	case strings.HasPrefix(path, "/vid/"):
		upstream = "https://v.redd.it/" + strings.TrimPrefix(path, "/vid/")
	case strings.HasPrefix(path, "/hls/"):
		upstream = "https://v.redd.it/" + strings.TrimPrefix(path, "/hls/")
	default:
		http.NotFound(w, r)
		return
	}

	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	if strings.HasSuffix(path, ".m3u8") || strings.Contains(path, "HLSPlaylist") {
		h.proxyHLSManifest(w, r, upstream)
		return
	}

	r.URL.RawQuery = "url=" + url.QueryEscape(upstream)
	h.mediaProxy.ServeMedia(w, r)
}

func (h *Handler) proxyHLSManifest(w http.ResponseWriter, r *http.Request, upstream string) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", upstream, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", h.uaPool.Get())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	s := strings.ReplaceAll(string(body), "https://v.redd.it/", "/vid/")

	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(s)))
	w.WriteHeader(resp.StatusCode)
	w.Write([]byte(s))
}

func (h *Handler) handleWiki(w http.ResponseWriter, r *http.Request) {
	h.renderer.RenderError(w, "Wiki page is currently unavailable", http.StatusServiceUnavailable)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	budget, _ := h.oauthPool.RemainingBudget(r.Context())
	reset, window := h.oauthPool.EarliestReset()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, `{"remaining":%d,"reset":%d,"window":%d}`, budget, reset, window)
}

func (h *Handler) handleCountdown(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)
	budget, _ := h.oauthPool.RemainingBudget(r.Context())
	reset, window := h.oauthPool.EarliestReset()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderer.RenderCountdown(w, prefs, budget, reset, window)
}

func (h *Handler) handleDebug(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)
	if prefs.EnableDebug != "on" {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	var details []string

	// OAuth tokens → structured view
	statuses := h.oauthPool.TokenStatuses()
	tokenViews := make([]render.TokenView, len(statuses))
	for i, ts := range statuses {
		kind := "static"
		if ts.Dynamic {
			kind = "dynamic"
		}
		var resetStr string
		if ts.RateResetAt.IsZero() {
			resetStr = "unknown"
		} else if d := time.Until(ts.RateResetAt); d > 0 {
			resetStr = "in " + formatDuration(d)
		} else {
			resetStr = "available"
		}
		tokenViews[i] = render.TokenView{
			Index:         i,
			Backend:       ts.Backend,
			Kind:          kind,
			RateRemaining: ts.RateRemaining,
			RateReset:     resetStr,
			HasBudget:     ts.RateRemaining > 0,
		}
	}

	// Archive stats
	postCount, _ := h.postStore.Count()
	subCount, _ := h.postStore.SubredditCount()
	details = append(details, fmt.Sprintf("Archived posts: %d", postCount))
	details = append(details, fmt.Sprintf("Archived subreddits: %d", subCount))

	// Media stats
	mediaCount, mediaSize, _ := h.mediaStore.Stats()
	details = append(details, fmt.Sprintf("Cached media: %d files, %s", mediaCount, formatBytes(mediaSize)))

	// Config
	details = append(details, fmt.Sprintf("Listen: %s", h.cfg.Server.Listen))
	details = append(details, fmt.Sprintf("Brand: %s", h.cfg.Render.BrandName))
	details = append(details, fmt.Sprintf("Prefetch enabled: %v (%d subs)", h.cfg.Prefetch.Enabled, len(h.cfg.Prefetch.Subreddits)))
	details = append(details, fmt.Sprintf("Media cap: %d GB", h.cfg.Media.MaxSizeGB))

	// Redis
	details = append(details, fmt.Sprintf("Redis: %s", h.cfg.Redis.Addr))

	// Total token budget
	budget, _ := h.oauthPool.RemainingBudget(r.Context())

	var prefetchEvents []render.PrefetchEventView
	if h.prefetcher != nil {
		events := h.prefetcher.Events.Snapshot()
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			prefetchEvents = append(prefetchEvents, render.PrefetchEventView{
				Time:         e.TimeStr(),
				RelativeTime: e.RelativeTime(),
				Level:        string(e.Level),
				Phase:        e.Phase,
				Message:      e.Message,
			})
		}
	}

	dd := render.DebugData{
		Details:        details,
		TokenBudget:    budget,
		Tokens:         tokenViews,
		PrefetchEvents: prefetchEvents,
	}

	if h.uaPool != nil {
		dd.UAList = h.uaPool.List()
		currentUA := h.uaPool.Get()
		for i, ua := range dd.UAList {
			if ua == currentUA {
				dd.UACurrentIndex = i
				break
			}
		}
		if h.settingsStore != nil {
			if ts, ok, _ := h.settingsStore.Get("_ua_pool_fetched_at"); ok && ts != "" {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					dd.UAFetchedAt = formatDuration(time.Since(t)) + " ago"
				} else {
					dd.UAFetchedAt = ts
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderer.RenderDebug(w, "Instance Diagnostics", dd)
}

func (h *Handler) handleProbeSub(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || !validSubName.MatchString(name) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"exists": false, "error": "invalid name"})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if h.subStatusStore != nil {
		st, _ := h.subStatusStore.Get(name)
		if st != nil {
			if st.Status == "live" {
				json.NewEncoder(w).Encode(map[string]interface{}{"exists": true, "name": st.Name, "cached": true})
				return
			}
			if st.Status == "dead" || st.Status == "private" || st.Status == "quarantined" {
				json.NewEncoder(w).Encode(map[string]interface{}{"exists": false, "status": st.Status, "cached": true})
				return
			}
		}
	}

	sub, err := h.publicCli.FetchSubredditAbout(r.Context(), name)
	if err != nil {
		if h.subStatusStore != nil {
			h.subStatusStore.RecordFailure(name, err.Error())
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"exists": false})
		return
	}

	if h.subStatusStore != nil {
		h.subStatusStore.MarkLive(sub.Name)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"exists": true,
		"name":   sub.Name,
		"title":  sub.Title,
	})
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

