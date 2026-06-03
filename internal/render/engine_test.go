package render

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/reddit"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := config.RenderConfig{BrandName: "TestBrand", ShowArchiveBadge: true}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return e
}

func TestNewEngine(t *testing.T) {
	e := newTestEngine(t)
	if e == nil {
		t.Fatal("New() returned nil")
	}
	// Every page renders through templ now; the engine only carries a
	// locale-bound translator per supported language (consumed via i18nContext).
	if e.translators == nil {
		t.Fatal("translators should not be nil")
	}
	if len(e.translators) != len(SupportedLangs) {
		t.Errorf("translator sets = %d, want %d", len(e.translators), len(SupportedLangs))
	}
	for _, lang := range SupportedLangs {
		if e.translators[lang] == nil {
			t.Errorf("translators[%s] is nil", lang)
		}
	}
}

func TestRenderError(t *testing.T) {
	e := newTestEngine(t)
	w := httptest.NewRecorder()
	e.RenderError(w, "en", "Something went wrong", 500)

	resp := w.Result()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Something went wrong") {
		t.Errorf("body should contain error message, got %d bytes", len(body))
	}
	if !strings.Contains(body, "TestBrand") {
		t.Errorf("body should contain brand name TestBrand")
	}
}

func TestRenderError404(t *testing.T) {
	e := newTestEngine(t)
	w := httptest.NewRecorder()
	e.RenderError(w, "en", "Not Found", 404)

	if w.Result().StatusCode != 404 {
		t.Errorf("status = %d, want 404", w.Result().StatusCode)
	}
	if !strings.Contains(w.Body.String(), "Not Found") {
		t.Error("body should contain 'Not Found'")
	}
}

func TestRenderSubreddit(t *testing.T) {
	e := newTestEngine(t)
	var buf bytes.Buffer

	data := SubredditPageData{
		BasePage: BasePage{
			URL:       "/r/golang",
			BrandName: "TestBrand",
			Version:   "0.1.0",
			Prefs:     reddit.Preferences{},
		},
		Sub: reddit.Subreddit{
			Name:        "golang",
			Title:       "The Go Programming Language",
			Description: "For discussion about Go",
		},
		Posts: []reddit.Post{
			{
				ID:        "abc123",
				Title:     "Test Post",
				Community: "golang",
				PostType:  "self",
				Score:     [2]string{"42", "42"},
				Comments:  [2]string{"5", "5"},
				Author:    reddit.Author{Name: "testuser"},
				RelTime:   "2h ago",
			},
		},
		Sort: [2]string{"hot", ""},
	}

	err := e.RenderSubreddit(&buf, data)
	if err != nil {
		t.Fatalf("RenderSubreddit() error: %v", err)
	}
	html := buf.String()
	if html == "" {
		t.Fatal("RenderSubreddit() produced empty output")
	}
	if !strings.Contains(html, "TestBrand") {
		t.Error("output should contain brand name")
	}
}

func TestRenderPost(t *testing.T) {
	e := newTestEngine(t)
	var buf bytes.Buffer

	data := PostPageData{
		BasePage: BasePage{
			URL:       "/r/golang/comments/abc/test",
			BrandName: "TestBrand",
			Version:   "0.1.0",
		},
		Post: reddit.Post{
			ID:        "abc",
			Title:     "A Test Post",
			Community: "golang",
			PostType:  "self",
			Score:     [2]string{"100", "100"},
			Comments:  [2]string{"10", "10"},
			Author:    reddit.Author{Name: "poster"},
			RelTime:   "1h ago",
		},
		Comments: []reddit.Comment{
			{
				ID:      "c1",
				Kind:    "t1",
				Body:    "Great post!",
				Author:  reddit.Author{Name: "commenter"},
				Score:   [2]string{"5", "5"},
				RelTime: "30m ago",
			},
		},
		Sort: "confidence",
	}

	err := e.RenderPost(&buf, data)
	if err != nil {
		t.Fatalf("RenderPost() error: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("RenderPost() produced empty output")
	}
}

func TestRenderPostTimeMachineBadge(t *testing.T) {
	build := func(removed bool, lang string, comments []reddit.Comment) PostPageData {
		return PostPageData{
			BasePage: BasePage{
				URL:       "/r/golang/comments/abc/test",
				BrandName: "TestBrand",
				Version:   "0.1.0",
				Prefs:     reddit.Preferences{Lang: lang},
			},
			Post: reddit.Post{
				ID:        "abc",
				Title:     "A Test Post",
				Community: "golang",
				PostType:  "self",
				Score:     [2]string{"100", "100"},
				Comments:  [2]string{"10", "10"},
				Author:    reddit.Author{Name: "poster"},
				RelTime:   "1h ago",
				Removed:   removed,
			},
			Comments: comments,
			Sort:     "confidence",
		}
	}

	render := func(t *testing.T, d PostPageData) string {
		t.Helper()
		e := newTestEngine(t)
		var buf bytes.Buffer
		if err := e.RenderPost(&buf, d); err != nil {
			t.Fatalf("RenderPost() error: %v", err)
		}
		return buf.String()
	}

	t.Run("post removed en shows badge", func(t *testing.T) {
		html := render(t, build(true, "en", nil))
		if !strings.Contains(html, `class="time-machine-inline"`) {
			t.Errorf("expected time-machine-inline class in output")
		}
		if !strings.Contains(html, "Time Machine") {
			t.Errorf("expected 'Time Machine' label in output")
		}
	})

	t.Run("post not removed has no badge", func(t *testing.T) {
		html := render(t, build(false, "en", nil))
		if strings.Contains(html, "time-machine-inline") {
			t.Errorf("did not expect time-machine-inline class when Removed=false")
		}
	})

	t.Run("removed comment shows badge", func(t *testing.T) {
		comments := []reddit.Comment{
			{
				ID:      "c1",
				Kind:    "t1",
				Body:    "Gone.",
				Author:  reddit.Author{Name: "commenter"},
				Score:   [2]string{"5", "5"},
				RelTime: "30m ago",
				Removed: true,
			},
		}
		d := build(false, "en", comments)
		html := render(t, d)
		if strings.Count(html, "time-machine-inline") < 1 {
			t.Errorf("expected time-machine-inline class in comment area")
		}
		// Comment id should be present so we know the comment actually rendered.
		if !strings.Contains(html, `id="c1"`) {
			t.Errorf("expected comment c1 to be rendered")
		}
	})

	t.Run("zh locale uses 时光机", func(t *testing.T) {
		html := render(t, build(true, "zh", nil))
		if !strings.Contains(html, "时光机") {
			t.Errorf("expected '时光机' label in zh locale output")
		}
	})
}

func TestRenderSearch(t *testing.T) {
	e := newTestEngine(t)
	var buf bytes.Buffer

	data := SearchPageData{
		BasePage: BasePage{
			URL:       "/search",
			BrandName: "TestBrand",
			Version:   "0.1.0",
		},
		Posts: nil,
		Params: reddit.SearchParams{
			Query: "test query",
		},
	}

	err := e.RenderSearch(&buf, data)
	if err != nil {
		t.Fatalf("RenderSearch() error: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("RenderSearch() produced empty output")
	}
}

func TestRenderSettings(t *testing.T) {
	e := newTestEngine(t)
	var buf bytes.Buffer

	data := SettingsPageData{
		BasePage: BasePage{
			URL:       "/settings",
			BrandName: "TestBrand",
			Version:   "0.1.0",
		},
	}

	err := e.RenderSettings(&buf, data)
	if err != nil {
		t.Fatalf("RenderSettings() error: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("RenderSettings() produced empty output")
	}
}

func TestStaticHandler(t *testing.T) {
	e := newTestEngine(t)
	h := e.StaticHandler()
	if h == nil {
		t.Fatal("StaticHandler() returned nil")
	}
}
