package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/media"
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

	// v.redd.it DASH/CMAF video segments are video-only — the audio track
	// lives in a sibling DASH_AUDIO_*.mp4. Mux them on the fly so the page's
	// plain <video src> tag plays with sound. The muxed result is cached by
	// the standard media proxy machinery.
	if strings.HasPrefix(path, "/vid/") && media.IsMuxableVideoSegment(path) {
		h.mediaProxy.ServeMuxed(w, r, upstream)
		return
	}

	r.URL.RawQuery = "url=" + url.QueryEscape(upstream)
	h.mediaProxy.ServeMedia(w, r)
}

// handleAudioStatus reports the audio-mux state of a v.redd.it video for the
// page's audioSync.js poller. The "src" query param is the video element's
// own /vid/... URL. A pending state also kicks the background mux so a video
// the user merely loaded gets its audio fetched promptly.
func (h *Handler) handleAudioStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	src := r.URL.Query().Get("src")
	u, err := url.Parse(src)
	if err != nil || !strings.HasPrefix(u.Path, "/vid/") || !media.IsMuxableVideoSegment(u.Path) {
		io.WriteString(w, `{"state":"unsupported"}`)
		return
	}

	upstream := "https://v.redd.it/" + strings.TrimPrefix(u.Path, "/vid/")
	if u.RawQuery != "" {
		upstream += "?" + u.RawQuery
	}
	fmt.Fprintf(w, `{"state":%q}`, h.mediaProxy.AudioStatus(upstream))
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
	h.renderer.RenderError(w, h.readPreferences(r).Lang, "Wiki page is currently unavailable", http.StatusServiceUnavailable)
}

func (h *Handler) handleFuckReddit(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)
	q := r.URL.Query()
	debug := prefs.EnableDebug == "on"

	reset, _ := h.oauthPool.EarliestReset()
	reason := q.Get("reason")

	// Debug mode: query params are authoritative for previewing the page —
	// skip every auto-detection branch so the developer can render any
	// combination of states without waiting for a real failure.
	//   /fuckreddit?reason=hr_l1&reset=42&from=/r/golang
	//   /fuckreddit?reason=         (force healthy)
	//   /fuckreddit?reason=quota_exhausted&from=/r/golang/comments/abc/title
	if debug {
		if v := q.Get("reset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				reset = n
			}
		}
	} else {
		// Reason priority: explicit query param > active HR cooldown > quota probe.
		if reason == "" && h.hr != nil {
			if r2, _ := h.hr.CooldownReason(r.Context()); r2 != "" {
				reason = r2
			}
		}
		if reason == "" && !h.oauthPool.HasAvailableTokens() {
			reason = "quota_exhausted"
		}
		// For HR cooldowns, override reset with the actual cooldown TTL —
		// the OAuth quota reset is unrelated to when HR cooldown lifts.
		if strings.HasPrefix(reason, "hr_") && h.hr != nil {
			if _, until := h.hr.CooldownReason(r.Context()); until > 0 {
				if secs := until - time.Now().Unix(); secs > 0 {
					reset = int(secs)
				}
			}
		}
	}

	from := validateFromPath(q.Get("from"))

	// ?freeze=1 (debug-only): pin the countdown to 99:99 and disable polling,
	// so the page can be inspected without the timer ticking down or the
	// auto-redirect firing once the degrade clears.
	freeze := debug && q.Get("freeze") == "1"

	h.renderer.RenderFuckReddit(w, prefs, reset, reason, from, freeze)
}

// validateFromPath accepts paths under /r/, /user/, or /search that look safe
// to append to "https://www.reddit.com". Returns "" for anything else.
// Prevents open-redirect through the "Go back to Reddit" button.
func validateFromPath(from string) string {
	if from == "" || !strings.HasPrefix(from, "/") || strings.HasPrefix(from, "//") {
		return ""
	}
	// Reject characters that could break out of attribute / URL context.
	for _, c := range from {
		if c < 0x20 || c == 0x7f || c == '"' || c == '<' || c == '>' || c == '\\' {
			return ""
		}
	}
	rest := strings.TrimPrefix(from, "/")
	seg, _, _ := strings.Cut(rest, "/")
	seg, _, _ = strings.Cut(seg, "?")
	switch seg {
	case "r", "user", "search":
		return from
	}
	return ""
}


func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	budget, _ := h.oauthPool.RemainingBudget(r.Context())
	reset, window := h.oauthPool.EarliestReset()

	// HR cooldown (most-severe active tier).
	hrReset := 0
	hrReason := ""
	if h.hr != nil {
		if reason, until := h.hr.CooldownReason(r.Context()); reason != "" && until > 0 {
			if secs := until - time.Now().Unix(); secs > 0 {
				hrReset = int(secs)
				hrReason = reason
			}
		}
	}

	// Combined "current degrade" view consumed by /fuckreddit so the page
	// shows one authoritative reason + countdown per poll. Priority mirrors
	// shouldDegrade: HR cooldown > quota_exhausted > clear.
	currentReason := ""
	currentReset := 0
	if hrReason != "" {
		currentReason = hrReason
		currentReset = hrReset
	} else if !h.oauthPool.HasAvailableTokens() {
		currentReason = "quota_exhausted"
		currentReset = reset
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, `{"remaining":%d,"reset":%d,"window":%d,"hr_reset":%d,"hr_reason":%q,"current_reason":%q,"current_reset":%d}`,
		budget, reset, window, hrReset, hrReason, currentReason, currentReset)
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
			L5Phase:     ps.L5Phase,
			L5Current:   ps.L5Current,
			L5Pending:   ps.L5Pending,
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
	h.renderer.RenderDebug(w, "Instance Diagnostics", prefs, dd)
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

