package render

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/redmemo/redmemo/internal/reddit"
)

// pageSlots are the optional override regions of the base layout, mirroring the
// {{ block }} hooks of the old base.html (title, head_extra, subscriptions,
// search, body, content, footer). A nil slot renders the layout's default.
// Body, when set, replaces the entire <main> wrapper; otherwise Content is
// wrapped in the default <main>.
type pageSlots struct {
	Title         templ.Component
	HeadExtra     templ.Component
	Subscriptions templ.Component
	Search        templ.Component
	Body          templ.Component
	Content       templ.Component
	Footer        templ.Component
	// Media gates the media-only scripts (lazy load, autoplay, preload, audio
	// sync, image reload). Content pages that render posts set it; chrome-only
	// pages (settings, error, fuckreddit) leave it false so they don't ship —
	// or run a global MutationObserver for — media machinery they never use.
	Media bool
}

// i18nState carries the locale-bound translator and the resolved <html lang>
// value for one request. It is stashed in the context so templ components can
// translate without threading locale through every nested component — the same
// ergonomic the old html/template FuncMap gave via the bound "T"/"lang" funcs.
type i18nState struct {
	t    func(key string, args ...any) string
	lang string // value for <html lang="...">
}

type ctxKey int

const i18nKey ctxKey = 0

// i18nContext returns a context carrying the translator and html-lang for lang,
// falling back to DefaultLang when lang is unknown.
func (e *Engine) i18nContext(lang string) context.Context {
	t := e.translators[lang]
	if t == nil {
		t = e.translators[DefaultLang]
	}
	return context.WithValue(context.Background(), i18nKey, i18nState{t: t, lang: htmlLang(lang)})
}

// T translates key within the request's locale. Outside a templ render (no
// i18n state in ctx) it degrades to returning the key, matching the old
// translator's final fallback.
func T(ctx context.Context, key string, args ...any) string {
	if s, ok := ctx.Value(i18nKey).(i18nState); ok {
		return s.t(key, args...)
	}
	return key
}

// ctxLang returns the <html lang> value bound to ctx, or DefaultLang.
func ctxLang(ctx context.Context) string {
	if s, ok := ctx.Value(i18nKey).(i18nState); ok {
		return s.lang
	}
	return DefaultLang
}

// --- view helpers (replace the old FuncMap entries used by the base layout) ---

// brandHead/brandTail split the brand name the way base.html did with
// `slice .BrandName 0 3` / `slice .BrandName 3 (len .BrandName)`.
func brandHead(brand string) string {
	if len(brand) < 3 {
		return brand
	}
	return brand[:3]
}

func brandTail(brand string) string {
	if len(brand) < 3 {
		return ""
	}
	return brand[3:]
}

// htmlRootClass is the <html> class: "fixed_navbar" when the pref is on.
func htmlRootClass(p reddit.Preferences) string {
	if p.FixedNavbar == "on" {
		return "fixed_navbar"
	}
	return ""
}

// navClass mirrors htmlRootClass for the <nav> element.
func navClass(p reddit.Preferences) string { return htmlRootClass(p) }

// bodyClass reproduces base.html's <body> class expression: layout, optional
// "wide", the theme (unless system), and "fixed_navbar".
func bodyClass(p reddit.Preferences) string {
	var b strings.Builder
	if p.Layout != "" {
		b.WriteString(p.Layout)
	}
	if p.Wide == "on" {
		b.WriteString(" wide")
	}
	if p.Theme != "" && p.Theme != "system" {
		b.WriteString(" ")
		b.WriteString(p.Theme)
	}
	if p.FixedNavbar == "on" {
		b.WriteString(" fixed_navbar")
	}
	return strings.TrimSpace(b.String())
}

// i64 formats an int64 for emission in markup (width/height attrs etc.).
func i64(n int64) string { return strconv.FormatInt(n, 10) }

// searchEndpoint returns the route the local-search infinite-scroll loader hits
// for its partial=1 fragments. Without a sub it's the global /search; with a
// sub it's the per-sub /r/<sub>/search.
func searchEndpoint(sub string) string {
	if sub != "" {
		return "/r/" + sub + "/search"
	}
	return "/search"
}

// searchLocalQS encodes the filter half of the URL (raw query box + sort) used
// by the offline /search infinite-scroll loader. The loader appends
// "&offset=N&partial=1" to whatever this returns.
func searchLocalQS(query, sort string) string {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if sort != "" {
		v.Set("sort", sort)
	}
	return v.Encode()
}

// u64p formats a *uint64 poll vote count; nil becomes "" so the markup matches
// the old `{{ .VoteCount }}` on an open poll.
func u64p(p *uint64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatUint(*p, 10)
}

// communityPath maps a community name to its feed path: "u_x" -> "u/x",
// everything else -> "r/<name>". Mirrors the old FuncMap entry.
func communityPath(name string) string {
	if strings.HasPrefix(name, "u_") {
		return "u/" + name[2:]
	}
	return "r/" + name
}

// commentsWord returns the singular/plural comment noun for a raw count.
func commentsWord(raw string) string {
	if raw == "1" {
		return "comment"
	}
	return "comments"
}

// sortMethods / timeframes are the listing sort + timeframe option sets the
// subreddit page iterates over (the old `list ...` literals).
var sortMethods = []string{"hot", "new", "top", "rising", "controversial"}
var timeframes = []string{"hour", "day", "week", "month", "year", "all"}

// commentSorts is the comment sort option set on the post page.
var commentSorts = []string{"confidence", "top", "new", "controversial", "old"}

// userListings / userSorts are the user-profile listing tabs and sort options.
var userListings = []string{"overview", "comments", "submitted"}
var userSorts = []string{"hot", "new", "top", "controversial"}

// sortHref builds a subreddit/front-page sort link: "/r/<sub>/<method>" when a
// subreddit is in scope, otherwise "/<method>".
func sortHref(subName, method string) string {
	if subName != "" {
		return "/r/" + subName + "/" + method
	}
	return "/" + method
}

// showRealSub reports whether name is a concrete subreddit (not the aggregate
// "all"/"popular" feeds, not a multi "a+b" feed) — i.e. one that has a sidebar.
func showRealSub(name string) bool {
	return name != "" && name != "all" && name != "popular" && !strings.Contains(name, "+")
}

// isBlurredInList reports whether a listing card should be blurred under the
// user's NSFW/spoiler blur prefs.
func isBlurredInList(p reddit.Post, prefs reddit.Preferences) bool {
	return (p.Flags.NSFW && prefs.BlurNSFW == "on") || (p.Flags.Spoiler && prefs.BlurSpoiler == "on")
}

// playAsGif decides whether a gif/video plays through the silent autoplay GIF
// widget rather than the full <video controls> player: an is_gif clip of 3s or
// less (or unknown duration), or any clip 0<dur<=3s. Matches the old template's
// `or (and (eq PostType "gif") (not (Duration>3))) (Duration>0 && <=3)`.
func playAsGif(p reddit.Post) bool {
	if p.PostType == "gif" && !(p.Media.Duration > 3.0) {
		return true
	}
	return p.Media.Duration > 0 && p.Media.Duration <= 3.0
}

// videoMuted reports whether a post's <video controls> player should start
// muted, honoring the user's mute prefs: "mute all videos" mutes everything,
// otherwise "mute NSFW videos" mutes only posts flagged NSFW. GIF-style clips
// are always muted regardless (handled at the template), so this only governs
// the full controls player.
func videoMuted(p reddit.Post, prefs reddit.Preferences) bool {
	if prefs.MuteAllVideos == "on" {
		return true
	}
	return p.Flags.NSFW && prefs.MuteNSFWVideos == "on"
}

// The flair/thumbnail style helpers return templ.SafeCSS so the (Reddit-sourced)
// values are emitted verbatim, matching the old direct interpolation rather than
// being neutralized by templ's CSS sanitizer.
func flairEmojiStyle(url string) templ.SafeCSS {
	return templ.SafeCSS("background-image:url('" + url + "');")
}

func flairBoxStyle(fg, bg string) templ.SafeCSS {
	return templ.SafeCSS("color:" + fg + "; background:" + bg + ";")
}

func thumbBoxStyle(w, h int64) templ.SafeCSS {
	return templ.SafeCSS("max-width:" + i64(w) + "px;max-height:" + i64(h) + "px;")
}

// frReasonTexts maps each degrade reason code to its localized message, used by
// /fuckreddit both for the initial server-rendered text and (serialized as JSON)
// for the client poller that swaps the text when the active tier changes.
func frReasonTexts(ctx context.Context) map[string]string {
	return map[string]string{
		"upstream_disabled": T(ctx, "fr.upstream_disabled"),
		"quota_exhausted":   T(ctx, "fr.quota_exhausted"),
		"hr_l1":             T(ctx, "fr.hr_l1"),
		"hr_l2":             T(ctx, "fr.hr_l2"),
		"hr_l3":             T(ctx, "fr.hr_l3"),
		"hr_redis_down":     T(ctx, "fr.hr_redis_down"),
		"totp_replay":       T(ctx, "fr.totp_replay"),
		"auth_locked":       T(ctx, "fr.auth_locked"),
		"unsafe_env":        T(ctx, "fr.unsafe_env"),
		"internal_error":    T(ctx, "fr.internal_error"),
	}
}

// frReasonText returns the message for a single reason, or "" for an unknown
// code (matching the old template's silent fall-through).
func frReasonText(ctx context.Context, reason string) string {
	return frReasonTexts(ctx)[reason]
}

// allFuckRedditReasons returns every reason code in a stable display order, for
// the /debug "Error Render Variants" puppet. New reasons must be appended here
// (and to frReasonTexts) so the debug index covers them.
func allFuckRedditReasons() []string {
	return []string{
		"upstream_disabled",
		"quota_exhausted",
		"hr_l1",
		"hr_l2",
		"hr_l3",
		"hr_redis_down",
		"totp_replay",
		"auth_locked",
		"unsafe_env",
		"internal_error",
	}
}

// frReasonIsStatic reports whether the reason is a one-shot auth-gate verdict
// (no server-side countdown, no /api/status mirror). The /fuckreddit page must
// suppress the countdown row AND the status poller for these — otherwise the
// poller sees an empty current_reason on its first tick and yanks the user back
// to "/", as if the degrade had cleared.
func frReasonIsStatic(reason string) bool {
	switch reason {
	case "auth_locked", "unsafe_env", "totp_replay", "internal_error", "upstream_disabled":
		return true
	}
	return false
}

// --- archive hub helpers ---

// archiveSortBase builds the base href (ending in "sort=") for the /archive sort
// tabs, carrying the active search query string when a search is in progress.
func archiveSortBase(search bool, qs string) string {
	if search && qs != "" {
		return "/archive?" + qs + "&sort="
	}
	return "/archive?sort="
}

// archiveSubCardTitle composes the title attr for an archive sub card: the sub
// name plus optional NSFW/dead tags.
func archiveSubCardTitle(ctx context.Context, e ArchiveHubEntry) string {
	s := "r/" + e.Name
	if e.NSFW {
		s += T(ctx, "archive_hub.nsfw_tag")
	}
	if e.Dead {
		s += T(ctx, "archive_hub.dead_tag")
	}
	return s
}

// --- settings page helpers ---

// layoutOptions is the option value set the settings page iterates over (the
// old `list ...` literal).
var layoutOptions = []string{"card", "clean", "compact"}

// displayNoneIf returns "display:none" when cond holds, else "" — mirrors the
// old inline `style="{{ if ... }}display:none{{ end }}"`. SafeCSS bypasses
// templ's style sanitizer so the empty case renders as a bare style="" too.
func displayNoneIf(cond bool) templ.SafeCSS {
	if cond {
		return templ.SafeCSS("display:none")
	}
	return templ.SafeCSS("")
}

// subView is the {name, posts} shape the sub-picker JS consumes via JSONScript.
type subView struct {
	Name  string `json:"name"`
	Posts int64  `json:"posts"`
}

// settingsTopSubs adapts the archived-sub stats into the JS sub shape used to
// seed window._topSubs / window._allSubs post counts.
func settingsTopSubs(stats []SubredditStatView) []subView {
	out := make([]subView, 0, len(stats))
	for _, s := range stats {
		out = append(out, subView{Name: s.Name, Posts: s.PostCount})
	}
	return out
}

// orDash returns s, or an em dash when s is empty (the old `{{ or .X "—" }}`).
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// tokenClass picks the per-token <li> class from its budget state.
func tokenClass(hasBudget bool) string {
	if hasBudget {
		return "token-ok"
	}
	return "token-empty"
}

// metaDescription returns the page's <meta name="description"> content:
// a per-page override (used by archive pages to list the actual subreddits this
// instance mirrors) or the i18n default. Threaded through the layout so the
// global meta tag stays a single tag — multiple description metas just confuse
// crawlers.
func metaDescription(ctx context.Context, p BasePage) string {
	if p.MetaDescription != "" {
		return p.MetaDescription
	}
	return T(ctx, "meta.description", p.BrandName)
}

// robotsMeta returns the page's <meta name="robots"> content. Default is the
// safe noindex,nofollow — only pages that explicitly set Indexable (the archive
// surfaces, gated behind SEO.AllowIndexing) opt in.
func robotsMeta(p BasePage) string {
	if p.Indexable {
		return "index, follow"
	}
	return "noindex, nofollow"
}

// ShowThemeStylesheet is the exported counterpart to showThemeStylesheet, used
// by the handler-side auth gate which renders outside the templ layout.
func ShowThemeStylesheet(theme string) bool { return showThemeStylesheet(theme) }

// showThemeStylesheet reports whether a per-theme stylesheet <link> should be
// emitted. style.css already ships dark + tokyoNight (and "system" defers to the
// OS), so those three never need an extra sheet.
func showThemeStylesheet(theme string) bool {
	switch theme {
	case "", "system", "dark", "tokyoNight":
		return false
	}
	return true
}
