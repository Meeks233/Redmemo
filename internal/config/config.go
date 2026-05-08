package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Redlib    RedlibConfig    `yaml:"redlib"`
	Postgres  PostgresConfig  `yaml:"postgres"`
	Redis     RedisConfig     `yaml:"redis"`
	Media     MediaConfig     `yaml:"media"`
	OAuth     OAuthConfig     `yaml:"oauth"`
	RateLimit RateLimitConfig `yaml:"ratelimit"`
	Prefetch  PrefetchConfig  `yaml:"prefetch"`
	Render    RenderConfig    `yaml:"render"`
}

type ServerConfig struct {
	Listen       string        `yaml:"listen"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

type RedlibConfig struct {
	Upstream string `yaml:"upstream"`
	Enabled  bool   `yaml:"enabled"`
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
	ArchiveOnProxy bool          `yaml:"archive_on_proxy"`
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
		Redlib: RedlibConfig{
			Enabled: true,
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
			ArchiveOnProxy: true,
		},
		Prefetch: PrefetchConfig{
			Enabled:       true,
			CheckInterval: 30 * time.Second,
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
	if c.Redlib.Enabled && c.Redlib.Upstream == "" {
		errs = append(errs, errors.New("redlib.upstream is required when redlib is enabled"))
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
		if tok.Backend != "mobile_spoof" && tok.Backend != "generic_web" {
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

// applyEnvOverrides applies environment variable overrides.
// Format: REDMEMO_SECTION_KEY (e.g. REDMEMO_POSTGRES_DSN).
func applyEnvOverrides(cfg *Config) {
	envMap := map[string]*string{
		"REDMEMO_SERVER_LISTEN":    &cfg.Server.Listen,
		"REDMEMO_REDLIB_UPSTREAM":  &cfg.Redlib.Upstream,
		"REDMEMO_POSTGRES_DSN":     &cfg.Postgres.DSN,
		"REDMEMO_REDIS_ADDR":       &cfg.Redis.Addr,
		"REDMEMO_REDIS_PASSWORD":   &cfg.Redis.Password,
		"REDMEMO_MEDIA_ROOT_PATH":  &cfg.Media.RootPath,
		"REDMEMO_RENDER_BRAND_NAME": &cfg.Render.BrandName,
	}

	for env, ptr := range envMap {
		if v := os.Getenv(env); v != "" {
			*ptr = v
		}
	}

	if v := os.Getenv("REDMEMO_REDLIB_ENABLED"); v != "" {
		cfg.Redlib.Enabled = parseBool(v)
	}
}

func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes"
}

// String returns a redacted summary of the config for logging.
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{server=%s, redlib=%v/%s, postgres=***, redis=%s, media=%s/%dGB, oauth=%d tokens, prefetch=%v/%d subs}",
		c.Server.Listen,
		c.Redlib.Enabled, c.Redlib.Upstream,
		c.Redis.Addr,
		c.Media.RootPath, c.Media.MaxSizeGB,
		len(c.OAuth.Tokens),
		c.Prefetch.Enabled, len(c.Prefetch.Subreddits),
	)
}
