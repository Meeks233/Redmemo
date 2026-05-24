package render

import (
	"context"
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

// searchSorts is the search result sort option set.
var searchSorts = []string{"relevance", "hot", "top", "new", "comments"}

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

// searchAction builds the search form's action URL: scoped to the subreddit
// when root is a real "/r/<name>" prefix, otherwise the global "/search".
func searchAction(root string) string {
	if root != "" && root != "/r/" {
		return root + "/search"
	}
	return "/search"
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
		"quota_exhausted": T(ctx, "fr.quota_exhausted"),
		"hr_l1":           T(ctx, "fr.hr_l1"),
		"hr_l2":           T(ctx, "fr.hr_l2"),
		"hr_l3":           T(ctx, "fr.hr_l3"),
		"hr_redis_down":   T(ctx, "fr.hr_redis_down"),
	}
}

// frReasonText returns the message for a single reason, or "" for an unknown
// code (matching the old template's silent fall-through).
func frReasonText(ctx context.Context, reason string) string {
	return frReasonTexts(ctx)[reason]
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
