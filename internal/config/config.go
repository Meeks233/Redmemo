package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Legacy    LegacyConfig    `yaml:"legacy"`
	Postgres  PostgresConfig  `yaml:"postgres"`
	Redis     RedisConfig     `yaml:"redis"`
	Media     MediaConfig     `yaml:"media"`
	OAuth     OAuthConfig     `yaml:"oauth"`
	RateLimit RateLimitConfig `yaml:"ratelimit"`
	Prefetch  PrefetchConfig  `yaml:"prefetch"`
	HRLimit   HRLimitConfig   `yaml:"hrlimit"`
	Render    RenderConfig    `yaml:"render"`
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
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
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
		case "mobile_spoof", "generic_web", "password":
		default:
			errs = append(errs, fmt.Errorf("oauth.tokens[%d].backend must be \"mobile_spoof\", \"generic_web\", or \"password\"", i))
		}
		if tok.Backend == "password" {
			if tok.ClientSecret == "" {
				errs = append(errs, fmt.Errorf("oauth.tokens[%d].client_secret is required for password backend", i))
			}
			if tok.Username == "" {
				errs = append(errs, fmt.Errorf("oauth.tokens[%d].username is required for password backend", i))
			}
			if tok.Password == "" {
				errs = append(errs, fmt.Errorf("oauth.tokens[%d].password is required for password backend", i))
			}
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
	}

	for env, ptr := range envMap {
		if v := os.Getenv(env); v != "" {
			*ptr = v
		}
	}

	if v := os.Getenv("REDMEMO_LEGACY_SYNC"); v != "" {
		cfg.Legacy.SyncEnabled = parseBool(v)
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
