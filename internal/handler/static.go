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

func (h *Handler) handleFuckReddit(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)
	reset, _ := h.oauthPool.EarliestReset()

	// Reason priority: explicit query param > active HR cooldown > quota probe.
	reason := r.URL.Query().Get("reason")
	if reason == "" && h.hr != nil {
		if r2, _ := h.hr.CooldownReason(r.Context()); r2 != "" {
			reason = r2
		}
	}
	if reason == "" && !h.oauthPool.HasAvailableTokens() {
		reason = "quota_exhausted"
	}

	h.renderer.RenderFuckReddit(w, prefs, reset, reason)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	budget, _ := h.oauthPool.RemainingBudget(r.Context())
	reset, window := h.oauthPool.EarliestReset()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, `{"remaining":%d,"reset":%d,"window":%d}`, budget, reset, window)
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
		var expiresIn string
		if ts.ExpiresAt != nil {
			if d := time.Until(*ts.ExpiresAt); d > 0 {
				expiresIn = "in " + formatDuration(d)
			} else {
				expiresIn = "expired"
			}
		}
		tokenViews[i] = render.TokenView{
			Index:         i,
			Backend:       ts.Backend,
			Kind:          kind,
			RateRemaining: ts.RateRemaining,
			RateReset:     resetStr,
			HasBudget:     ts.RateRemaining > 0,
			UserAgent:     ts.UserAgent,
			DeviceID:      ts.DeviceID,
			Loid:          ts.Loid,
			Session:       ts.Session,
			ExpiresIn:     expiresIn,
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
	var subNames []string
	for _, s := range h.cfg.Prefetch.Subreddits {
		subNames = append(subNames, s.Name)
	}
	details = append(details, fmt.Sprintf("Prefetch enabled: %v (%d subs: %s)", h.cfg.Prefetch.Enabled, len(h.cfg.Prefetch.Subreddits), strings.Join(subNames, ", ")))
	details = append(details, fmt.Sprintf("Media cap: %d GB", h.cfg.Media.MaxSizeGB))

	// Redis
	details = append(details, fmt.Sprintf("Redis: %s", h.cfg.Redis.Addr))

	// Total token budget
	budget, _ := h.oauthPool.RemainingBudget(r.Context())

	var prefetchEvents []render.PrefetchEventView
	var prefetchStatus render.PrefetchStatusView
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

		ps := h.prefetcher.Status()
		var cursors []render.PrefetchCursorView
		for sub, cursor := range ps.L1Cursors {
			cursors = append(cursors, render.PrefetchCursorView{Sub: sub, Cursor: cursor})
		}
		var l1Progress string
		if ps.L1MaxRounds > 0 {
			l1Progress = fmt.Sprintf("%d / %d", ps.L1Round, ps.L1MaxRounds)
		}
		prefetchStatus = render.PrefetchStatusView{
			Enabled:     ps.Enabled,
			ActiveSubs:  strings.Join(ps.ActiveSubs, ", "),
			L1Phase:     ps.L1Phase,
			L1Progress:  l1Progress,
			L1Subs:      strings.Join(ps.L1Subs, ", "),
			L1Cursors:   cursors,
			L1NextCycle: ps.L1NextCycle,
			L2Phase:     ps.L2Phase,
			L2Sub:       ps.L2Sub,
			L2Pending:   ps.L2Pending,
			NPPhase:     ps.NPPhase,
			NPCurrent:   ps.NPCurrent,
			QueueLen:    ps.QueueLen,
		}
	}

	dd := render.DebugData{
		Details:        details,
		TokenBudget:    budget,
		Tokens:         tokenViews,
		PrefetchStatus: prefetchStatus,
		PrefetchEvents: prefetchEvents,
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

	// User-triggered probe is HR-gated.
	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("X-Reason", reason)
		json.NewEncoder(w).Encode(map[string]interface{}{"exists": false, "error": "degraded", "reason": reason})
		return
	}

	sub, err := h.publicCli.FetchSubredditAbout(r.Context(), name)
	h.recordUpstream(r.Context())
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

