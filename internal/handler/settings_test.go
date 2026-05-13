package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/config"
)

func TestHandleSettingsSave_WritesToSiteDefaults(t *testing.T) {
	defaults := make(map[string]string)
	h := &Handler{
		cfg:          configForTest(),
		siteDefaults: defaults,
	}

	form := url.Values{}
	form.Set("theme", "dracula")
	form.Set("layout", "compact")
	form.Set("wide", "on")
	form.Set("fixed_navbar", "on")
	form.Set("show_nsfw", "off")

	body := strings.NewReader(form.Encode())
	req := httptest.NewRequest("POST", "/settings", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleSettingsSave(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}

	if defaults["theme"] != "dracula" {
		t.Errorf("siteDefaults[theme] = %q, want dracula", defaults["theme"])
	}
	if defaults["layout"] != "compact" {
		t.Errorf("siteDefaults[layout] = %q, want compact", defaults["layout"])
	}
	if defaults["wide"] != "on" {
		t.Errorf("siteDefaults[wide] = %q, want on", defaults["wide"])
	}
	if defaults["show_nsfw"] != "off" {
		t.Errorf("siteDefaults[show_nsfw] = %q, want off", defaults["show_nsfw"])
	}
}

func TestHandleSettingsSave_SetsThemeCookie(t *testing.T) {
	h := &Handler{
		cfg:          configForTest(),
		siteDefaults: make(map[string]string),
	}

	form := url.Values{}
	form.Set("theme", "nord")

	body := strings.NewReader(form.Encode())
	req := httptest.NewRequest("POST", "/settings", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleSettingsSave(rec, req)

	cookies := rec.Result().Cookies()
	var themeCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "theme" {
			themeCookie = c
			break
		}
	}
	if themeCookie == nil {
		t.Fatal("expected theme cookie to be set")
	}
	if themeCookie.Value != "nord" {
		t.Errorf("theme cookie = %q, want nord", themeCookie.Value)
	}
	if themeCookie.MaxAge != cookieMaxAge {
		t.Errorf("maxAge = %d, want %d", themeCookie.MaxAge, cookieMaxAge)
	}
}

func configForTest() *config.Config {
	return &config.Config{}
}
