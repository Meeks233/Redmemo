package render

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"

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
}

type PostPageData struct {
	BasePage
	Post            reddit.Post
	Comments        []reddit.Comment
	Sort            string
	CommentQuery    string
	SingleThread    bool
	URLWithoutQuery string
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
}

type ErrorPageData struct {
	BasePage
	Message    string
	StatusCode int
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

func (e *Engine) RenderSettings(w io.Writer, data SettingsPageData) error {
	if data.BrandName == "" {
		data.BrandName = e.cfg.BrandName
	}
	return e.renderPage(w, "settings.html", data)
}

func (e *Engine) RenderError(w http.ResponseWriter, msg string, statusCode int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	data := ErrorPageData{
		BasePage:   e.basePage("", reddit.Preferences{}),
		Message:    msg,
		StatusCode: statusCode,
	}
	e.renderPage(w, "error.html", data)
}

func (e *Engine) StaticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	return http.FileServerFS(sub)
}
