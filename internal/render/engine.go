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
	pages map[string]*template.Template
	cfg   config.RenderConfig
}

// sharedTemplates are parsed into every page template set.
var sharedTemplates = []string{
	"templates/base.html",
	"templates/partials.html",
	"templates/comment.html",
}

// pageTemplates maps a page name to its template file.
var pageTemplates = map[string]string{
	"subreddit.html": "templates/subreddit.html",
	"post.html":      "templates/post.html",
	"search.html":    "templates/search.html",
	"user.html":      "templates/user.html",
	"settings.html":  "templates/settings.html",
	"archive.html":   "templates/archive.html",
	"error.html":     "templates/error.html",
}

func New(cfg config.RenderConfig) (*Engine, error) {
	pages := make(map[string]*template.Template, len(pageTemplates))
	for name, pagePath := range pageTemplates {
		files := make([]string, 0, len(sharedTemplates)+1)
		files = append(files, sharedTemplates...)
		files = append(files, pagePath)
		tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pages[name] = tmpl
	}
	return &Engine{pages: pages, cfg: cfg}, nil
}

type BasePage struct {
	URL       string
	Prefs     reddit.Preferences
	BrandName string
	Version   string
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

type ArchivePageData struct {
	BasePage
	Sub        string
	Posts      []reddit.Post
	TotalPosts int64
	Page       int
	TotalPages int
	HasPrev    bool
	HasNext    bool
}

type TokenView struct {
	Index         int
	Backend       string
	Kind          string // "static" or "dynamic"
	RateRemaining int
	RateReset     string // relative: "in 5m30s" or "expired"
	HasBudget     bool
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
	IsCountdown    bool
	IsDebug        bool
	Tokens         []TokenView
	UAList         []string
	UACurrentIndex int
	UAFetchedAt    string
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

func (e *Engine) renderPage(w io.Writer, name string, data any) error {
	tmpl, ok := e.pages[name]
	if !ok {
		return fmt.Errorf("template %q not found", name)
	}
	return tmpl.ExecuteTemplate(w, name, data)
}

func (e *Engine) RenderSubreddit(w io.Writer, data SubredditPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "subreddit.html", data)
}

func (e *Engine) RenderPostList(w io.Writer, posts []reddit.Post, prefs reddit.Preferences) error {
	tmpl := e.pages["subreddit.html"]
	if tmpl == nil {
		return fmt.Errorf("subreddit template not found")
	}
	for i, p := range posts {
		if i > 0 {
			io.WriteString(w, `<hr class="sep" />`)
		}
		data := map[string]any{"Post": p, "Prefs": prefs}
		if err := tmpl.ExecuteTemplate(w, "post_in_list", data); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) RenderPost(w io.Writer, data PostPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "post.html", data)
}

func (e *Engine) RenderSearch(w io.Writer, data SearchPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "search.html", data)
}

func (e *Engine) RenderUser(w io.Writer, data UserPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "user.html", data)
}

func (e *Engine) RenderArchive(w io.Writer, data ArchivePageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "archive.html", data)
}

func (e *Engine) RenderSettings(w io.Writer, data SettingsPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "settings.html", data)
}

type DebugData struct {
	Details        []string
	TokenBudget    int
	Tokens         []TokenView
	UAList         []string
	UACurrentIndex int
	UAFetchedAt    string
	PrefetchEvents []PrefetchEventView
}

func (e *Engine) RenderDebug(w io.Writer, msg string, d DebugData) {
	data := ErrorPageData{
		BasePage:       e.basePage("", reddit.Preferences{}),
		Message:        msg,
		StatusCode:     200,
		Details:        d.Details,
		TokenBudget:    d.TokenBudget,
		IsDebug:        true,
		Tokens:         d.Tokens,
		UAList:         d.UAList,
		UACurrentIndex: d.UACurrentIndex,
		UAFetchedAt:    d.UAFetchedAt,
		PrefetchEvents: d.PrefetchEvents,
	}
	e.renderPage(w, "error.html", data)
}

func (e *Engine) RenderCountdown(w io.Writer, prefs reddit.Preferences, remaining int, resetSeconds int, windowSeconds int) {
	data := ErrorPageData{
		BasePage:      e.basePage("", prefs),
		StatusCode:    200,
		TokenBudget:   remaining,
		ResetSeconds:  resetSeconds,
		WindowSeconds: windowSeconds,
		IsCountdown:   true,
	}
	e.renderPage(w, "error.html", data)
}

func (e *Engine) RenderRateLimit(w http.ResponseWriter, resetSeconds int, details []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	data := ErrorPageData{
		BasePage:     e.basePage("", reddit.Preferences{}),
		Message:      "All upstreams are rate-limited, please try again later",
		StatusCode:   http.StatusTooManyRequests,
		Details:      details,
		ResetSeconds: resetSeconds,
	}
	e.renderPage(w, "error.html", data)
}

func (e *Engine) RenderError(w http.ResponseWriter, msg string, statusCode int, details ...string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	data := ErrorPageData{
		BasePage:   e.basePage("", reddit.Preferences{}),
		Message:    msg,
		StatusCode: statusCode,
		Details:    details,
	}
	e.renderPage(w, "error.html", data)
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
