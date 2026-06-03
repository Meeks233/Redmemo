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

// numericNormCase exercises one numeric-input setting through NormalizeSettings.
// The same shape covers every bounded integer pref — page_limit, prefetch_threshold,
// scroll_interval — so the validation contract (in-range kept and canonicalised,
// anything else dropped + reported via rejected slice) is pinned identically.
type numericNormCase struct {
	name      string
	in        string
	wantKeep  bool
	wantValue string
}

func runNumericNorm(t *testing.T, key string, cases []numericNormCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, rejected := NormalizeSettings(map[string]string{key: tc.in})
			got, kept := out[key]
			if kept != tc.wantKeep {
				t.Fatalf("%s kept=%v want %v (got value %q, rejected=%+v)", key, kept, tc.wantKeep, got, rejected)
			}
			if kept && got != tc.wantValue {
				t.Errorf("%s value=%q want %q", key, got, tc.wantValue)
			}
			if !tc.wantKeep {
				found := false
				for _, r := range rejected {
					if r.Key == key {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %s in rejected slice, got %+v", key, rejected)
				}
			}
		})
	}
}

// TestNormalizeSettings_PageLimit pins the validation contract: values inside
// [5, 25] are accepted and canonicalised; anything else is dropped from the
// updates map (so the existing stored value — DB row or env_override —
// survives) and surfaces in the rejected slice. The same call runs at startup
// over REDMEMO_DEFAULT_PAGE_LIMIT, so a garbage env var never poisons
// siteDefaults and pref() falls through to prefDefaults["page_limit"]="5".
func TestNormalizeSettings_PageLimit(t *testing.T) {
	runNumericNorm(t, "page_limit", []numericNormCase{
		{"min boundary", "5", true, "5"},
		{"max boundary", "25", true, "25"},
		{"interior", "12", true, "12"},
		{"below range", "4", false, ""},
		{"above range", "26", false, ""},
		{"zero", "0", false, ""},
		{"negative", "-5", false, ""},
		{"non-numeric", "abc", false, ""},
		{"empty", "", false, ""},
		{"whitespace", "  ", false, ""},
		{"float", "5.5", false, ""},
		{"leading plus", "+10", true, "10"},
		{"hex", "0x10", false, ""},
		{"huge", "999999999", false, ""},
		{"overflow", "99999999999999999999", false, ""},
	})
}

// TestNormalizeSettings_PrefetchThreshold pins the [1, 99] bound. The frontend
// also runs client-side JS validation in savePrefetchThreshold (which fires on
// `onchange` and skips the fetch when out-of-range), but a hand-crafted POST or
// REDMEMO_DEFAULT_PREFETCH_THRESHOLD env var bypasses that — backend is the
// last line of defence.
func TestNormalizeSettings_PrefetchThreshold(t *testing.T) {
	runNumericNorm(t, "prefetch_threshold", []numericNormCase{
		{"min boundary", "1", true, "1"},
		{"max boundary", "99", true, "99"},
		{"interior", "50", true, "50"},
		{"zero", "0", false, ""},
		{"above range", "100", false, ""},
		{"negative", "-1", false, ""},
		{"non-numeric", "fifty", false, ""},
		{"empty", "", false, ""},
		{"whitespace", " 50 ", false, ""},
		{"float", "50.0", false, ""},
		{"huge", "999999999", false, ""},
		{"overflow", "99999999999999999999", false, ""},
	})
}

// TestNormalizeSettings_ScrollInterval pins the [1, 60] bound (seconds). Without
// an upper bound a user could enter 99999999 and starve the infinite-loader.
func TestNormalizeSettings_ScrollInterval(t *testing.T) {
	runNumericNorm(t, "scroll_interval", []numericNormCase{
		{"min boundary", "1", true, "1"},
		{"max boundary", "60", true, "60"},
		{"default", "2", true, "2"},
		{"interior", "30", true, "30"},
		{"zero", "0", false, ""},
		{"above range", "61", false, ""},
		{"negative", "-1", false, ""},
		{"non-numeric", "fast", false, ""},
		{"empty", "", false, ""},
		{"whitespace", "  ", false, ""},
		{"float", "2.5", false, ""},
		{"huge", "99999999", false, ""},
		{"overflow", "99999999999999999999", false, ""},
	})
}

// TestNormalizeSettings_SettingsTokenTTL pins the whitelist contract. Anything
// outside {5, 10, 15, 30, 60} is dropped — longer-lived ephemeral tokens defeat
// the lockout/TOTP gate's threat model.
func TestNormalizeSettings_SettingsTokenTTL(t *testing.T) {
	runNumericNorm(t, "settings_token_ttl", []numericNormCase{
		{"allowed 5", "5", true, "5"},
		{"allowed 10", "10", true, "10"},
		{"allowed 15", "15", true, "15"},
		{"allowed 30", "30", true, "30"},
		{"allowed 60", "60", true, "60"},
		{"unlisted 20", "20", false, ""},
		{"zero", "0", false, ""},
		{"negative", "-5", false, ""},
		{"above whitelist", "120", false, ""},
		{"non-numeric", "ten", false, ""},
		{"empty", "", false, ""},
		{"whitespace", " 10 ", false, ""},
		{"float", "10.0", false, ""},
		{"leading zero", "05", false, ""},
	})
}

// TestNormalizeSettings_PreservesUnrelatedKeys guards the drop-on-invalid
// contract: a rejected numeric pref must not collateral-damage other keys
// in the same save batch.
func TestNormalizeSettings_PreservesUnrelatedKeys(t *testing.T) {
	in := map[string]string{
		"page_limit":         "9999",
		"scroll_interval":    "abc",
		"prefetch_threshold": "-1",
		"settings_token_ttl": "999",
		"theme":              "dracula",
		"layout":             "compact",
	}
	out, rejected := NormalizeSettings(in)
	for _, k := range []string{"page_limit", "scroll_interval", "prefetch_threshold", "settings_token_ttl"} {
		if _, kept := out[k]; kept {
			t.Errorf("%s should have been dropped", k)
		}
	}
	if out["theme"] != "dracula" {
		t.Errorf("theme = %q, want dracula", out["theme"])
	}
	if out["layout"] != "compact" {
		t.Errorf("layout = %q, want compact", out["layout"])
	}
	if len(rejected) != 4 {
		t.Errorf("rejected count = %d, want 4: %+v", len(rejected), rejected)
	}
}
