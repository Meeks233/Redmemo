package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPathNormalize_DoubleSlash(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	})
	h := pathNormalize(inner)

	tests := []struct {
		path string
		want string
	}{
		{"/r//golang", "/r/golang"},
		{"///r///test///", "/r/test"}, // also triggers trailing-slash redirect
		{"/", "/"},
		{"/r/golang", "/r/golang"},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusMovedPermanently {
			loc := rec.Header().Get("Location")
			if loc != tt.want && loc != tt.want+"" {
				t.Errorf("pathNormalize(%q) redirected to %q, want %q", tt.path, loc, tt.want)
			}
			continue
		}
		got := rec.Body.String()
		if got != tt.want {
			t.Errorf("pathNormalize(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestPathNormalize_TrailingSlash(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	})
	h := pathNormalize(inner)

	req := httptest.NewRequest("GET", "/r/golang/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("expected 301 redirect, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/r/golang" {
		t.Errorf("redirect to %q, want /r/golang", loc)
	}
}

func TestPathNormalize_RootNotRedirected(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := pathNormalize(inner)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusMovedPermanently {
		t.Error("root path should not be redirected")
	}
}

func TestReadPreferences_WithCookies(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	req.AddCookie(&http.Cookie{Name: "layout", Value: "compact"})
	req.AddCookie(&http.Cookie{Name: "front_page", Value: "all"})
	req.AddCookie(&http.Cookie{Name: "wide", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "comment_sort", Value: "new"})
	req.AddCookie(&http.Cookie{Name: "post_sort", Value: "top"})
	req.AddCookie(&http.Cookie{Name: "show_nsfw", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "blur_nsfw", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "blur_spoiler", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "use_hls", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "hide_awards", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "hide_score", Value: "on"})
	req.AddCookie(&http.Cookie{Name: "fixed_navbar", Value: "off"})
	req.AddCookie(&http.Cookie{Name: "video_quality", Value: "worst"})

	prefs := readPreferences(req)

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"Theme", prefs.Theme, "dark"},
		{"Layout", prefs.Layout, "compact"},
		{"FrontPage", prefs.FrontPage, "all"},
		{"Wide", prefs.Wide, "on"},
		{"CommentSort", prefs.CommentSort, "new"},
		{"PostSort", prefs.PostSort, "top"},
		{"ShowNSFW", prefs.ShowNSFW, "on"},
		{"BlurNSFW", prefs.BlurNSFW, "on"},
		{"BlurSpoiler", prefs.BlurSpoiler, "on"},
		{"UseHLS", prefs.UseHLS, "on"},
		{"HideAwards", prefs.HideAwards, "on"},
		{"HideScore", prefs.HideScore, "on"},
		{"FixedNavbar", prefs.FixedNavbar, "off"},
		{"VideoQuality", prefs.VideoQuality, "worst"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("prefs.%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestReadPreferences_Defaults(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	prefs := readPreferences(req)

	if prefs.FixedNavbar != "on" {
		t.Errorf("FixedNavbar default = %q, want %q", prefs.FixedNavbar, "on")
	}
	if prefs.Theme != "" {
		t.Errorf("Theme default = %q, want empty", prefs.Theme)
	}
	if prefs.Subscriptions != nil {
		t.Errorf("Subscriptions default = %v, want nil", prefs.Subscriptions)
	}
	if prefs.Filters != nil {
		t.Errorf("Filters default = %v, want nil", prefs.Filters)
	}
}

func TestReadPreferences_MultiCookieSubscriptions(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "subscriptions", Value: "golang+rust"})
	req.AddCookie(&http.Cookie{Name: "subscriptions1", Value: "python+java"})
	req.AddCookie(&http.Cookie{Name: "subscriptions2", Value: "typescript"})

	prefs := readPreferences(req)
	want := []string{"golang", "rust", "python", "java", "typescript"}

	if len(prefs.Subscriptions) != len(want) {
		t.Fatalf("Subscriptions len = %d, want %d: %v", len(prefs.Subscriptions), len(want), prefs.Subscriptions)
	}
	for i, s := range prefs.Subscriptions {
		if s != want[i] {
			t.Errorf("Subscriptions[%d] = %q, want %q", i, s, want[i])
		}
	}
}

func TestReadPreferences_MultiCookieFilters(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "filters", Value: "memes+pics"})
	req.AddCookie(&http.Cookie{Name: "filters1", Value: "funny"})

	prefs := readPreferences(req)
	want := []string{"memes", "pics", "funny"}

	if len(prefs.Filters) != len(want) {
		t.Fatalf("Filters len = %d, want %d: %v", len(prefs.Filters), len(want), prefs.Filters)
	}
	for i, f := range prefs.Filters {
		if f != want[i] {
			t.Errorf("Filters[%d] = %q, want %q", i, f, want[i])
		}
	}
}

func TestRecovery_HandlerPanics(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	h := recovery(panicking)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	// Should not panic
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("recovery status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestLogging_SetsStatusCode(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	h := logging(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("logging passed through status %d, want %d", rec.Code, http.StatusNotFound)
	}
}
