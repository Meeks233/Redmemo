package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/config"
)

func TestSetCookiePref(t *testing.T) {
	rec := httptest.NewRecorder()
	setCookiePref(rec, "theme", "dark")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != "theme" {
		t.Errorf("name = %q, want %q", c.Name, "theme")
	}
	if c.Value != "dark" {
		t.Errorf("value = %q, want %q", c.Value, "dark")
	}
	if c.Path != "/" {
		t.Errorf("path = %q, want %q", c.Path, "/")
	}
	if c.MaxAge != cookieMaxAge {
		t.Errorf("maxAge = %d, want %d", c.MaxAge, cookieMaxAge)
	}
	if !c.HttpOnly {
		t.Error("expected HttpOnly = true")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %d, want Lax (%d)", c.SameSite, http.SameSiteLaxMode)
	}
}

func TestClearCookiePref(t *testing.T) {
	rec := httptest.NewRecorder()
	clearCookiePref(rec, "theme")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.MaxAge != -1 {
		t.Errorf("maxAge = %d, want -1 (deletion)", c.MaxAge)
	}
}

func TestSetListCookie_Short(t *testing.T) {
	rec := httptest.NewRecorder()
	setListCookie(rec, "subscriptions", []string{"golang", "rust", "python"})

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	if cookies[0].Value != "golang+rust+python" {
		t.Errorf("value = %q, want %q", cookies[0].Value, "golang+rust+python")
	}
}

func TestSetListCookie_Empty(t *testing.T) {
	rec := httptest.NewRecorder()
	setListCookie(rec, "subscriptions", []string{})

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie (deletion), got %d", len(cookies))
	}
	if cookies[0].MaxAge != -1 {
		t.Errorf("empty list should clear cookie, maxAge = %d", cookies[0].MaxAge)
	}
}

func TestSetListCookie_SplitLong(t *testing.T) {
	// Build a list of subreddits that exceeds maxCookieValueLen
	var subs []string
	for i := 0; i < 500; i++ {
		subs = append(subs, "subreddit_name_that_is_fairly_long_"+strings.Repeat("x", 10))
	}

	rec := httptest.NewRecorder()
	setListCookie(rec, "subscriptions", subs)

	cookies := rec.Result().Cookies()
	if len(cookies) < 2 {
		t.Fatalf("expected multiple cookies for long list, got %d", len(cookies))
	}

	// First cookie should be named "subscriptions", rest "subscriptions1", "subscriptions2", ...
	if cookies[0].Name != "subscriptions" {
		t.Errorf("first cookie name = %q, want %q", cookies[0].Name, "subscriptions")
	}
	if cookies[1].Name != "subscriptions1" {
		t.Errorf("second cookie name = %q, want %q", cookies[1].Name, "subscriptions1")
	}

	// Each chunk should not exceed maxCookieValueLen
	for _, c := range cookies {
		if len(c.Value) > maxCookieValueLen {
			t.Errorf("cookie %q value length %d exceeds max %d", c.Name, len(c.Value), maxCookieValueLen)
		}
	}

	// Reconstruct and verify no data lost
	var parts []string
	for _, c := range cookies {
		parts = append(parts, c.Value)
	}
	joined := strings.Join(parts, "+")
	reconstructed := strings.Split(joined, "+")
	if len(reconstructed) != len(subs) {
		t.Errorf("reconstructed %d subs, want %d", len(reconstructed), len(subs))
	}
}

func TestClearNumberedCookies(t *testing.T) {
	rec := httptest.NewRecorder()
	clearNumberedCookies(rec, "subscriptions")

	cookies := rec.Result().Cookies()
	// Should clear: subscriptions, subscriptions1, ..., subscriptions9 = 10
	if len(cookies) != 10 {
		t.Fatalf("expected 10 clear cookies, got %d", len(cookies))
	}
	for _, c := range cookies {
		if c.MaxAge != -1 {
			t.Errorf("cookie %q maxAge = %d, want -1", c.Name, c.MaxAge)
		}
	}
}

func TestHandleSettingsRestore_SetsFromQuery(t *testing.T) {
	h := &Handler{
		cfg: configForTest(),
	}

	query := url.Values{}
	query.Set("theme", "nord")
	query.Set("layout", "clean")
	query.Set("comment_sort", "old")
	query.Set("subscriptions", "golang+rust+python")

	req := httptest.NewRequest("GET", "/settings/restore?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	h.handleSettingsRestore(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}

	cookies := rec.Result().Cookies()
	cookieMap := make(map[string]string)
	for _, c := range cookies {
		if c.MaxAge > 0 {
			cookieMap[c.Name] = c.Value
		}
	}

	if cookieMap["theme"] != "nord" {
		t.Errorf("theme = %q, want nord", cookieMap["theme"])
	}
	if cookieMap["layout"] != "clean" {
		t.Errorf("layout = %q, want clean", cookieMap["layout"])
	}
	if cookieMap["comment_sort"] != "old" {
		t.Errorf("comment_sort = %q, want old", cookieMap["comment_sort"])
	}
	if cookieMap["subscriptions"] != "golang+rust+python" {
		t.Errorf("subscriptions = %q, want golang+rust+python", cookieMap["subscriptions"])
	}
}

func TestHandleSettingsSave_SetsCookies(t *testing.T) {
	h := &Handler{
		cfg: configForTest(),
	}

	form := url.Values{}
	form.Set("theme", "dracula")
	form.Set("layout", "compact")
	form.Set("wide", "on")
	form.Set("fixed_navbar", "on")

	body := strings.NewReader(form.Encode())
	req := httptest.NewRequest("POST", "/settings", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleSettingsSave(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}

	cookies := rec.Result().Cookies()
	cookieMap := make(map[string]string)
	maxAgeMap := make(map[string]int)
	for _, c := range cookies {
		cookieMap[c.Name] = c.Value
		maxAgeMap[c.Name] = c.MaxAge
	}

	if cookieMap["theme"] != "dracula" {
		t.Errorf("theme = %q, want dracula", cookieMap["theme"])
	}
	if cookieMap["layout"] != "compact" {
		t.Errorf("layout = %q, want compact", cookieMap["layout"])
	}
	if cookieMap["wide"] != "on" {
		t.Errorf("wide = %q, want on", cookieMap["wide"])
	}

	// Unset preferences should be cleared (maxAge = -1)
	if maxAgeMap["show_nsfw"] != -1 {
		t.Errorf("show_nsfw maxAge = %d, want -1 (cleared)", maxAgeMap["show_nsfw"])
	}
}

func configForTest() *config.Config {
	return &config.Config{}
}
