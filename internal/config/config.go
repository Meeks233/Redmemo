package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Auth      AuthConfig      `yaml:"auth"`
	Legacy    LegacyConfig    `yaml:"legacy"`
	Postgres  PostgresConfig  `yaml:"postgres"`
	Redis     RedisConfig     `yaml:"redis"`
	Media     MediaConfig     `yaml:"media"`
	OAuth     OAuthConfig     `yaml:"oauth"`
	RateLimit RateLimitConfig `yaml:"ratelimit"`
	Prefetch  PrefetchConfig  `yaml:"prefetch"`
	HRLimit   HRLimitConfig   `yaml:"hrlimit"`
	Render    RenderConfig    `yaml:"render"`
	SEO       SEOConfig       `yaml:"seo"`
}

// SEOConfig controls how the instance presents itself to search engines.
// Off by default — only the instance owner who actually wants the archive
// pages crawled flips AllowIndexing on. When off, robots.txt is "Disallow: /",
// sitemap.xml 404s, and every page keeps noindex,nofollow. The "what this
// instance archives" story (archive hub + per-sub pages + sitemap of subs)
// only goes live once the owner opts in.
type SEOConfig struct {
	AllowIndexing bool   `yaml:"allow_indexing"`
	CanonicalHost string `yaml:"canonical_host"` // e.g. "https://memo.example.com" — used for absolute URLs in sitemap + <link rel=canonical>
}

type HRLimitConfig struct {
	Enabled     bool          `yaml:"enabled"`
	L1Window    time.Duration `yaml:"l1_window"`
	L1Threshold int           `yaml:"l1_threshold"`
	L2Window    time.Duration `yaml:"l2_window"`
	L2Threshold int           `yaml:"l2_threshold"`
	L3Window    time.Duration `yaml:"l3_window"`
	L3Threshold int           `yaml:"l3_threshold"`
}

// AuthConfig gates the settings UI behind TOTP. ServerSecret is the
// pre-shared secret required before TOTP enrollment or re-enrollment is
// allowed; it MUST be supplied via REDMEMO_SERVER_SECRET — startup refuses
// to launch the HTTP server without it, UNLESS BypassAuth is on.
//
// BypassAuth (REDMEMO_AUTH_BYPASS) disables the TOTP gate entirely: both
// /settings and /debug become reachable without any cookie or code. Intended
// for trusted homelab deployments where access is already gated by an outer
// layer (Tailscale ACL, a reverse-proxy auth provider, a VPN, …). When on,
// REDMEMO_SERVER_SECRET is no longer required. NEVER enable this on a
// public-facing instance — the settings UI accepts unauthenticated POSTs in
// this mode (the same-origin CSRF check is the only remaining brake).
type AuthConfig struct {
	ServerSecret string `yaml:"server_secret"`
	BypassAuth   bool   `yaml:"bypass_auth"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	// TrustedProxyCIDRs lists CIDR blocks (or single IPs) whose X-Forwarded-For
	// header we will honor when deriving the per-request client IP for
	// auth-gate lockout. When empty (default), XFF is ignored entirely and the
	// lockout uses RemoteAddr — safe but coarse behind a reverse proxy. Add the
	// reverse proxy's IP/CIDR here ("127.0.0.1/32", "10.0.0.0/8") only when you
	// trust it not to forward attacker-controlled XFF headers.
	TrustedProxyCIDRs []string `yaml:"trusted_proxy_cidrs"`
}

type LegacyConfig struct {
	SyncEnabled bool   `yaml:"sync_enabled"`
	Instance    string `yaml:"instance"`
}

func (lc *LegacyConfig) ResolvedInstance() string {
	if lc.Instance != "" {
		return lc.Instance
	}
	return "http://redlib:8080"
}

type PostgresConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

type RedisConfig struct {
	Addr        string `yaml:"addr"`
	Password    string `yaml:"password"`
	DB          int    `yaml:"db"`
	MaxMemoryMB int    `yaml:"max_memory_mb"`
}

type MediaConfig struct {
	RootPath              string        `yaml:"root_path"`
	MaxSizeGB             float64       `yaml:"max_size_gb"`
	EvictionCheckInterval time.Duration `yaml:"eviction_check_interval"`
	EvictionThreshold     float64       `yaml:"eviction_threshold"`
}

type OAuthConfig struct {
	Tokens []OAuthTokenConfig `yaml:"tokens"`
}

type OAuthTokenConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	Backend      string `yaml:"backend"`
}

type RateLimitConfig struct {
	WindowSize     int           `yaml:"window_size"`
	WindowDuration time.Duration `yaml:"window_duration"`
	SafetyBuffer   int           `yaml:"safety_buffer"`
}

// PrefetchConfig only carries the master enable switch and dispatcher tick.
// The crawl list lives in the DB (settings key `prefetch_subs`) and is owned
// by the settings UI / `REDMEMO_DEFAULT_PREFETCH_SUBS` env override — config
// has no authority over which subs run.
type PrefetchConfig struct {
	Enabled       bool          `yaml:"enabled"`
	CheckInterval time.Duration `yaml:"check_interval"`
}

type RenderConfig struct {
	BrandName        string `yaml:"brand_name"`
	ShowArchiveBadge bool   `yaml:"show_archive_badge"`
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:       ":8080",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 60 * time.Second,
		},
		Legacy: LegacyConfig{
			SyncEnabled: true,
		},
		Postgres: PostgresConfig{
			MaxOpenConns: 50,
			MaxIdleConns: 10,
		},
		Redis: RedisConfig{
			DB:          0,
			MaxMemoryMB: 256,
		},
		Media: MediaConfig{
			RootPath:              "/data/media",
			MaxSizeGB:             50.0,
			EvictionCheckInterval: 5 * time.Minute,
			EvictionThreshold:     0.8,
		},
		RateLimit: RateLimitConfig{
			WindowSize:     500,
			WindowDuration: 10 * time.Minute,
			SafetyBuffer:   50,
		},
		Prefetch: PrefetchConfig{
			Enabled:       true,
			CheckInterval: 30 * time.Second,
		},
		HRLimit: HRLimitConfig{
			Enabled:     true,
			L1Window:    5 * time.Second,
			L1Threshold: 5,
			L2Window:    30 * time.Second,
			L2Threshold: 15,
			L3Window:    5 * time.Minute,
			L3Threshold: 50,
		},
		Render: RenderConfig{
			BrandName:        "RedMemo",
			ShowArchiveBadge: true,
		},
	}
}

func Load(path string) (*Config, error) {
	cfg := defaults()

	// config.yaml is fully optional: a missing file just means "use defaults
	// plus REDMEMO_* env overrides". Only fail when the file exists but is
	// unreadable or malformed — those are real misconfigurations to surface.
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
		log.Printf("config: %s not found, running on defaults + env vars", path)
	default:
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	translateLegacyEnvVars()
	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate: %w", err)
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	var errs []error

	if c.Postgres.DSN == "" {
		errs = append(errs, errors.New("postgres.dsn is required"))
	}
	if c.Redis.Addr == "" {
		errs = append(errs, errors.New("redis.addr is required"))
	}
	if c.Media.RootPath == "" {
		errs = append(errs, errors.New("media.root_path is required"))
	}
	if c.Media.EvictionThreshold < 0 || c.Media.EvictionThreshold > 1 {
		errs = append(errs, errors.New("media.eviction_threshold must be between 0 and 1"))
	}
	if c.RateLimit.WindowSize <= 0 {
		errs = append(errs, errors.New("ratelimit.window_size must be positive"))
	}
	if c.RateLimit.WindowDuration <= 0 {
		errs = append(errs, errors.New("ratelimit.window_duration must be positive"))
	}
	if c.Media.EvictionCheckInterval <= 0 {
		errs = append(errs, errors.New("media.eviction_check_interval must be positive"))
	}
	for i, tok := range c.OAuth.Tokens {
		if tok.ClientID == "" {
			errs = append(errs, fmt.Errorf("oauth.tokens[%d].client_id is required", i))
		}
		switch tok.Backend {
		case "", "mobile_spoof", "generic_web":
		default:
			errs = append(errs, fmt.Errorf("oauth.tokens[%d].backend must be \"mobile_spoof\" or \"generic_web\"", i))
		}
	}
	return errors.Join(errs...)
}

// translateLegacyEnvVars scans the process environment for REDLIB_* and
// LIBREDDIT_* variables and injects REDMEMO_* equivalents — but only when
// the REDMEMO_* version is not already set. This lets users switch their
// docker-compose from the redlib image to redmemo without touching env vars.
//
// The scan is fully automatic: any new REDLIB_FOO_BAR variable is translated
// to REDMEMO_FOO_BAR without requiring code changes here.
//
// Precedence: REDMEMO_* > REDLIB_* > LIBREDDIT_*
func translateLegacyEnvVars() {
	// Pass 1: collect REDLIB_* (higher priority)
	// Pass 2: collect LIBREDDIT_* (lower priority, only fills gaps)
	for _, prefix := range []string{"REDLIB_", "LIBREDDIT_"} {
		for _, entry := range os.Environ() {
			eqIdx := strings.IndexByte(entry, '=')
			if eqIdx < 0 {
				continue
			}
			key := entry[:eqIdx]
			val := entry[eqIdx+1:]
			if val == "" {
				continue
			}

			if !strings.HasPrefix(key, prefix) {
				continue
			}

			suffix := key[len(prefix):]
			target := "REDMEMO_" + suffix

			if os.Getenv(target) != "" {
				continue
			}
			os.Setenv(target, val)
			log.Printf("config: translated %s → %s=%s", key, target, val)
		}
	}

	// PORT → REDMEMO_SERVER_LISTEN  (Heroku-style port convention)
	if os.Getenv("REDMEMO_SERVER_LISTEN") == "" {
		for _, key := range []string{"REDLIB_PORT", "PORT"} {
			if port := os.Getenv(key); port != "" {
				if !strings.HasPrefix(port, ":") {
					port = ":" + port
				}
				os.Setenv("REDMEMO_SERVER_LISTEN", port)
				log.Printf("config: translated %s → REDMEMO_SERVER_LISTEN=%s", key, port)
				break
			}
		}
	}
}

// applyEnvOverrides applies REDMEMO_* environment variable overrides to the
// config struct. Runs after translateLegacyEnvVars so translated values are
// already available.
func applyEnvOverrides(cfg *Config) {
	envMap := map[string]*string{
		"REDMEMO_SERVER_LISTEN":      &cfg.Server.Listen,
		"REDMEMO_POSTGRES_DSN":       &cfg.Postgres.DSN,
		"REDMEMO_REDIS_ADDR":         &cfg.Redis.Addr,
		"REDMEMO_REDIS_PASSWORD":     &cfg.Redis.Password,
		"REDMEMO_MEDIA_ROOT_PATH":    &cfg.Media.RootPath,
		"REDMEMO_RENDER_BRAND_NAME":  &cfg.Render.BrandName,
		"REDMEMO_LEGACY_INSTANCE":    &cfg.Legacy.Instance,
		"REDMEMO_SERVER_SECRET":      &cfg.Auth.ServerSecret,
		"REDMEMO_SEO_CANONICAL_HOST": &cfg.SEO.CanonicalHost,
	}

	for env, ptr := range envMap {
		if v := os.Getenv(env); v != "" {
			*ptr = v
		}
	}

	boolEnv := map[string]*bool{
		"REDMEMO_LEGACY_SYNC":               &cfg.Legacy.SyncEnabled,
		"REDMEMO_PREFETCH_ENABLED":          &cfg.Prefetch.Enabled,
		"REDMEMO_RENDER_SHOW_ARCHIVE_BADGE": &cfg.Render.ShowArchiveBadge,
		"REDMEMO_SEO_ALLOW_INDEXING":        &cfg.SEO.AllowIndexing,
		"REDMEMO_AUTH_BYPASS":               &cfg.Auth.BypassAuth,
		"REDMEMO_HRLIMIT_ENABLED":           &cfg.HRLimit.Enabled,
	}
	for env, ptr := range boolEnv {
		if v := os.Getenv(env); v != "" {
			*ptr = parseBool(v)
		}
	}

	intEnv := map[string]*int{
		"REDMEMO_RATELIMIT_WINDOW_SIZE":   &cfg.RateLimit.WindowSize,
		"REDMEMO_RATELIMIT_SAFETY_BUFFER": &cfg.RateLimit.SafetyBuffer,
		"REDMEMO_HRLIMIT_L1_THRESHOLD":    &cfg.HRLimit.L1Threshold,
		"REDMEMO_HRLIMIT_L2_THRESHOLD":    &cfg.HRLimit.L2Threshold,
		"REDMEMO_HRLIMIT_L3_THRESHOLD":    &cfg.HRLimit.L3Threshold,
	}
	for env, ptr := range intEnv {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*ptr = n
			} else {
				log.Printf("config: %s=%q is not a valid integer; ignoring", env, v)
			}
		}
	}

	durationEnv := map[string]*time.Duration{
		"REDMEMO_SERVER_READ_TIMEOUT":           &cfg.Server.ReadTimeout,
		"REDMEMO_SERVER_WRITE_TIMEOUT":          &cfg.Server.WriteTimeout,
		"REDMEMO_MEDIA_EVICTION_CHECK_INTERVAL": &cfg.Media.EvictionCheckInterval,
		"REDMEMO_RATELIMIT_WINDOW_DURATION":     &cfg.RateLimit.WindowDuration,
		"REDMEMO_PREFETCH_CHECK_INTERVAL":       &cfg.Prefetch.CheckInterval,
		"REDMEMO_HRLIMIT_L1_WINDOW":             &cfg.HRLimit.L1Window,
		"REDMEMO_HRLIMIT_L2_WINDOW":             &cfg.HRLimit.L2Window,
		"REDMEMO_HRLIMIT_L3_WINDOW":             &cfg.HRLimit.L3Window,
	}
	for env, ptr := range durationEnv {
		if v := os.Getenv(env); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				*ptr = d
			} else {
				log.Printf("config: %s=%q is not a valid duration; ignoring", env, v)
			}
		}
	}

	if v := os.Getenv("REDMEMO_MEDIA_MAX_SIZE_GB"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Media.MaxSizeGB = f
		} else {
			log.Printf("config: REDMEMO_MEDIA_MAX_SIZE_GB=%q is not a valid float; ignoring", v)
		}
	}
	if v := os.Getenv("REDMEMO_MEDIA_EVICTION_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Media.EvictionThreshold = f
		} else {
			log.Printf("config: REDMEMO_MEDIA_EVICTION_THRESHOLD=%q is not a valid float; ignoring", v)
		}
	}

	// Comma-separated CIDR list for the reverse-proxy XFF allowlist. Empty
	// entries are dropped; whitespace trimmed. Set under nginx/caddy as
	// REDMEMO_SERVER_TRUSTED_PROXY_CIDRS=127.0.0.1/32,10.0.0.0/8.
	if v := os.Getenv("REDMEMO_SERVER_TRUSTED_PROXY_CIDRS"); v != "" {
		var cidrs []string
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				cidrs = append(cidrs, s)
			}
		}
		cfg.Server.TrustedProxyCIDRs = cidrs
	}
}

func settingEnvName(cookieName string) string {
	return "REDMEMO_DEFAULT_" + strings.ToUpper(cookieName)
}

func IsSettingExplicitlySet(settingName string) bool {
	return os.Getenv(settingEnvName(settingName)) != ""
}

func GetExplicitSetting(settingName string) string {
	return os.Getenv(settingEnvName(settingName))
}

// ScanExplicitSettings returns all REDMEMO_DEFAULT_* env vars as a
// cookie-name → value map. Fully dynamic — no hardcoded list.
func ScanExplicitSettings() map[string]string {
	const prefix = "REDMEMO_DEFAULT_"
	settings := make(map[string]string)
	for _, entry := range os.Environ() {
		eqIdx := strings.IndexByte(entry, '=')
		if eqIdx < 0 {
			continue
		}
		key := entry[:eqIdx]
		val := entry[eqIdx+1:]
		if val == "" || !strings.HasPrefix(key, prefix) {
			continue
		}
		cookieName := strings.ToLower(key[len(prefix):])
		settings[cookieName] = val
	}
	return settings
}

func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	// "on" mirrors the cookie/settings-page convention used by every
	// REDMEMO_DEFAULT_* toggle — accept it here so REDMEMO_AUTH_BYPASS,
	// REDMEMO_PREFETCH_ENABLED, etc. behave identically to the per-user
	// settings without each call site reinventing parsing.
	return s == "true" || s == "1" || s == "yes" || s == "on"
}

// String returns a redacted summary of the config for logging.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{server=%s, legacy_sync=%v/%s, postgres=***, redis=%s, media=%s/%.1fGB, oauth=%d tokens, prefetch=%v}",
		c.Server.Listen,
		c.Legacy.SyncEnabled, c.Legacy.ResolvedInstance(),
		c.Redis.Addr,
		c.Media.RootPath, c.Media.MaxSizeGB,
		len(c.OAuth.Tokens),
		c.Prefetch.Enabled,
	)
}
