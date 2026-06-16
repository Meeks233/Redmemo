package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
// [5, 100] are accepted and canonicalised; anything else is dropped from the
// updates map (so the existing stored value — DB row or env_override —
// survives) and surfaces in the rejected slice. The same call runs at startup
// over REDMEMO_DEFAULT_PAGE_LIMIT, so a garbage env var never poisons
// siteDefaults and pref() falls through to prefDefaults["page_limit"]="50".
// The upper bound is Reddit's listing-endpoint cap; the default of 50 reflects
// that the OAuth quota is per-request, so a larger page is strictly cheaper.
func TestNormalizeSettings_PageLimit(t *testing.T) {
	runNumericNorm(t, "page_limit", []numericNormCase{
		{"min boundary", "5", true, "5"},
		{"max boundary", "100", true, "100"},
		{"interior", "50", true, "50"},
		{"below range", "4", false, ""},
		{"above range", "101", false, ""},
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

// TestNormalizeSettings_PrefetchL3MinComments pins the [0, 100000] bound. 0
// is a valid value (= filter disabled) so the lower bound differs from the
// other numeric prefs. Negative values must be rejected — and a rejection here
// is fatal at startup (see IsFatalSettingKey + main.go's env loop) so the
// validator absolutely cannot let a negative through.
func TestNormalizeSettings_PrefetchL3MinComments(t *testing.T) {
	runNumericNorm(t, "prefetch_l3_min_comments", []numericNormCase{
		{"min boundary (zero disables)", "0", true, "0"},
		{"interior", "5", true, "5"},
		{"max boundary", "100000", true, "100000"},
		{"above range", "100001", false, ""},
		{"negative", "-1", false, ""},
		{"large negative", "-9999", false, ""},
		{"non-numeric", "five", false, ""},
		{"empty", "", false, ""},
		{"whitespace", " 5 ", false, ""},
		{"float", "5.0", false, ""},
		{"huge", "999999999", false, ""},
		{"overflow", "99999999999999999999", false, ""},
	})
}

func TestNormalizeSettings_LongVideoThreshold(t *testing.T) {
	runNumericNorm(t, "long_video_threshold", []numericNormCase{
		{"zero disables gate", "0", true, "0"},
		{"min positive", "1", true, "1"},
		{"max boundary", "99", true, "99"},
		{"default", "5", true, "5"},
		{"above range", "100", false, ""},
		{"negative", "-1", false, ""},
		{"non-numeric", "five", false, ""},
		{"empty", "", false, ""},
		{"float", "5.0", false, ""},
	})
}

// TestIsFatalSettingKey pins which keys cause a startup abort when their
// env_override fails validation. Today only prefetch_l3_min_comments — but the
// list MUST stay tight, so any addition surfaces here.
func TestIsFatalSettingKey(t *testing.T) {
	fatal := []string{"prefetch_l3_min_comments"}
	nonFatal := []string{
		"page_limit", "scroll_interval", "prefetch_threshold",
		"settings_token_ttl", "prefetch_default_depth", "theme", "lang",
	}
	for _, k := range fatal {
		if !IsFatalSettingKey(k) {
			t.Errorf("IsFatalSettingKey(%q) = false, want true", k)
		}
	}
	for _, k := range nonFatal {
		if IsFatalSettingKey(k) {
			t.Errorf("IsFatalSettingKey(%q) = true, want false", k)
		}
	}
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

// TestResolveLatest pins the pure latest-writer-wins rule between the user
// shadow and the env shadow.
func TestResolveLatest(t *testing.T) {
	old := time.Unix(0, 0)
	newer := time.Unix(1000, 0)
	cases := []struct {
		name    string
		uVal    string
		uAt     time.Time
		uOK     bool
		eVal    string
		eAt     time.Time
		eOK     bool
		wantVal string
		wantOK  bool
	}{
		{"user newer than env wins", "rust", newer, true, "golang", old, true, "rust", true},
		{"env newer than user wins", "rust", old, true, "cats", newer, true, "cats", true},
		{"user only", "rust", newer, true, "", time.Time{}, false, "rust", true},
		{"env only", "", time.Time{}, false, "golang", old, true, "golang", true},
		{"neither", "", time.Time{}, false, "", time.Time{}, false, "", false},
		{"empty user value newer disables", "", newer, true, "all", old, true, "", true},
		{"tie favours user", "rust", old, true, "cats", old, true, "rust", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotVal, gotOK := ResolveLatest(c.uVal, c.uAt, c.uOK, c.eVal, c.eAt, c.eOK)
			if gotVal != c.wantVal || gotOK != c.wantOK {
				t.Errorf("ResolveLatest() = (%q,%v), want (%q,%v)", gotVal, gotOK, c.wantVal, c.wantOK)
			}
		})
	}
}

// fakeManagedStore is an in-memory ManagedSettingsStore for reconcile tests.
type fakeManagedStore struct {
	rows         map[string]fakeRow
	batchClock   time.Time // timestamp stamped on SetBatch (live) writes
	shadowWrites int       // count of SetShadow calls
	batchWrites  int       // count of SetBatch calls
}
type fakeRow struct {
	value     string
	updatedAt time.Time
	source    string
}

func newFakeManagedStore() *fakeManagedStore {
	return &fakeManagedStore{rows: make(map[string]fakeRow)}
}

func (f *fakeManagedStore) GetMeta(name string) (string, time.Time, bool, error) {
	r, ok := f.rows[name]
	return r.value, r.updatedAt, ok, nil
}
func (f *fakeManagedStore) SetShadow(name, value string, at time.Time) error {
	f.rows[name] = fakeRow{value: value, updatedAt: at, source: "shadow"}
	f.shadowWrites++
	return nil
}
func (f *fakeManagedStore) SetBatch(settings map[string]string, source string) error {
	for k, v := range settings {
		f.rows[k] = fakeRow{value: v, updatedAt: f.batchClock, source: source}
	}
	f.batchWrites++
	return nil
}
func (f *fakeManagedStore) Delete(name string) error {
	delete(f.rows, name)
	return nil
}
func (f *fakeManagedStore) live(name string) (string, bool) {
	r, ok := f.rows[name]
	return r.value, ok
}

// TestReconcileManagedSettings is the regression suite for the reported bug:
// "LAN compose declares (or once declared) the homepage default, I set it
// manually, and every rebuild wipes my value." Each case simulates the startup
// reconcile and asserts the live row ends up at the latest writer's value.
func TestReconcileManagedSettings(t *testing.T) {
	const key = "front_page_subs"
	uShadow := userShadowKey(key)
	eShadow := envShadowKey(key)
	envTime := time.Unix(1000, 0)  // env value first seen / last changed
	userTime := time.Unix(1500, 0) // user changed later (> envTime)
	now := time.Unix(2000, 0)      // this boot

	t.Run("BUG: user value set after an unchanged env default survives rebuild", func(t *testing.T) {
		f := newFakeManagedStore()
		// Env "all" was first seen at envTime; user later set sub:golang.
		f.rows[eShadow] = fakeRow{value: "all", updatedAt: envTime}
		f.rows[uShadow] = fakeRow{value: "sub:golang", updatedAt: userTime}
		// Simulate Step B having clobbered the live row with the env default.
		f.rows[key] = fakeRow{value: "all", source: "env_override"}

		if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, now); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.live(key); got != "sub:golang" {
			t.Errorf("live %s = %q, want sub:golang (user's later value must survive)", key, got)
		}
		// The unchanged env value must keep its original timestamp.
		if got := f.rows[eShadow].updatedAt; !got.Equal(envTime) {
			t.Errorf("env shadow time = %v, want unchanged %v", got, envTime)
		}
	})

	t.Run("operator changes env after user → newest env wins", func(t *testing.T) {
		f := newFakeManagedStore()
		f.rows[eShadow] = fakeRow{value: "all", updatedAt: envTime}
		f.rows[uShadow] = fakeRow{value: "sub:golang", updatedAt: userTime}
		// Compose changed all → sub:cats; reconcile stamps it at `now` (> userTime).
		if _, err := ReconcileManagedSettings(f, map[string]string{key: "sub:cats"}, now); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.live(key); got != "sub:cats" {
			t.Errorf("live %s = %q, want sub:cats (operator's newer change wins)", key, got)
		}
		if got := f.rows[eShadow].updatedAt; !got.Equal(now) {
			t.Errorf("changed env shadow time = %v, want now %v", got, now)
		}
	})

	t.Run("env withdrawn → user value wins", func(t *testing.T) {
		f := newFakeManagedStore()
		f.rows[eShadow] = fakeRow{value: "all", updatedAt: envTime}
		f.rows[uShadow] = fakeRow{value: "sub:golang", updatedAt: userTime}
		if _, err := ReconcileManagedSettings(f, map[string]string{}, now); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.live(key); got != "sub:golang" {
			t.Errorf("live %s = %q, want sub:golang", key, got)
		}
		if _, ok := f.rows[eShadow]; ok {
			t.Errorf("env shadow should be deleted when the env var is withdrawn")
		}
	})

	t.Run("fresh env default, no user → env seeds the value (stamped now)", func(t *testing.T) {
		f := newFakeManagedStore()
		if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, now); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.live(key); got != "all" {
			t.Errorf("live %s = %q, want all (env seeds when user never set it)", key, got)
		}
		// First observation is stamped `now` — symmetric with a user save, NOT a
		// hardcoded epoch.
		if r, ok := f.rows[eShadow]; !ok || !r.updatedAt.Equal(now) {
			t.Errorf("first env observation must be stamped now (%v), got %v ok=%v", now, r.updatedAt, ok)
		}
	})

	t.Run("env added after user → newest env wins", func(t *testing.T) {
		// User set sub:golang at userTime; the operator only NOW adds the env
		// default (first observation at `now` > userTime), so the env wins.
		f := newFakeManagedStore()
		f.rows[uShadow] = fakeRow{value: "sub:golang", updatedAt: userTime}
		if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, now); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.live(key); got != "all" {
			t.Errorf("live %s = %q, want all (env added after the user save is newer)", key, got)
		}
	})

	t.Run("user clears box after env → disabled (empty wins)", func(t *testing.T) {
		f := newFakeManagedStore()
		f.rows[eShadow] = fakeRow{value: "all", updatedAt: envTime}
		f.rows[uShadow] = fakeRow{value: "", updatedAt: userTime} // cleared, newer
		if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, now); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.live(key); got != "" {
			t.Errorf("live %s = %q, want empty (user's clear wins → disabled)", key, got)
		}
	})
}

// TestReconcileManagedSettings_StableOnRepeatedRebuilds is the regression for
// "如果相同的env值被反复重构，会不会被刷时间": reconciling the SAME env value over and
// over (every rebuild) must NOT touch any timestamp or rewrite the live row.
// Both the env shadow (decision input) and the live row must stay byte-for-byte
// stable, so a user value set long ago never gets leapfrogged by a re-stamped
// env seed.
func TestReconcileManagedSettings_StableOnRepeatedRebuilds(t *testing.T) {
	const key = "front_page_subs"
	eShadow := envShadowKey(key)

	t.Run("env-only: first boot stamps now, later boots are pure no-ops", func(t *testing.T) {
		f := newFakeManagedStore()
		boot1 := time.Unix(100, 0)
		f.batchClock = boot1

		// Boot 1: first observation stamps the env shadow `now` and writes live.
		if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, boot1); err != nil {
			t.Fatal(err)
		}
		envAt := f.rows[eShadow].updatedAt
		liveAt := f.rows[key].updatedAt
		shadowWrites, batchWrites := f.shadowWrites, f.batchWrites
		if !envAt.Equal(boot1) {
			t.Fatalf("env shadow stamped %v, want boot1 %v (first observation = now)", envAt, boot1)
		}

		// Boots 2..4: same env value, advancing wall clock. Nothing must change.
		for i, ts := range []time.Time{time.Unix(500, 0), time.Unix(900, 0), time.Unix(1300, 0)} {
			f.batchClock = ts
			if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, ts); err != nil {
				t.Fatal(err)
			}
			if got := f.rows[eShadow].updatedAt; !got.Equal(envAt) {
				t.Errorf("boot %d: env shadow time = %v, want unchanged %v (same env must not refresh)", i+2, got, envAt)
			}
			if got := f.rows[key].updatedAt; !got.Equal(liveAt) {
				t.Errorf("boot %d: live row time = %v, want unchanged %v (no-op rewrite must be skipped)", i+2, got, liveAt)
			}
			if f.shadowWrites != shadowWrites {
				t.Errorf("boot %d: shadowWrites = %d, want unchanged %d", i+2, f.shadowWrites, shadowWrites)
			}
			if f.batchWrites != batchWrites {
				t.Errorf("boot %d: batchWrites = %d, want unchanged %d (live row rewritten redundantly)", i+2, f.batchWrites, batchWrites)
			}
		}
	})

	t.Run("user-wins: repeated same-env rebuilds keep both shadows frozen", func(t *testing.T) {
		uShadow := userShadowKey(key)
		envTime := time.Unix(1000, 0)
		f := newFakeManagedStore()
		f.batchClock = time.Unix(2000, 0)
		// env "all" first seen at envTime; user later (1500) picked sub:golang.
		f.rows[eShadow] = fakeRow{value: "all", updatedAt: envTime}
		f.rows[uShadow] = fakeRow{value: "sub:golang", updatedAt: time.Unix(1500, 0)}

		var prevLiveAt time.Time
		for i, ts := range []time.Time{time.Unix(2000, 0), time.Unix(3000, 0), time.Unix(4000, 0)} {
			f.batchClock = ts
			if _, err := ReconcileManagedSettings(f, map[string]string{key: "all"}, ts); err != nil {
				t.Fatal(err)
			}
			if got, _ := f.live(key); got != "sub:golang" {
				t.Errorf("boot %d: live = %q, want sub:golang (user keeps winning)", i+1, got)
			}
			if got := f.rows[eShadow].updatedAt; !got.Equal(envTime) {
				t.Errorf("boot %d: env shadow time = %v, want unchanged %v (unchanged env must not re-stamp)", i+1, got, envTime)
			}
			if got := f.rows[uShadow].updatedAt; !got.Equal(time.Unix(1500, 0)) {
				t.Errorf("boot %d: user shadow time = %v, want 1500 (reconcile must never touch user shadow)", i+1, got)
			}
			// Live row is written once (boot 1, value changes all→sub:golang) then
			// stable: its timestamp must not advance on later identical boots.
			liveAt := f.rows[key].updatedAt
			if i == 0 {
				prevLiveAt = liveAt
			} else if !liveAt.Equal(prevLiveAt) {
				t.Errorf("boot %d: live row time = %v, want unchanged %v", i+1, liveAt, prevLiveAt)
			}
		}
	})
}

// TestNormalizeSettings_ParamsParsedWhenQueryEmpty pins the "disabled but still
// parses related params" contract: an empty crawl list must NOT cause the
// scheduler tuning params (sort/timeframe/depth/threshold) to be dropped — they
// are canonicalised and kept so they are ready the moment a query is added.
func TestNormalizeSettings_ParamsParsedWhenQueryEmpty(t *testing.T) {
	out, _ := NormalizeSettings(map[string]string{
		"prefetch_unified":       "", // disabled — no subs
		"prefetch_sort":          "top",
		"prefetch_timeframe":     "week",
		"prefetch_default_depth": "L3+L2", // exercises canonicalisation
		"prefetch_threshold":     "70",
	})
	if out["prefetch_subs"] != "" {
		t.Errorf("prefetch_subs = %q, want empty (disabled)", out["prefetch_subs"])
	}
	want := map[string]string{
		"prefetch_sort":          "top",
		"prefetch_timeframe":     "week",
		"prefetch_default_depth": "l2+l3",
		"prefetch_threshold":     "70",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %q, want %q (params must survive an empty query)", k, out[k], v)
		}
	}
}

// TestHasAlnum pins the blank-query primitive: a string is "blank" for skip
// purposes when it has no ASCII letter or digit, regardless of how much
// whitespace/punctuation it carries.
func TestHasAlnum(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"\t\n", false},
		{"!!!", false},
		{"+++", false},
		{"***", false},
		{"-_-", false},
		{" :// ", false},
		{"a", true},
		{"0", true},
		{"golang", true},
		{"sub:golang", true},
		{"!x!", true},
	}
	for _, c := range cases {
		if got := hasAlnum(c.in); got != c.want {
			t.Errorf("hasAlnum(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNormalizeSettings_FrontPageSkipSamples is the homepage half of the
// defensive "query decides everything" contract. There is no disable toggle:
// an empty box, or one that canonicalises down to pure punctuation, must store
// "" so handleFrontPage treats the homepage as skipped (redirect to /archive).
// Anything with a real subreddit or free-text term survives and keeps the feed.
func TestNormalizeSettings_FrontPageSkipSamples(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// --- skip (stored as "") ---
		{"empty", "", ""},
		{"spaces only", "   ", ""},
		{"tabs/newlines", "\t\n ", ""},
		{"bang symbols", "!!!", ""},
		{"plus symbols", "+++", ""},
		{"star symbols", "***", ""},
		{"single dash (env skip sentinel)", "-", ""},
		{"dash underscore", "-_-", ""},
		{"slashes", "/// ::", ""},
		{"invalid sub token only", "sub:!!!", ""},
		// --- active (kept) ---
		{"all keeps homepage", "all", "all"},
		{"single sub", "sub:golang", "sub:golang"},
		{"r-prefixed sub", "sub:r/golang", "sub:golang"},
		{"free text term", "cats", "cats"},
		{"sub list", "sub:golang+rust", "sub:golang+rust"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := NormalizeSettings(map[string]string{"front_page_subs": c.in})
			if got := out["front_page_subs"]; got != c.want {
				t.Errorf("front_page_subs %q normalised to %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeSettings_PrefetchSkipSamples is the Natural-Prefetch half of the
// same contract, exercised through the real form key (prefetch_unified). A
// blank or symbol-only crawl list must collapse to an empty prefetch_subs so
// the scheduler's isEnabled() reports disabled; a list with at least one valid
// subreddit yields the canonical sub: query that drives the crawl.
func TestNormalizeSettings_PrefetchSkipSamples(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// --- skip (prefetch_subs stored as "") ---
		{"empty", "", ""},
		{"spaces only", "   ", ""},
		{"bang symbols", "!!!", ""},
		{"plus symbols only", "+++", ""},
		{"dash symbols only", "---", ""},
		{"mixed junk separators", " + - + ", ""},
		// --- active (canonical sub: list) ---
		{"single sub", "golang", "sub:golang"},
		{"sub list", "cats+dogs", "sub:cats+dogs"},
		{"override clause", "golang=sort:rising", "sub:golang"},
		{"junk plus real", "!!!+golang", "sub:golang"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := NormalizeSettings(map[string]string{"prefetch_unified": c.in})
			if got := out["prefetch_subs"]; got != c.want {
				t.Errorf("prefetch_unified %q → prefetch_subs %q, want %q", c.in, got, c.want)
			}
		})
	}
}
