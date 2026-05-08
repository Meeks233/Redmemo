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
redlib:
  upstream: "http://redlib:8080"
  enabled: true
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
	if cfg.Redlib.Upstream != "http://redlib:8080" {
		t.Errorf("Redlib.Upstream = %q, want %q", cfg.Redlib.Upstream, "http://redlib:8080")
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
redlib:
  upstream: "http://redlib:8080"
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
}

func TestValidateMissingDSN(t *testing.T) {
	cfg := defaults()
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	cfg.Redlib.Upstream = "http://redlib:8080"
	// Postgres.DSN intentionally empty
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should fail when Postgres.DSN is empty")
	}
}

func TestValidateMissingRedisAddr(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Media.RootPath = "/tmp/media"
	cfg.Redlib.Upstream = "http://redlib:8080"
	// Redis.Addr intentionally empty
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should fail when Redis.Addr is empty")
	}
}

func TestValidateMissingRedlibUpstream(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	cfg.Redlib.Enabled = true
	// Redlib.Upstream intentionally empty
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() should fail when Redlib.Enabled but Upstream empty")
	}
}

func TestValidateRedlibDisabledNoUpstream(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	cfg.Redlib.Enabled = false
	// No upstream needed when disabled
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate() should pass when Redlib disabled: %v", err)
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	cfg.Redlib.Upstream = "http://redlib:8080"
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
	cfg.Redlib.Upstream = "http://redlib:8080"

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
redlib:
  upstream: "http://redlib:8080"
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

func TestValidateOAuthTokenBackend(t *testing.T) {
	cfg := defaults()
	cfg.Postgres.DSN = "postgres://localhost/test"
	cfg.Redis.Addr = "localhost:6379"
	cfg.Media.RootPath = "/tmp/media"
	cfg.Redlib.Upstream = "http://redlib:8080"
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
	// DSN should be redacted
	if contains(s, "secret") || contains(s, "pass") {
		t.Errorf("String() should redact DSN, got: %s", s)
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
