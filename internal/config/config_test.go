package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  listen: ":9090"
postgres:
  dsn: "postgres://user:pass@localhost/redmemo"
redis:
  addr: "localhost:6379"
media:
  root_path: "/data/media"
`
	os.WriteFile(path, []byte(yaml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("Server.Listen = %q, want %q", cfg.Server.Listen, ":9090")
	}
	if cfg.Postgres.DSN != "postgres://user:pass@localhost/redmemo" {
		t.Errorf("Postgres.DSN mismatch")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("Load() should fail for missing file")
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
postgres:
  dsn: "postgres://localhost/test"
redis:
  addr: "localhost:6379"
media:
  root_path: "/tmp/media"
`
	os.WriteFile(path, []byte(yaml), 0644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Listen != ":8080" {
		t.Errorf("default Server.Listen = %q, want %q", cfg.Server.Listen, ":8080")
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("default ReadTimeout = %v, want 30s", cfg.Server.ReadTimeout)
	}
	if cfg.RateLimit.WindowSize != 500 {
		t.Errorf("default WindowSize = %d, want 500", cfg.RateLimit.WindowSize)
	}
	if cfg.RateLimit.WindowDuration != 10*time.Minute {
		t.Errorf("default WindowDuration = %v, want 10m", cfg.RateLimit.WindowDuration)
	}
	if cfg.RateLimit.SafetyBuffer != 50 {
		t.Errorf("default SafetyBuffer = %d, want 50", cfg.RateLimit.SafetyBuffer)
	}
	if cfg.Media.MaxSizeGB != 50 {
		t.Errorf("default MaxSizeGB = %d, want 50", cfg.Media.MaxSizeGB)
	}
	if cfg.Media.EvictionThreshold != 0.8 {
		t.Errorf("default EvictionThreshold = %f, want 0.8", cfg.Media.EvictionThreshold)
	}
	if cfg.Render.BrandName != "RedMemo" {
		t.Errorf("default BrandName = %q, want %q", cfg.Render.BrandName, "RedMemo")
	}
	if !cfg.Legacy.SyncEnabled {
		t.Error("default Legacy.SyncEnabled should be true")
	}
}

func TestValidateMissingDSN(t *testing.T) {
	cfg := defaults()
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should fail when Postgres.DSN is empty")
	}
}

func TestValidateMissingRedisAddr(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Media.RootPath = "/tmp/media"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should fail when Redis.Addr is empty")
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate() should pass for valid config: %v", err)
	}
}

func TestValidateEvictionThreshold(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"

	cfg.Media.EvictionThreshold = 1.5
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() should fail for eviction_threshold > 1")
	}

	cfg.Media.EvictionThreshold = -0.1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() should fail for eviction_threshold < 0")
	}
}

func TestEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
postgres:
  dsn: "postgres://original/db"
redis:
  addr: "localhost:6379"
media:
  root_path: "/tmp/media"
`
	os.WriteFile(path, []byte(yaml), 0644)

	t.Setenv("REDMEMO_POSTGRES_DSN", "postgres://overridden/db")
	t.Setenv("REDMEMO_SERVER_LISTEN", ":3000")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Postgres.DSN != "postgres://overridden/db" {
		t.Errorf("env override DSN = %q, want %q", cfg.Postgres.DSN, "postgres://overridden/db")
	}
	if cfg.Server.Listen != ":3000" {
		t.Errorf("env override Listen = %q, want %q", cfg.Server.Listen, ":3000")
	}
}

func TestEnvOverrideLegacyInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
postgres:
  dsn: "postgres://localhost/test"
redis:
  addr: "localhost:6379"
media:
  root_path: "/tmp/media"
`
	os.WriteFile(path, []byte(yaml), 0644)

	t.Setenv("REDMEMO_LEGACY_INSTANCE", "192.168.1.100:8080")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Legacy.Instance != "192.168.1.100:8080" {
		t.Errorf("Legacy.Instance = %q, want %q", cfg.Legacy.Instance, "192.168.1.100:8080")
	}
	if cfg.Legacy.ResolvedInstance() != "192.168.1.100:8080" {
		t.Errorf("ResolvedInstance() = %q, want explicit address", cfg.Legacy.ResolvedInstance())
	}
}

func TestLegacyResolvedInstanceDefault(t *testing.T) {
	cfg := defaults()
	if cfg.Legacy.ResolvedInstance() != "http://redlib:8080" {
		t.Errorf("default ResolvedInstance() = %q, want %q", cfg.Legacy.ResolvedInstance(), "http://redlib:8080")
	}
}

func TestValidateOAuthTokenBackend(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	cfg.OAuth.Tokens = []OAuthTokenConfig{
		{ClientID: "test", Backend: "invalid"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should fail for invalid OAuth backend")
	}
}

func TestStringRedactsDSN(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://secret:pass@host/db"
	cfg.Redis.Addr = "localhost:6379"
	s := cfg.String()
	if s == "" {
		t.Fatal("String() should not be empty")
	}
	if contains(s, "secret") || contains(s, "pass") {
		t.Errorf("String() should redact DSN, got: %s", s)
	}
}

func TestIsSettingExplicitlySet(t *testing.T) {
	t.Setenv("REDMEMO_DEFAULT_THEME", "dark")
	if !IsSettingExplicitlySet("theme") {
		t.Error("theme should be explicitly set")
	}
	if IsSettingExplicitlySet("layout") {
		t.Error("layout should not be explicitly set")
	}
	if IsSettingExplicitlySet("nonexistent") {
		t.Error("nonexistent setting should not be explicitly set")
	}
}

func minimalConfigYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`
postgres:
  dsn: "postgres://localhost/test"
redis:
  addr: "localhost:6379"
media:
  root_path: "/tmp/media"
`), 0644)
	return path
}

func TestTranslateRedlibDefaultEnvVars(t *testing.T) {
	t.Setenv("REDLIB_DEFAULT_THEME", "dark")
	t.Setenv("REDLIB_DEFAULT_SHOW_NSFW", "on")
	t.Setenv("REDLIB_DEFAULT_AUTOPLAY_VIDEOS", "on")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_DEFAULT_THEME") != "dark" {
		t.Errorf("REDLIB_DEFAULT_THEME not translated: REDMEMO_DEFAULT_THEME=%q", os.Getenv("REDMEMO_DEFAULT_THEME"))
	}
	if os.Getenv("REDMEMO_DEFAULT_SHOW_NSFW") != "on" {
		t.Errorf("REDLIB_DEFAULT_SHOW_NSFW not translated")
	}
	if os.Getenv("REDMEMO_DEFAULT_AUTOPLAY_VIDEOS") != "on" {
		t.Errorf("REDLIB_DEFAULT_AUTOPLAY_VIDEOS not translated")
	}
}

func TestTranslateLibredditEnvVars(t *testing.T) {
	t.Setenv("REDMEMO_DEFAULT_THEME", "")
	t.Setenv("LIBREDDIT_DEFAULT_THEME", "nord")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_DEFAULT_THEME") != "nord" {
		t.Errorf("LIBREDDIT_DEFAULT_THEME not translated: REDMEMO_DEFAULT_THEME=%q", os.Getenv("REDMEMO_DEFAULT_THEME"))
	}
}

func TestTranslateRedmemoTakesPrecedence(t *testing.T) {
	t.Setenv("REDMEMO_DEFAULT_THEME", "gruvbox")
	t.Setenv("REDLIB_DEFAULT_THEME", "dark")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_DEFAULT_THEME") != "gruvbox" {
		t.Errorf("REDMEMO_DEFAULT_THEME should not be overwritten by REDLIB_, got %q", os.Getenv("REDMEMO_DEFAULT_THEME"))
	}
}

func TestTranslateRedlibOverLibreddit(t *testing.T) {
	t.Setenv("REDLIB_DEFAULT_LAYOUT", "compact")
	t.Setenv("LIBREDDIT_DEFAULT_LAYOUT", "card")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_DEFAULT_LAYOUT") != "compact" {
		t.Errorf("REDLIB_ should win over LIBREDDIT_, got %q", os.Getenv("REDMEMO_DEFAULT_LAYOUT"))
	}
}

func TestTranslateInstanceVars(t *testing.T) {
	t.Setenv("REDLIB_SFW_ONLY", "on")
	t.Setenv("REDLIB_BANNER", "Welcome!")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_SFW_ONLY") != "on" {
		t.Errorf("REDLIB_SFW_ONLY not translated")
	}
	if os.Getenv("REDMEMO_BANNER") != "Welcome!" {
		t.Errorf("REDLIB_BANNER not translated")
	}
}

func TestTranslateSubscriptionsAndFilters(t *testing.T) {
	t.Setenv("REDMEMO_DEFAULT_SUBSCRIPTIONS", "")
	t.Setenv("REDMEMO_DEFAULT_FILTERS", "")
	t.Setenv("REDLIB_DEFAULT_SUBSCRIPTIONS", "golang+rust+linux")
	t.Setenv("REDLIB_DEFAULT_FILTERS", "memes+pics")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_DEFAULT_SUBSCRIPTIONS") != "golang+rust+linux" {
		t.Errorf("subscriptions not translated: %q", os.Getenv("REDMEMO_DEFAULT_SUBSCRIPTIONS"))
	}
	if os.Getenv("REDMEMO_DEFAULT_FILTERS") != "memes+pics" {
		t.Errorf("filters not translated: %q", os.Getenv("REDMEMO_DEFAULT_FILTERS"))
	}

	if !IsSettingExplicitlySet("subscriptions") {
		t.Error("subscriptions should be explicitly set after translation")
	}
	if GetExplicitSetting("subscriptions") != "golang+rust+linux" {
		t.Errorf("GetExplicitSetting(subscriptions) = %q", GetExplicitSetting("subscriptions"))
	}
	if !IsSettingExplicitlySet("filters") {
		t.Error("filters should be explicitly set after translation")
	}
}

func TestTranslateInstancePrecedence(t *testing.T) {
	t.Setenv("REDMEMO_SFW_ONLY", "")
	t.Setenv("REDLIB_SFW_ONLY", "on")
	t.Setenv("LIBREDDIT_SFW_ONLY", "off")

	path := minimalConfigYAML(t)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if os.Getenv("REDMEMO_SFW_ONLY") != "on" {
		t.Errorf("REDLIB_ should win over LIBREDDIT_, got REDMEMO_SFW_ONLY=%q", os.Getenv("REDMEMO_SFW_ONLY"))
	}
}

func TestTranslatePort(t *testing.T) {
	t.Setenv("PORT", "3000")

	path := minimalConfigYAML(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Listen != ":3000" {
		t.Errorf("PORT=3000 should translate to Listen=:3000, got %q", cfg.Server.Listen)
	}
}

func TestTranslatePortWithColon(t *testing.T) {
	t.Setenv("REDMEMO_SERVER_LISTEN", "")
	t.Setenv("REDLIB_PORT", ":9090")

	path := minimalConfigYAML(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Listen != ":9090" {
		t.Errorf("REDLIB_PORT=:9090 should translate to Listen=:9090, got %q", cfg.Server.Listen)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && searchString(haystack, needle)
}

func searchString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
