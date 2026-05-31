package render

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/reddit"
)

//go:embed static
var staticFS embed.FS

// assetETag is a content hash of every embedded static asset, computed once at
// startup and sent as the ETag for all of them. The display Version is hardcoded
// ("0.1.0") and never changes, so versioning asset URLs by it (or marking them
// "immutable") froze stale CSS/JS in browsers forever across rebuilds. With this
// ETag plus must-revalidate, browsers still cache but check in on every load:
// they get a cheap 304 while nothing changed, and the fresh file the instant a
// rebuild alters the embedded assets (the hash, and thus the ETag, changes).
var assetETag = computeAssetETag()

func computeAssetETag() string {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return `"0"`
	}
	h := sha256.New()
	_ = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		f, err := sub.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		io.WriteString(h, path) // include the name so renames also change the hash
		_, _ = io.Copy(h, f)
		return nil
	})
	return `"` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
}

type Engine struct {
	// translators is the locale-bound T function per language, used by the
	// templ render path (which reads it from context via i18nContext).
	translators map[string]func(key string, args ...any) string
	cfg         config.RenderConfig
}

func New(cfg config.RenderConfig) (*Engine, error) {
	locales, err := loadLocales()
	if err != nil {
		return nil, err
	}
	defaultLoc := locales[DefaultLang]

	translators := make(map[string]func(key string, args ...any) string, len(SupportedLangs))
	for _, lang := range SupportedLangs {
		translators[lang] = translator(locales[lang], defaultLoc)
	}
	return &Engine{translators: translators, cfg: cfg}, nil
}

type BasePage struct {
	URL       string
	Prefs     reddit.Preferences
	BrandName string
	Version   string
	// DegradedReason is set when the page is being served from local archive
	// instead of the live API because of the HR rate-limit layer or OAuth
	// quota exhaustion. Empty string = not degraded. When non-empty the
	// "degraded_banner" partial renders and links to /fuckreddit?reason=...
	DegradedReason string
}

type SubredditPageData struct {
	BasePage
	Sub                reddit.Subreddit
	Posts              []reddit.Post
	Sort               [2]string
	Ends               [2]string
	IsFiltered         bool
	AllPostsHiddenNSFW bool
	NoPosts            bool
	AllPostsFiltered   bool
	RedirectURL        string
	HomepageSort       string
	HasOAuth           bool
	IsOffline          bool
}

type PostPageData struct {
	BasePage
	Post            reddit.Post
	Comments        []reddit.Comment
	Sort            string
	CommentQuery    string
	SingleThread    bool
	URLWithoutQuery string
	HasOAuth        bool
	IsOffline       bool
}

type SearchPageData struct {
	BasePage
	Posts              []reddit.Post
	Subreddits         []reddit.Subreddit
	Params             reddit.SearchParams
	Sub                string
	IsFiltered         bool
	AllPostsHiddenNSFW bool
	NoPosts            bool
	AllPostsFiltered   bool
	IsOffline          bool
	// IsLocalOnly is true when the page was served entirely from the local
	// archive (offline fallback or upstream_disabled). Switches the "Load More"
	// footer to the offset-based infinite-scroll loader against /search itself.
	IsLocalOnly bool
	PageSize    int    // posts per partial=1 archive batch
	Interval    string // ScrollInterval pref, threaded through for the loader
}

type UserPageData struct {
	BasePage
	User               reddit.User
	Posts              []reddit.Post
	Listing            string
	Sort               [2]string
	Ends               [2]string
	IsFiltered         bool
	AllPostsHiddenNSFW bool
	NoPosts            bool
	AllPostsFiltered   bool
	RedirectURL        string
}

type SettingsPageData struct {
	BasePage
	PostCount      int64
	SubredditCount int64
	MediaCount     int64
	MediaSize      string
	OAuthEnabled   bool
	PrefetchSubs   []string
	// FrontPageQuery and PrefetchQuery are the backend-normalized filter strings
	// echoed into the settings inputs: FrontPageQuery is the homepage feed's
	// accepted sub: clause (e.g. "sub:cats+dogs-meta"), PrefetchQuery is the NP
	// crawl list in the simple "a+b+c" format. Both are produced server-side so
	// the page no longer needs JS to reconstruct or validate them.
	FrontPageQuery string
	PrefetchQuery  string
	SubredditStats []SubredditStatView
	ArchivedSubs   []string
	LiveSubs       []string
	SelectedCounts map[string]int
}

type ArchiveHubEntry struct {
	Name      string
	PostCount int64
	IconURL   string
	NSFW      bool
	Dead      bool // banned / private / quarantined upstream — local archive still accessible
}

type ArchiveAlphaGroup struct {
	Letter string
	Subs   []ArchiveHubEntry
}

// ArchiveSearchView holds the /archive search box state. The box now uses the
// shared e621-style query syntax (see docs/reddit-search.md), so the raw text is
// the only state — all constraints are encoded inside it.
type ArchiveSearchView struct {
	Query string // raw query box text (free text + e621-style constraints)
}

type ArchiveHubPageData struct {
	BasePage
	Sort        string // "new", "top", "all"
	Subs        []ArchiveHubEntry
	AlphaGroups []ArchiveAlphaGroup // populated only when Sort == "all"
	AlphaIndex  []string            // letters present, in display order (A-Z then "#")
	MinPosts    int                 // threshold used for new/top

	// Local archive search.
	SearchParams ArchiveSearchView

	// Populated only when a search is active.
	Search         bool
	SearchPosts    []reddit.Post
	SearchTotal    int64
	SearchPageSize int    // posts per partial=1 batch — drives infinite-loader's offset step
	SearchQS       string // URL-encoded raw query (no offset) for infinite-loader data-qs
	Interval       string // ScrollInterval pref, threaded through for the loader
}

type ArchivePageData struct {
	BasePage
	Sub                string
	Posts              []reddit.Post
	TotalPosts         int64
	PageSize           int    // posts per partial=1 batch — drives infinite-loader's offset step
	Interval           string // ScrollInterval pref, threaded through for the loader
	AllPostsHiddenNSFW bool
}

type TokenView struct {
	Index         int
	Backend       string
	Kind          string // "static" or "dynamic"
	RateRemaining int
	RateReset     string // relative: "in 5m30s" or "expired"
	HasBudget     bool
	UserAgent     string
	DeviceID      string
	Loid          string
	Session       string
	ExpiresIn     string
}

type PrefetchEventView struct {
	Time         string
	RelativeTime string
	Level        string
	Phase        string
	Message      string
}

type ErrorPageData struct {
	BasePage
	Message        string
	StatusCode     int
	Details        []string
	TokenBudget    int
	ResetSeconds   int
	WindowSeconds  int
	IsDebug        bool
	Tokens         []TokenView
	PrefetchStatus PrefetchStatusView
	PrefetchEvents []PrefetchEventView
}

type SubredditStatView struct {
	Name      string
	PostCount int64
}

func (e *Engine) basePage(url string, prefs reddit.Preferences) BasePage {
	return BasePage{
		URL:       url,
		Prefs:     prefs,
		BrandName: e.cfg.BrandName,
		Version:   "0.1.0",
	}
}

// filterNSFW is the single chokepoint that enforces the user's show_nsfw
// preference at the render boundary. SQL-side filtering already drops NSFW rows
// from local archive reads; this is the safety net for upstream Reddit
// responses (which always carry include_over_18=on so the archive can capture
// the full feed) and any future code path that bypasses the store.
//
// Returns the filtered slice plus a flag indicating that the input was
// non-empty but every post was hidden — used to show the "all NSFW hidden"
// banner without a second pass.
func filterNSFW(posts []reddit.Post, prefs reddit.Preferences) ([]reddit.Post, bool) {
	if prefs.ShowNSFW == "on" || len(posts) == 0 {
		return posts, false
	}
	kept := make([]reddit.Post, 0, len(posts))
	for _, p := range posts {
		if !p.Flags.NSFW {
			kept = append(kept, p)
		}
	}
	return kept, len(kept) == 0
}

func (e *Engine) RenderSubreddit(w io.Writer, data SubredditPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	var hidden bool
	data.Posts, hidden = filterNSFW(data.Posts, data.Prefs)
	if hidden {
		data.AllPostsHiddenNSFW = true
	}
	return subredditPage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

func (e *Engine) RenderPostList(w io.Writer, posts []reddit.Post, prefs reddit.Preferences) error {
	posts, _ = filterNSFW(posts, prefs)
	ctx := e.i18nContext(prefs.Lang)
	lazy := prefs.LazyMedia == "on"
	for i, p := range posts {
		if i > 0 {
			io.WriteString(w, `<hr class="sep" />`)
		}
		if err := postInList(p, prefs, lazy).Render(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) RenderPost(w io.Writer, data PostPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return postPage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

func (e *Engine) RenderSearch(w io.Writer, data SearchPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	var hidden bool
	data.Posts, hidden = filterNSFW(data.Posts, data.Prefs)
	if hidden {
		data.AllPostsHiddenNSFW = true
	}
	return searchPage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

// RenderSearchPostList renders just the search post-list fragment (the same
// markup the full search page produces for its results), used by the
// "Load More" button's partial=1 requests.
func (e *Engine) RenderSearchPostList(w io.Writer, posts []reddit.Post, prefs reddit.Preferences) error {
	posts, _ = filterNSFW(posts, prefs)
	return searchPostList(posts, prefs, prefs.LazyMedia == "on").Render(e.i18nContext(prefs.Lang), w)
}

func (e *Engine) RenderUser(w io.Writer, data UserPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	var hidden bool
	data.Posts, hidden = filterNSFW(data.Posts, data.Prefs)
	if hidden {
		data.AllPostsHiddenNSFW = true
	}
	return userPage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

func (e *Engine) RenderArchiveHub(w io.Writer, data ArchiveHubPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	data.SearchPosts, _ = filterNSFW(data.SearchPosts, data.Prefs)
	return archiveHubPage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

func (e *Engine) RenderArchive(w io.Writer, data ArchivePageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	var hidden bool
	data.Posts, hidden = filterNSFW(data.Posts, data.Prefs)
	if hidden {
		data.AllPostsHiddenNSFW = true
	}
	return archivePage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

func (e *Engine) RenderSettings(w io.Writer, data SettingsPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return settingsPage(data).Render(e.i18nContext(data.Prefs.Lang), w)
}

type PrefetchStatusView struct {
	Enabled     bool
	ActiveSubs  string
	L1Phase     string
	L1Progress  string
	L1Subs      string
	L1Cursors   []PrefetchCursorView
	L1NextCycle string
	L2Phase     string
	L2Sub       string
	L2Pending   int
	L5Phase     string
	L5Current   string
	L5Pending   int
	NPPhase     string
	NPCurrent   string
	QueueLen    int
}

type PrefetchCursorView struct {
	Sub    string
	Cursor string
}

type DebugData struct {
	Details        []string
	TokenBudget    int
	Tokens         []TokenView
	PrefetchStatus PrefetchStatusView
	PrefetchEvents []PrefetchEventView
}

func (e *Engine) RenderDebug(w io.Writer, msg string, prefs reddit.Preferences, d DebugData) {
	data := ErrorPageData{
		BasePage:       e.basePage("", prefs),
		Message:        msg,
		StatusCode:     200,
		Details:        d.Details,
		TokenBudget:    d.TokenBudget,
		IsDebug:        true,
		Tokens:         d.Tokens,
		PrefetchStatus: d.PrefetchStatus,
		PrefetchEvents: d.PrefetchEvents,
	}
	errorPage(data).Render(e.i18nContext(prefs.Lang), w)
}

func (e *Engine) RenderRateLimit(w http.ResponseWriter, lang string, resetSeconds int, details []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	prefs := reddit.Preferences{Lang: lang}
	data := ErrorPageData{
		BasePage:     e.basePage("", prefs),
		Message:      "All upstreams are rate-limited, please try again later",
		StatusCode:   http.StatusTooManyRequests,
		Details:      details,
		ResetSeconds: resetSeconds,
	}
	errorPage(data).Render(e.i18nContext(lang), w)
}

func (e *Engine) RenderError(w http.ResponseWriter, lang, msg string, statusCode int, details ...string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	prefs := reddit.Preferences{Lang: lang}
	data := ErrorPageData{
		BasePage:   e.basePage("", prefs),
		Message:    msg,
		StatusCode: statusCode,
		Details:    details,
	}
	errorPage(data).Render(e.i18nContext(lang), w)
}

type FuckRedditPageData struct {
	BasePage
	ResetSeconds int
	// Reason explains why the user landed here: "hr_l1"/"hr_l2"/"hr_l3"
	// (HR rate-limit cooldown), "hr_redis_down" (rate-limit store unreachable),
	// "quota_exhausted" (OAuth budget drained), or "" (no failure — render the
	// healthy state).
	Reason string
	// From is the reddit path the user was trying to reach (validated to
	// /r/, /user/, or /search prefixes). Empty when there is no origin
	// context. When set, the template renders an "Access Reddit directly"
	// escape hatch linking to https://www.reddit.com{From}.
	From string
	// Freeze pins the countdown to 99:99 and disables polling. Debug-only.
	Freeze bool
}

func (e *Engine) RenderFuckReddit(w http.ResponseWriter, prefs reddit.Preferences, resetSeconds int, reason, from string, freeze bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if reason != "" {
		w.Header().Set("X-Reason", reason)
	}
	data := FuckRedditPageData{
		BasePage:     e.basePage("", prefs),
		ResetSeconds: resetSeconds,
		Reason:       reason,
		From:         from,
		Freeze:       freeze,
	}
	fuckRedditPage(data).Render(e.i18nContext(prefs.Lang), w)
}

func AvailableThemes() []string {
	entries, _ := fs.ReadDir(staticFS, "static/themes")
	themes := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".css") {
			themes = append(themes, strings.TrimSuffix(name, ".css"))
		}
	}
	return themes
}

// IsSelectableTheme reports whether name is a concrete palette a user can pick
// as an auto day/night target: a real embedded theme that is neither "auto"
// (no fixed palette — it delegates) nor "system" (defers entirely to the OS).
func IsSelectableTheme(name string) bool {
	if name == "auto" || name == "system" {
		return false
	}
	for _, t := range AvailableThemes() {
		if t == name {
			return true
		}
	}
	return false
}

// autoThemeVars returns the CSS custom-property declarations from a theme's
// primary `.<name> { ... }` rule in its embedded stylesheet. Theme files lead
// with that block, so the inner of the first `{ ... }` is the palette.
func autoThemeVars(theme string) string {
	b, err := staticFS.ReadFile("static/themes/" + theme + ".css")
	if err != nil {
		return ""
	}
	s := string(b)
	open := strings.IndexByte(s, '{')
	if open < 0 {
		return ""
	}
	close := strings.IndexByte(s[open:], '}')
	if close < 0 {
		return ""
	}
	return strings.TrimSpace(s[open+1 : open+close])
}

// autoThemeCSS builds the stylesheet that powers the "auto" theme: it wakes the
// night palette by default and flips to the day palette when the OS reports
// light mode — mirroring the static auto.css structure but with user-chosen
// palettes. day/night must be validated (IsSelectableTheme) by the caller;
// empty values fall back to the light/black defaults.
func autoThemeCSS(day, night string) string {
	if !IsSelectableTheme(day) {
		day = "light"
	}
	if !IsSelectableTheme(night) {
		night = "black"
	}
	var b strings.Builder
	b.WriteString(".auto {\n")
	b.WriteString(autoThemeVars(night))
	b.WriteString("\n}\nhtml:has(> .auto) { color-scheme: dark; }\n")
	b.WriteString("@media (prefers-color-scheme: light) {\n.auto {\n")
	b.WriteString(autoThemeVars(day))
	b.WriteString("\n}\nhtml:has(> .auto) { color-scheme: light; }\n}\n")
	return b.String()
}

// autoThemeStyle wraps autoThemeCSS in a <style> element. The layout emits this
// as a raw templ node rather than writing the call inside a literal <style>
// tag: templ treats a <style> element's body as inert text, so an expression
// placed there would be printed verbatim instead of evaluated.
func autoThemeStyle(day, night string) string {
	return `<style type="text/css">` + autoThemeCSS(day, night) + `</style>`
}

func (e *Engine) StaticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	fs := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets live at stable URLs (/style.css, /quotaRing.js, ...). They
		// must NOT be "immutable": that, plus a never-changing display Version,
		// pinned stale CSS/JS in browsers across every rebuild. Tag them with a
		// build-content ETag and force revalidation instead — http.ServeContent
		// then answers conditional requests with 304 while nothing changed and the
		// fresh asset the moment a new build alters the embedded files.
		w.Header().Set("ETag", assetETag)
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		fs.ServeHTTP(w, r)
	})
}
