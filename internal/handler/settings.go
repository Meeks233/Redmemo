package handler

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/redmemo/redmemo/internal/archive"
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
	"prefetch_sort", "prefetch_timeframe", "prefetch_sub_modes",
	"prefetch_unified",
	"archive_control",
	"prefetch_threshold", "scroll_interval", "lazy_media",
	"video_quality", "mute_all_videos", "mute_nsfw_videos",
	"auto_theme_day", "auto_theme_night",
	"disable_initiative_upstream_access",
	"settings_token_ttl",
	"page_limit",
}

// allowedSettingsTokenTTL is the whitelist of valid /settings auth-cookie
// lifetimes in minutes. Anything outside this set is dropped on save and the
// stored default ("10") stands. Capped at 60 by design — longer-lived ephemeral
// tokens defeat the lockout/TOTP gate's threat model.
var allowedSettingsTokenTTL = map[string]bool{
	"5": true, "10": true, "15": true, "30": true, "60": true,
}

// validPrefetchSort and validPrefetchTimeframe mirror redlib's listing-API
// query grammar: any value Reddit accepts for `/r/{sub}/{sort}.json?t=...`.
var validPrefetchSort = map[string]bool{
	"hot": true, "new": true, "top": true, "rising": true, "controversial": true,
}

var validPrefetchTimeframe = map[string]bool{
	"hour": true, "day": true, "week": true, "month": true, "year": true, "all": true,
}

type prefetchSubModeReject struct {
	raw    string
	reason string
}

// PrefetchSubOverride is one parsed per-sub override clause. Empty Sort or
// Timeframe means "fall back to the global setting".
type PrefetchSubOverride struct {
	Sub       string
	Sort      string
	Timeframe string
}

// ParsePrefetchSubModes parses the navbar-style query grammar used by the NP
// per-sub override field:
//
//	sub=sort:rising&time:day+sub2=sort:top+sub3=time:week
//
// Clauses are separated by '+'. Each clause is `<sub>=<k>:<v>(&<k>:<v>)*`.
// Recognised keys are `sort` and `time`. The parser is intentionally lenient
// to match how the navbar search drops unparseable trailing fragments — any
// clause whose subname is malformed is dropped wholesale, and any individual
// k:v pair with an unknown key or out-of-range value is silently discarded
// without nuking the rest of its clause. Duplicate subnames collapse to the
// last occurrence.
func ParsePrefetchSubModes(raw string) ([]PrefetchSubOverride, []prefetchSubModeReject) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var bad []prefetchSubModeReject
	byName := make(map[string]*PrefetchSubOverride)
	var order []string

	for _, clause := range strings.Split(raw, "+") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		eq := strings.IndexByte(clause, '=')
		if eq < 0 {
			bad = append(bad, prefetchSubModeReject{raw: clause, reason: "missing '=' (expected sub=k:v)"})
			continue
		}
		sub := strings.ToLower(strings.TrimSpace(clause[:eq]))
		body := strings.TrimSpace(clause[eq+1:])
		if !validSubName.MatchString(sub) {
			bad = append(bad, prefetchSubModeReject{raw: clause, reason: "invalid subreddit name"})
			continue
		}

		ov := &PrefetchSubOverride{Sub: sub}
		anyOK := false
		for _, kv := range strings.Split(body, "&") {
			kv = strings.TrimSpace(kv)
			if kv == "" {
				continue
			}
			colon := strings.IndexByte(kv, ':')
			if colon < 0 {
				bad = append(bad, prefetchSubModeReject{raw: kv, reason: "missing ':' (expected k:v)"})
				continue
			}
			key := strings.ToLower(strings.TrimSpace(kv[:colon]))
			val := strings.ToLower(strings.TrimSpace(kv[colon+1:]))
			switch key {
			case "sort":
				if !validPrefetchSort[val] {
					bad = append(bad, prefetchSubModeReject{raw: kv, reason: "sort must be hot/new/top/rising/controversial"})
					continue
				}
				ov.Sort = val
				anyOK = true
			case "time", "t", "timeframe":
				if !validPrefetchTimeframe[val] {
					bad = append(bad, prefetchSubModeReject{raw: kv, reason: "time must be hour/day/week/month/year/all"})
					continue
				}
				ov.Timeframe = val
				anyOK = true
			default:
				bad = append(bad, prefetchSubModeReject{raw: kv, reason: "unknown key (expected sort or time)"})
			}
		}
		if !anyOK {
			bad = append(bad, prefetchSubModeReject{raw: clause, reason: "no usable sort/time overrides"})
			continue
		}
		if _, dup := byName[sub]; !dup {
			order = append(order, sub)
		}
		byName[sub] = ov
	}

	out := make([]PrefetchSubOverride, 0, len(order))
	for _, sub := range order {
		out = append(out, *byName[sub])
	}
	return out, bad
}

// CanonicalPrefetchSubModes renders parsed overrides back to the wire form,
// stable for echo-back in the settings UI.
func CanonicalPrefetchSubModes(overrides []PrefetchSubOverride) string {
	if len(overrides) == 0 {
		return ""
	}
	parts := make([]string, 0, len(overrides))
	for _, o := range overrides {
		var kvs []string
		if o.Sort != "" {
			kvs = append(kvs, "sort:"+o.Sort)
		}
		if o.Timeframe != "" {
			kvs = append(kvs, "time:"+o.Timeframe)
		}
		if len(kvs) == 0 {
			continue
		}
		parts = append(parts, o.Sub+"="+strings.Join(kvs, "&"))
	}
	return strings.Join(parts, "+")
}

func normalizePrefetchSubModes(raw string) (string, []prefetchSubModeReject) {
	overrides, bad := ParsePrefetchSubModes(raw)
	return CanonicalPrefetchSubModes(overrides), bad
}

// splitPrefetchUnified parses the merged NP textarea grammar — a single
// `+`-separated stream where each clause is either a bare subreddit name
// ("golang") or a per-sub override clause ("cats=sort:rising&time:day") — and
// fans it out into the two storage keys the scheduler already consumes. Bare
// names land in prefetch_subs; override clauses land in prefetch_sub_modes,
// and their subnames are also added to prefetch_subs so the override actually
// drives a crawl. Final canonicalisation/validation is left to the existing
// prefetch_subs and prefetch_sub_modes branches below.
func splitPrefetchUnified(raw string) (subsCSV, modesRaw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	var modesOrder, allSubsOrder []string
	seenSub := make(map[string]bool)
	for _, clause := range strings.Split(raw, "+") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		eq := strings.IndexByte(clause, '=')
		var name string
		if eq < 0 {
			name = strings.ToLower(clause)
		} else {
			name = strings.ToLower(strings.TrimSpace(clause[:eq]))
			modesOrder = append(modesOrder, clause)
		}
		if name != "" && !seenSub[name] {
			seenSub[name] = true
			allSubsOrder = append(allSubsOrder, name)
		}
	}
	return strings.Join(allSubsOrder, "+"), strings.Join(modesOrder, "+")
}

// ComposePrefetchUnified rebuilds the merged textarea value for echo-back to
// the settings page from the two stored keys. Bare subs (those without an
// override clause) come first, then the canonical override clauses, joined by
// '+'. Empty inputs produce an empty string.
func ComposePrefetchUnified(subsCSV, modesRaw string) string {
	overrideSet := make(map[string]bool)
	for _, clause := range strings.Split(modesRaw, "+") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		eq := strings.IndexByte(clause, '=')
		if eq < 0 {
			continue
		}
		overrideSet[strings.ToLower(strings.TrimSpace(clause[:eq]))] = true
	}
	var bare []string
	for _, name := range strings.Split(subsCSV, "+") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || overrideSet[name] {
			continue
		}
		bare = append(bare, name)
	}
	parts := bare
	if modesRaw != "" {
		parts = append(parts, modesRaw)
	}
	return strings.Join(parts, "+")
}

// NormalizeSettings canonicalises a settings map the same way the form save
// does, EXCEPT for the dead-sub filter — that consults the substatus DB which
// may not exist yet at env-application time. Returns the normalised map plus a
// slice of (key, original-value, reason) tuples for keys that were dropped so
// callers can log them. Safe to call on env_override input at startup so
// `REDMEMO_DEFAULT_PREFETCH_SUBS=golang` becomes stored as `sub:golang` (matching
// what a UI save would produce) and obvious typos surface in the log instead
// of being silently persisted.
func NormalizeSettings(updates map[string]string) (map[string]string, []RejectedSetting) {
	out := make(map[string]string, len(updates))
	var rejected []RejectedSetting

	for k, v := range updates {
		out[k] = v
	}

	// prefetch_unified is a virtual form-only key: the merged settings textarea
	// posts a single `+`-separated stream of bare subs and per-sub override
	// clauses. Split it into the two real storage keys (prefetch_subs and
	// prefetch_sub_modes) so the existing validation, persistence, and
	// scheduler-side resolution all continue to work unchanged. If the caller
	// also sent the legacy keys, the unified value wins.
	if v, ok := out["prefetch_unified"]; ok {
		subsCSV, modesRaw := splitPrefetchUnified(v)
		out["prefetch_subs"] = subsCSV
		out["prefetch_sub_modes"] = modesRaw
		delete(out, "prefetch_unified")
	}

	if v, ok := out["front_page_subs"]; ok && v != "" && v != "all" {
		p := searchquery.Parse(v)
		p.WhiteSubs = filterValidSubsList(p.WhiteSubs)
		p.BlackSubs = filterValidSubsList(p.BlackSubs)
		out["front_page_subs"] = p.Canonical()
	}

	if v, ok := out["prefetch_subs"]; ok && v != "" {
		names := filterValidSubsList(searchquery.ParseSubList(v))
		if len(names) == 0 {
			out["prefetch_subs"] = ""
		} else {
			out["prefetch_subs"] = "sub:" + searchquery.JoinSubs(names)
		}
	}

	// archive_control runs its own rule set (+ wins over -, duplicates dropped
	// entirely) — see archive.ParseControl. We canonicalise to the same form
	// here so the input box echoes exactly what the archive layer will honor.
	if v, ok := out["archive_control"]; ok && v != "" {
		out["archive_control"] = archive.ParseControl(v).Canonical()
	}

	if v, ok := out["prefetch_threshold"]; ok {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 99 {
			delete(out, "prefetch_threshold")
			rejected = append(rejected, RejectedSetting{Key: "prefetch_threshold", Value: v, Reason: "must be an integer in [1, 99]"})
		} else {
			out["prefetch_threshold"] = strconv.Itoa(n)
		}
	}

	if v, ok := out["video_quality"]; ok {
		if _, valid := reddit.VideoQualityHeights[v]; !valid && v != "source" {
			delete(out, "video_quality")
			rejected = append(rejected, RejectedSetting{Key: "video_quality", Value: v, Reason: "must be \"source\" or a known DASH ladder height"})
		}
	}

	for _, key := range []string{"auto_theme_day", "auto_theme_night"} {
		if v, ok := out[key]; ok && !render.IsSelectableTheme(v) {
			delete(out, key)
			rejected = append(rejected, RejectedSetting{Key: key, Value: v, Reason: "must be a selectable theme (not \"auto\" / \"system\")"})
		}
	}

	if v, ok := out["settings_token_ttl"]; ok && !allowedSettingsTokenTTL[v] {
		delete(out, "settings_token_ttl")
		rejected = append(rejected, RejectedSetting{Key: "settings_token_ttl", Value: v, Reason: "must be one of 5, 10, 15, 30, 60"})
	}

	if v, ok := out["page_limit"]; ok {
		if n, err := strconv.Atoi(v); err != nil || n < 5 || n > 100 {
			delete(out, "page_limit")
			rejected = append(rejected, RejectedSetting{Key: "page_limit", Value: v, Reason: "must be an integer in [5, 100]"})
		} else {
			out["page_limit"] = strconv.Itoa(n)
		}
	}

	if v, ok := out["prefetch_sort"]; ok && v != "" {
		if !validPrefetchSort[v] {
			delete(out, "prefetch_sort")
			rejected = append(rejected, RejectedSetting{Key: "prefetch_sort", Value: v, Reason: "must be one of hot/new/top/rising/controversial"})
		}
	}

	if v, ok := out["prefetch_timeframe"]; ok && v != "" {
		if !validPrefetchTimeframe[v] {
			delete(out, "prefetch_timeframe")
			rejected = append(rejected, RejectedSetting{Key: "prefetch_timeframe", Value: v, Reason: "must be one of hour/day/week/month/year/all"})
		}
	}

	if v, ok := out["prefetch_sub_modes"]; ok && v != "" {
		cleaned, bad := normalizePrefetchSubModes(v)
		out["prefetch_sub_modes"] = cleaned
		for _, b := range bad {
			rejected = append(rejected, RejectedSetting{Key: "prefetch_sub_modes", Value: b.raw, Reason: b.reason})
		}
	}

	if v, ok := out["scroll_interval"]; ok {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 60 {
			delete(out, "scroll_interval")
			rejected = append(rejected, RejectedSetting{Key: "scroll_interval", Value: v, Reason: "must be an integer in [1, 60] (seconds)"})
		} else {
			out["scroll_interval"] = strconv.Itoa(n)
		}
	}

	return out, rejected
}

// RejectedSetting describes one key NormalizeSettings refused to accept,
// used for startup logging of bad REDMEMO_DEFAULT_* values.
type RejectedSetting struct {
	Key    string
	Value  string
	Reason string
}

// filterValidSubsList keeps only syntactically valid subreddit names — same
// regex check the form path uses, but standalone so NormalizeSettings can run
// before any Handler exists.
func filterValidSubsList(names []string) []string {
	var out []string
	for _, n := range names {
		if validSubName.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	snap := h.stats.get(h)
	postCount := snap.postCount
	subCount := snap.subCount
	var subStats []render.SubredditStatView
	for _, s := range snap.subStats {
		subStats = append(subStats, render.SubredditStatView{
			Name:      s.Name,
			PostCount: s.PostCount,
		})
	}
	mediaCount, mediaSize := snap.mediaCount, snap.mediaSize

	// prefetch_subs holds a query in the unified search grammar; the configured
	// subs are its sub: includes. Used only to surface per-sub stats below.
	// h.siteDefaults already mirrors the settings table (refreshed on every
	// save), so consult it instead of issuing another DB round-trip.
	var prefetchSubs []string
	if v := h.siteDefaults["prefetch_subs"]; v != "" {
		prefetchSubs = searchquery.Parse(v).WhiteSubs
	}

	if h.postStore != nil && len(prefetchSubs) > 0 {
		statSet := make(map[string]bool, len(subStats))
		for _, s := range subStats {
			statSet[s.Name] = true
		}
		var missing []string
		for _, name := range prefetchSubs {
			if !statSet[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			// Batch the per-name CountBySubreddit calls into one round-trip.
			// SubredditCounts keys by stored-case subreddit, so fold to lower
			// for the lookup; the display preserves the input name.
			counts, _ := h.postStore.SubredditCounts(missing)
			lower := make(map[string]int, len(counts))
			for k, v := range counts {
				lower[strings.ToLower(k)] = v
			}
			for _, name := range missing {
				subStats = append(subStats, render.SubredditStatView{
					Name:      name,
					PostCount: int64(lower[strings.ToLower(name)]),
				})
			}
		}
	}

	archivedSubs := snap.distinctSubs
	liveSubs := snap.liveSubs

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
	prefetchUnified := ComposePrefetchUnified(prefetchQuery, h.siteDefaults["prefetch_sub_modes"])

	archiveControl := h.siteDefaults["archive_control"]

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
		PrefetchUnified: prefetchUnified,
		ArchiveControl: archiveControl,
		SubredditStats: subStats,
		ArchivedSubs:   archivedSubs,
		LiveSubs:       liveSubs,
		SelectedCounts: selectedCounts,
		AuthBypass:     h.cfg.Auth.BypassAuth,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Settings is rendered behind the ephemeral TOTP token cookie — never let a
	// browser or intermediary cache it, or a token rollover would still serve
	// the old session's view.
	w.Header().Set("Cache-Control", "no-store")
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

	// Format-canonicalise everything via the shared normaliser (same routine
	// main.go runs on REDMEMO_DEFAULT_* values), then layer the dead-sub
	// filter on top — that consults the live substatus DB and so only the form
	// path can run it.
	normalised, _ := NormalizeSettings(updates)
	updates = normalised

	if v, ok := updates["front_page_subs"]; ok && v != "" && v != "all" {
		p := searchquery.Parse(v)
		p.WhiteSubs = h.filterUsableSubs(p.WhiteSubs)
		p.BlackSubs = h.filterValidSubs(p.BlackSubs)
		updates["front_page_subs"] = p.Canonical()
	}

	if v, ok := updates["prefetch_subs"]; ok && v != "" {
		names := h.filterUsableSubs(searchquery.ParseSubList(v))
		if len(names) == 0 {
			updates["prefetch_subs"] = ""
		} else {
			updates["prefetch_subs"] = "sub:" + searchquery.JoinSubs(names)
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
		// Hot-swap the archiver's Control whenever the user changes the
		// archive_control field — no restart needed.
		if v, ok := updates["archive_control"]; ok && h.archiver != nil {
			h.archiver.SetControlFromString(v)
		}
		// Site-default changes affect every anonymous render: a visitor with
		// no overriding cookie picks up site defaults via readPreferences, so
		// any cached HTML built against the old defaults is now stale and
		// would never expire via per-cookie key fingerprinting. Sweep them.
		if h.cache != nil {
			if err := h.cache.InvalidateAllHTML(r.Context()); err != nil {
				log.Printf("[settings] invalidate html cache: %v", err)
			}
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
