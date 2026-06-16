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

func testHandler() *Handler {
	return &Handler{siteDefaults: make(map[string]string)}
}

func TestReadPreferences_ThemeCookie(t *testing.T) {
	h := testHandler()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})

	prefs := h.readPreferences(req)
	if prefs.Theme != "dark" {
		t.Errorf("Theme = %q, want %q", prefs.Theme, "dark")
	}
}

func TestReadPreferences_ThemeFromSiteDefaults(t *testing.T) {
	h := testHandler()
	h.siteDefaults["theme"] = "nord"
	req := httptest.NewRequest("GET", "/", nil)

	prefs := h.readPreferences(req)
	if prefs.Theme != "nord" {
		t.Errorf("Theme = %q, want %q", prefs.Theme, "nord")
	}
}

func TestReadPreferences_ThemeCookieOverridesSiteDefault(t *testing.T) {
	h := testHandler()
	h.siteDefaults["theme"] = "nord"
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dracula"})

	prefs := h.readPreferences(req)
	if prefs.Theme != "dracula" {
		t.Errorf("Theme = %q, want %q", prefs.Theme, "dracula")
	}
}

func TestReadPreferences_OtherPrefsFromSiteDefaults(t *testing.T) {
	h := testHandler()
	h.siteDefaults["layout"] = "compact"
	h.siteDefaults["wide"] = "on"
	h.siteDefaults["front_page"] = "all"
	req := httptest.NewRequest("GET", "/", nil)

	prefs := h.readPreferences(req)
	if prefs.Layout != "compact" {
		t.Errorf("Layout = %q, want %q", prefs.Layout, "compact")
	}
	if prefs.Wide != "on" {
		t.Errorf("Wide = %q, want %q", prefs.Wide, "on")
	}
	if prefs.FrontPage != "all" {
		t.Errorf("FrontPage = %q, want %q", prefs.FrontPage, "all")
	}
}

func TestReadPreferences_Defaults(t *testing.T) {
	h := testHandler()
	req := httptest.NewRequest("GET", "/", nil)
	prefs := h.readPreferences(req)

	if prefs.FixedNavbar != "on" {
		t.Errorf("FixedNavbar default = %q, want %q", prefs.FixedNavbar, "on")
	}
	if prefs.Theme != "" {
		t.Errorf("Theme default = %q, want empty", prefs.Theme)
	}
}

// TestReadPreferences_FrontPageSubsDefault pins the query-driven homepage
// switch: with no toggle, an unset or empty front_page_subs resolves to "" so
// handleFrontPage redirects to /archive (disabled), while a stored query is
// honored verbatim. The default is "" — no query means no homepage.
func TestReadPreferences_FrontPageSubsDefault(t *testing.T) {
	t.Run("unset means disabled", func(t *testing.T) {
		h := testHandler()
		prefs := h.readPreferences(httptest.NewRequest("GET", "/", nil))
		if prefs.FrontPageSubs != "" {
			t.Errorf("FrontPageSubs = %q, want empty (disabled)", prefs.FrontPageSubs)
		}
	})
	t.Run("explicit query is honored", func(t *testing.T) {
		h := testHandler()
		h.siteDefaults["front_page_subs"] = "sub:golang"
		prefs := h.readPreferences(httptest.NewRequest("GET", "/", nil))
		if prefs.FrontPageSubs != "sub:golang" {
			t.Errorf("FrontPageSubs = %q, want %q", prefs.FrontPageSubs, "sub:golang")
		}
	})
	t.Run("all renders a full feed", func(t *testing.T) {
		h := testHandler()
		h.siteDefaults["front_page_subs"] = "all"
		prefs := h.readPreferences(httptest.NewRequest("GET", "/", nil))
		if prefs.FrontPageSubs != "all" {
			t.Errorf("FrontPageSubs = %q, want %q", prefs.FrontPageSubs, "all")
		}
	})
}

// TestHandleFrontPage_DisabledRedirects verifies the query-driven homepage:
// with no front_page_subs configured the handler redirects to /archive before
// touching the post store (so a nil store is fine here).
func TestHandleFrontPage_DisabledRedirects(t *testing.T) {
	h := testHandler()
	h.cfg = configForTest()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	h.handleFrontPage(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (redirect)", rec.Code, http.StatusFound)
	}
	if loc := rec.Header().Get("Location"); loc != "/archive" {
		t.Errorf("Location = %q, want /archive", loc)
	}
}

func TestRecovery_HandlerPanics(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	h := recovery(panicking)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

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
