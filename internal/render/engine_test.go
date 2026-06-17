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
	// All errors now route through the /fuckreddit design — the page renders
	// the localized internal_error reason and the broken-heart icon.
	if !strings.Contains(body, "Something went wrong") {
		t.Errorf("body should contain the internal_error reason text, got %d bytes", len(body))
	}
	if !strings.Contains(body, "heart-broken") {
		t.Error("body should contain the unified heart-broken SVG")
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
	body := w.Body.String()
	if !strings.Contains(body, "heart-broken") {
		t.Error("body should render the unified fuckreddit broken-heart design")
	}
	if !strings.Contains(body, "All sources exhausted") {
		t.Error("body should render the fuckreddit 'exhausted' legend")
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

// TestRenderPostCloudCheckHint pins the cached-locally cloud-check behaviour:
//   - the SVG appears next to the comment count only when HasLocalComments is
//     set (a local copy of the thread exists);
//   - the archived-time badge renders the same cloud-check in place of the
//     literal word "archived", keeping the relative time but dropping the text.
func TestRenderPostCloudCheckHint(t *testing.T) {
	e := newTestEngine(t)
	build := func(local bool) PostPageData {
		return PostPageData{
			BasePage: BasePage{URL: "/r/golang/comments/abc/test", BrandName: "TestBrand", Version: "0.1.0"},
			Post: reddit.Post{
				ID: "abc", Title: "A Test Post", Community: "golang", PostType: "self",
				Score: [2]string{"100", "100"}, Comments: [2]string{"10", "10"},
				Author:          reddit.Author{Name: "poster"},
				RelTime:         "1h ago",
				ArchivedRelTime: "2h ago",
				ArchivedTime:    "2026-06-17 17:00 UTC",
			},
			Comments:         []reddit.Comment{{ID: "c1", Kind: "t1", Body: "Hi", Author: reddit.Author{Name: "u"}, Score: [2]string{"1", "1"}}},
			Sort:             "confidence",
			HasLocalComments: local,
		}
	}

	render := func(t *testing.T, d PostPageData) string {
		t.Helper()
		var buf bytes.Buffer
		if err := e.RenderPost(&buf, d); err != nil {
			t.Fatalf("RenderPost() error: %v", err)
		}
		return buf.String()
	}

	t.Run("archived badge swaps word for icon", func(t *testing.T) {
		body := render(t, build(false))
		// The archived-time badge keeps the relative time but no longer prints
		// the literal word "archived" as visible text — it's the cloud-check now.
		if !strings.Contains(body, `class="cloud-check"`) {
			t.Error("archived badge should render the cloud-check SVG")
		}
		if !strings.Contains(body, "2h ago") {
			t.Error("archived badge should still show the relative time")
		}
		idx := strings.Index(body, `class="archived"`)
		if idx < 0 {
			t.Fatal("archived span missing")
		}
		span := body[idx:min(idx+400, len(body))]
		if strings.Contains(span, ">archived ") {
			t.Error("archived badge should not print the literal word 'archived' as visible text")
		}
	})

	t.Run("comment count icon gated on HasLocalComments", func(t *testing.T) {
		with := render(t, build(true))
		without := render(t, build(false))
		ci := strings.Index(with, `id="comment_count"`)
		if ci < 0 {
			t.Fatal("comment_count missing")
		}
		if !strings.Contains(with[ci:min(ci+300, len(with))], "cloud-check") {
			t.Error("comment_count should carry the cloud-check when HasLocalComments=true")
		}
		ci2 := strings.Index(without, `id="comment_count"`)
		if strings.Contains(without[ci2:min(ci2+300, len(without))], "cloud-check") {
			t.Error("comment_count must NOT carry the cloud-check when HasLocalComments=false")
		}
	})
}

// TestRenderPostListCloudCheck pins the listing-card behaviour: the cloud-check
// sits inside the post_comments link only when the card's post carries
// HasLocalComments (an archived comment thread exists for it).
func TestRenderPostListCloudCheck(t *testing.T) {
	e := newTestEngine(t)
	post := func(local bool) reddit.Post {
		return reddit.Post{
			ID: "abc", Title: "A Test Post", Community: "golang", PostType: "self",
			Permalink:        "/r/golang/comments/abc/a_test_post/",
			Score:            [2]string{"100", "100"},
			Comments:         [2]string{"27", "27"},
			Author:           reddit.Author{Name: "poster"},
			RelTime:          "1h ago",
			HasLocalComments: local,
		}
	}
	render := func(t *testing.T, p reddit.Post) string {
		t.Helper()
		var buf bytes.Buffer
		if err := e.RenderPostList(&buf, []reddit.Post{p}, reddit.Preferences{}); err != nil {
			t.Fatalf("RenderPostList: %v", err)
		}
		return buf.String()
	}

	with := render(t, post(true))
	ci := strings.Index(with, `class="post_comments"`)
	if ci < 0 {
		t.Fatal("post_comments link missing")
	}
	if !strings.Contains(with[ci:min(ci+300, len(with))], "cloud-check") {
		t.Error("post_comments should carry the cloud-check when HasLocalComments=true")
	}

	without := render(t, post(false))
	ci2 := strings.Index(without, `class="post_comments"`)
	if strings.Contains(without[ci2:min(ci2+300, len(without))], "cloud-check") {
		t.Error("post_comments must NOT carry the cloud-check when HasLocalComments=false")
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

// TestLongVideoGateRendering covers the two surfaces independently:
//
//   - Listing view (RenderPostList): a >5min clip renders the long-video gate
//     placeholder instead of a live <video>, so a feed full of long clips
//     never auto-buffers a single byte. Short clips stay as live videos.
//
//   - Permalink view (RenderPost): the user explicitly opened the post to
//     watch it, so the <video> element is always rendered — but for long
//     clips it carries `preload="none"` and the `&long=1` priority marker
//     so nothing buffers until the user hits play and the backend gate
//     deprioritizes the resulting CDN download.
func TestLongVideoGateRendering(t *testing.T) {
	e := newTestEngine(t)

	listing := func(dur float64) string {
		var buf bytes.Buffer
		post := reddit.Post{
			ID:        "v1",
			Title:     "Clip",
			Community: "videos",
			PostType:  "video",
			Score:     [2]string{"1", "1"},
			Comments:  [2]string{"0", "0"},
			Author:    reddit.Author{Name: "u"},
			RelTime:   "1h ago",
			Media: reddit.Media{
				URL:      "https://v.redd.it/abc/DASH_720.mp4",
				Duration: dur,
			},
		}
		if err := e.RenderPostList(&buf, []reddit.Post{post}, reddit.Preferences{}); err != nil {
			t.Fatalf("RenderPostList: %v", err)
		}
		return buf.String()
	}

	long := listing(600)
	if !strings.Contains(long, "long-video-gate") {
		t.Errorf("long video in listing should render the gate placeholder")
	}
	if strings.Contains(long, "<video") {
		t.Errorf("long video in listing must not render a live <video> (would auto-buffer)")
	}
	if !strings.Contains(long, "long=1") {
		t.Errorf("listing gate URL should carry long=1 priority marker")
	}

	short := listing(60)
	if strings.Contains(short, "long-video-gate") {
		t.Errorf("short video must not render the long-video gate")
	}
	if !strings.Contains(short, "<video") {
		t.Errorf("short video in listing should render a live <video>")
	}

	permalink := func(dur float64) string {
		var buf bytes.Buffer
		data := PostPageData{
			BasePage: BasePage{URL: "/r/x/comments/v1/clip", BrandName: "TestBrand", Version: "0.1.0"},
			Post: reddit.Post{
				ID:        "v1",
				Title:     "Clip",
				Community: "videos",
				PostType:  "video",
				Score:     [2]string{"1", "1"},
				Comments:  [2]string{"0", "0"},
				Author:    reddit.Author{Name: "u"},
				RelTime:   "1h ago",
				Media: reddit.Media{
					URL:      "https://v.redd.it/abc/DASH_720.mp4",
					Duration: dur,
				},
			},
		}
		if err := e.RenderPost(&buf, data); err != nil {
			t.Fatalf("RenderPost: %v", err)
		}
		return buf.String()
	}

	longPerma := permalink(600)
	if strings.Contains(longPerma, "long-video-gate") {
		t.Errorf("permalink page should NOT render the gate — user explicitly opened the post")
	}
	if !strings.Contains(longPerma, "<video") {
		t.Errorf("permalink page should render a live <video> element for long clips")
	}
	if !strings.Contains(longPerma, `preload="none"`) {
		t.Errorf("long video permalink must use preload=none so bytes only fetch on user gesture")
	}
	if !strings.Contains(longPerma, "long=1") {
		t.Errorf("long video permalink src must carry long=1 for backend priority deprioritization")
	}

	shortPerma := permalink(60)
	if strings.Contains(shortPerma, `preload="none"`) {
		t.Errorf("short video permalink must not gate buffering")
	}
	if strings.Contains(shortPerma, "long=1") {
		t.Errorf("short video permalink must not carry long=1")
	}
}

func TestStaticHandler(t *testing.T) {
	e := newTestEngine(t)
	h := e.StaticHandler()
	if h == nil {
		t.Fatal("StaticHandler() returned nil")
	}
}
