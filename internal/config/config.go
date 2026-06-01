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
// to launch the HTTP server without it.
type AuthConfig struct {
	ServerSecret string `yaml:"server_secret"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
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
	MaxSizeGB             int           `yaml:"max_size_gb"`
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

type PrefetchConfig struct {
	Enabled       bool                `yaml:"enabled"`
	CheckInterval time.Duration       `yaml:"check_interval"`
	Subreddits    []PrefetchSubConfig `yaml:"subreddits"`
}

type PrefetchSubConfig struct {
	Name          string `yaml:"name"`
	Sort          string `yaml:"sort"`
	MaxPages      int    `yaml:"max_pages"`
	FetchComments bool   `yaml:"fetch_comments"`
	FetchMedia    bool   `yaml:"fetch_media"`
	Priority      int    `yaml:"priority"`
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
			MaxSizeGB:             50,
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

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
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
	for i, sub := range c.Prefetch.Subreddits {
		if sub.Name == "" {
			errs = append(errs, fmt.Errorf("prefetch.subreddits[%d].name is required", i))
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
		"REDMEMO_SERVER_LISTEN":     &cfg.Server.Listen,
		"REDMEMO_POSTGRES_DSN":      &cfg.Postgres.DSN,
		"REDMEMO_REDIS_ADDR":        &cfg.Redis.Addr,
		"REDMEMO_REDIS_PASSWORD":    &cfg.Redis.Password,
		"REDMEMO_MEDIA_ROOT_PATH":   &cfg.Media.RootPath,
		"REDMEMO_RENDER_BRAND_NAME": &cfg.Render.BrandName,
		"REDMEMO_LEGACY_INSTANCE":   &cfg.Legacy.Instance,
		"REDMEMO_SERVER_SECRET":     &cfg.Auth.ServerSecret,
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
	}
	for env, ptr := range boolEnv {
		if v := os.Getenv(env); v != "" {
			*ptr = parseBool(v)
		}
	}

	intEnv := map[string]*int{
		"REDMEMO_MEDIA_MAX_SIZE_GB":       &cfg.Media.MaxSizeGB,
		"REDMEMO_RATELIMIT_WINDOW_SIZE":   &cfg.RateLimit.WindowSize,
		"REDMEMO_RATELIMIT_SAFETY_BUFFER": &cfg.RateLimit.SafetyBuffer,
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

	if v := os.Getenv("REDMEMO_MEDIA_EVICTION_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Media.EvictionThreshold = f
		} else {
			log.Printf("config: REDMEMO_MEDIA_EVICTION_THRESHOLD=%q is not a valid float; ignoring", v)
		}
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
	return s == "true" || s == "1" || s == "yes"
}

// String returns a redacted summary of the config for logging.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{server=%s, legacy_sync=%v/%s, postgres=***, redis=%s, media=%s/%dGB, oauth=%d tokens, prefetch=%v/%d subs}",
		c.Server.Listen,
		c.Legacy.SyncEnabled, c.Legacy.ResolvedInstance(),
		c.Redis.Addr,
		c.Media.RootPath, c.Media.MaxSizeGB,
		len(c.OAuth.Tokens),
		c.Prefetch.Enabled, len(c.Prefetch.Subreddits),
	)
}
