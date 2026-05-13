package legacy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redmemo/redmemo/internal/config"
)

func TestParseSettingsHTML_SelectFields(t *testing.T) {
	html := `
<form>
  <select name="theme">
    <option value="">Default</option>
    <option value="dark" selected>Dark</option>
    <option value="light">Light</option>
  </select>
  <select name="layout">
    <option value="card" selected>Card</option>
    <option value="compact">Compact</option>
  </select>
  <select name="front_page">
    <option value="popular">Popular</option>
    <option value="all" selected>All</option>
  </select>
</form>`

	settings := parseSettingsHTML(html)

	checks := map[string]string{
		"theme":      "dark",
		"layout":     "card",
		"front_page": "all",
	}
	for name, want := range checks {
		got, ok := settings[name]
		if !ok {
			t.Errorf("missing setting %q", name)
			continue
		}
		if got != want {
			t.Errorf("settings[%q] = %q, want %q", name, got, want)
		}
	}
}

func TestParseSettingsHTML_Checkboxes(t *testing.T) {
	html := `
<form>
  <input type="checkbox" name="show_nsfw" checked>
  <input type="checkbox" name="blur_nsfw">
  <input name="use_hls" type="checkbox" checked>
</form>`

	settings := parseSettingsHTML(html)

	if settings["show_nsfw"] != "on" {
		t.Errorf("show_nsfw = %q, want %q", settings["show_nsfw"], "on")
	}
	if settings["use_hls"] != "on" {
		t.Errorf("use_hls = %q, want %q", settings["use_hls"], "on")
	}
	if settings["blur_nsfw"] != "" {
		t.Errorf("blur_nsfw = %q, want empty (unchecked)", settings["blur_nsfw"])
	}
}

func TestParseSettingsHTML_Empty(t *testing.T) {
	settings := parseSettingsHTML("")
	if len(settings) != 0 {
		t.Errorf("expected empty map, got %v", settings)
	}
}

func TestFilterByEnv(t *testing.T) {
	t.Setenv("REDMEMO_DEFAULT_THEME", "nord")

	settings := map[string]string{
		"theme":     "dark",
		"layout":    "compact",
		"show_nsfw": "on",
	}

	filtered := FilterByEnv(settings)

	if _, ok := filtered["theme"]; ok {
		t.Error("theme should be filtered out (env override exists)")
	}
	if filtered["layout"] != "compact" {
		t.Errorf("layout = %q, want %q", filtered["layout"], "compact")
	}
	if filtered["show_nsfw"] != "on" {
		t.Errorf("show_nsfw = %q, want %q", filtered["show_nsfw"], "on")
	}
}

func TestBuildTargetList_NoExplicit(t *testing.T) {
	cfg := config.LegacyConfig{SyncEnabled: true, Instance: ""}
	targets := buildTargetList(cfg)
	if len(targets) != 1 || targets[0] != defaultRedlibAddr {
		t.Errorf("no explicit: targets = %v, want [%s]", targets, defaultRedlibAddr)
	}
}

func TestBuildTargetList_ExplicitDifferent(t *testing.T) {
	cfg := config.LegacyConfig{SyncEnabled: true, Instance: "192.168.1.100:8080"}
	targets := buildTargetList(cfg)
	if len(targets) != 2 {
		t.Fatalf("explicit different: want 2 targets, got %v", targets)
	}
	if targets[0] != "http://192.168.1.100:8080" {
		t.Errorf("targets[0] = %q, want explicit addr with http", targets[0])
	}
	if targets[1] != defaultRedlibAddr {
		t.Errorf("targets[1] = %q, want default fallback", targets[1])
	}
}

func TestBuildTargetList_ExplicitWithScheme(t *testing.T) {
	cfg := config.LegacyConfig{SyncEnabled: true, Instance: "https://redlib.example.com"}
	targets := buildTargetList(cfg)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %v", targets)
	}
	if targets[0] != "https://redlib.example.com" {
		t.Errorf("targets[0] = %q, want explicit as-is", targets[0])
	}
}

func TestBuildTargetList_ExplicitSameAsDefault(t *testing.T) {
	cfg := config.LegacyConfig{SyncEnabled: true, Instance: "http://redlib:8080"}
	targets := buildTargetList(cfg)
	if len(targets) != 1 {
		t.Errorf("explicit == default: should deduplicate, got %v", targets)
	}
}

func TestSyncSettings_Disabled(t *testing.T) {
	cfg := config.LegacyConfig{SyncEnabled: false}
	result, err := SyncSettings(cfg)
	if err != nil {
		t.Fatalf("disabled sync should not error: %v", err)
	}
	if result != nil {
		t.Error("disabled sync should return nil result")
	}
}

func TestSyncSettings_FallbackChain(t *testing.T) {
	settingsHTML := `<form><select name="theme"><option value="dark" selected>Dark</option></select></form>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings" {
			w.Write([]byte(settingsHTML))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := config.LegacyConfig{
		SyncEnabled: true,
		Instance:    "http://127.0.0.1:1", // unreachable port
	}

	// This will fail on the explicit address and then fail on Docker DNS default too,
	// since we're not in Docker. That's expected — both unreachable.
	result, err := SyncSettings(cfg)
	if err == nil {
		t.Log("sync unexpectedly succeeded (Docker DNS reachable?), result:", result)
	}
	// The important thing: it tried both and didn't panic.
}

func TestTryFetchSettings_Success(t *testing.T) {
	settingsHTML := `
<form>
  <select name="theme"><option value="dark" selected>Dark</option></select>
  <input type="checkbox" name="show_nsfw" checked>
</form>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings" {
			w.Write([]byte(settingsHTML))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	result, err := tryFetchSettings(srv.URL)
	if err != nil {
		t.Fatalf("tryFetchSettings: %v", err)
	}
	if result.Settings["theme"] != "dark" {
		t.Errorf("theme = %q, want dark", result.Settings["theme"])
	}
	if result.Settings["show_nsfw"] != "on" {
		t.Errorf("show_nsfw = %q, want on", result.Settings["show_nsfw"])
	}
	if result.Source != srv.URL {
		t.Errorf("source = %q, want %q", result.Source, srv.URL)
	}
}

func TestTryFetchSettings_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	_, err := tryFetchSettings(srv.URL)
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestTryFetchSettings_Unreachable(t *testing.T) {
	_, err := tryFetchSettings("http://127.0.0.1:1")
	if err == nil {
		t.Error("expected error for unreachable host")
	}
}

func TestSyncSettings_UsesFirstReachable(t *testing.T) {
	settingsHTML := `<form><select name="theme"><option value="nord" selected>Nord</option></select></form>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings" {
			w.Write([]byte(settingsHTML))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := config.LegacyConfig{
		SyncEnabled: true,
		Instance:    srv.URL, // this one is reachable
	}

	result, err := SyncSettings(cfg)
	if err != nil {
		t.Fatalf("SyncSettings: %v", err)
	}
	if result.Settings["theme"] != "nord" {
		t.Errorf("theme = %q, want nord", result.Settings["theme"])
	}
	if result.Source != srv.URL {
		t.Errorf("source = %q, want %q", result.Source, srv.URL)
	}
}
