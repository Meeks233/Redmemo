package render

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/reddit"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Engine struct {
	// pages is indexed by language code, then by page name. Each language has
	// its own fully-parsed template set with a FuncMap bound to that locale.
	pages map[string]map[string]*template.Template
	// translators is the locale-bound T function per language, used by the
	// templ render path (which reads it from context via i18nContext).
	translators map[string]func(key string, args ...any) string
	cfg         config.RenderConfig
}

// sharedTemplates are parsed into every page template set. comment.html has
// been ported to templ (comment.templ); base/partials remain for the html
// pages not yet migrated.
var sharedTemplates = []string{
	"templates/base.html",
	"templates/partials.html",
}

// pageTemplates maps a page name to its template file. Every page has now been
// ported to templ; the map is empty and the html/template scaffolding below
// (pages/pageSet/renderPage, base.html, partials.html) is retained pending a
// separate decommission pass.
var pageTemplates = map[string]string{}

func New(cfg config.RenderConfig) (*Engine, error) {
	locales, err := loadLocales()
	if err != nil {
		return nil, err
	}
	defaultLoc := locales[DefaultLang]

	pages := make(map[string]map[string]*template.Template, len(SupportedLangs))
	translators := make(map[string]func(key string, args ...any) string, len(SupportedLangs))
	for _, lang := range SupportedLangs {
		translators[lang] = translator(locales[lang], defaultLoc)
		funcs := templateFuncs(locales[lang], defaultLoc, lang)
		langPages := make(map[string]*template.Template, len(pageTemplates))
		for name, pagePath := range pageTemplates {
			files := make([]string, 0, len(sharedTemplates)+1)
			files = append(files, sharedTemplates...)
			files = append(files, pagePath)
			tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, files...)
			if err != nil {
				return nil, fmt.Errorf("parse %s [%s]: %w", name, lang, err)
			}
			langPages[name] = tmpl
		}
		pages[lang] = langPages
	}
	return &Engine{pages: pages, translators: translators, cfg: cfg}, nil
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

// ArchiveSearchView holds the local-search form state for the /archive page.
type ArchiveSearchView struct {
	Query      string // free-text title query
	Time       string // "" (any) | "hour" | "day" | "week" | "month" | "year" | "custom"
	From       string // YYYY-MM-DD, used only when Time == "custom"
	To         string // YYYY-MM-DD, used only when Time == "custom"
	Type       string // "" (any) | "nsfw" | "sfw"
	Media      string // "" (any) | "image" | "video"
	Source     string // "+"-joined subreddit names; empty = any
	SourceMode string // "whitelist" (default) | "blacklist"
	Score      string // raw score threshold the user typed; empty = any
	ScoreOp    string // "gt" (default) | "lt"
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
	PickerSubs   []SubredditStatView // archived subs (name + count), sorted by count, for the Source picker

	// Populated only when a search is active.
	Search      bool
	SearchPosts []reddit.Post
	SearchTotal int64
	SearchPage  int
	SearchPages int
	SearchQS    string // URL-encoded query string (all filters, no page) for pagination links
}

type ArchivePageData struct {
	BasePage
	Sub                string
	Posts              []reddit.Post
	TotalPosts         int64
	Page               int
	TotalPages         int
	AllPostsHiddenNSFW bool
	HasPrev            bool
	HasNext            bool
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

// pageSet returns the template set for lang, falling back to DefaultLang when
// lang is unknown (e.g. an empty or stale cookie value).
func (e *Engine) pageSet(lang string) map[string]*template.Template {
	if set, ok := e.pages[lang]; ok {
		return set
	}
	return e.pages[DefaultLang]
}

func (e *Engine) renderPage(w io.Writer, lang, name string, data any) error {
	tmpl, ok := e.pageSet(lang)[name]
	if !ok {
		return fmt.Errorf("template %q not found", name)
	}
	return tmpl.ExecuteTemplate(w, name, data)
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

func (e *Engine) StaticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	fs := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		fs.ServeHTTP(w, r)
	})
}
