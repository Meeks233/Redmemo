package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/redmemo/redmemo/internal/config"
)

type RateLimitState struct {
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
	Used      int       `json:"used"`
}

type Cache struct {
	client *redis.Client
}

func New(cfg config.RedisConfig) (*Cache, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Cache{client: client}, nil
}

// --- HTML page cache ---

func htmlKey(urlPath string) string {
	return "cache:html:" + urlPath
}

func (c *Cache) GetHTML(ctx context.Context, urlPath string) ([]byte, error) {
	data, err := c.client.Get(ctx, htmlKey(urlPath)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Cache) PutHTML(ctx context.Context, urlPath string, html []byte, ttl time.Duration) error {
	return c.client.Set(ctx, htmlKey(urlPath), html, ttl).Err()
}

func (c *Cache) InvalidateHTML(ctx context.Context, urlPath string) error {
	return c.client.Del(ctx, htmlKey(urlPath)).Err()
}

// InvalidateHTMLPrefix drops every cached HTML entry whose key begins with the
// given URL path — used to flush all per-prefs / per-query variants of one page
// when a user-triggered refresh changes the underlying archive.
func (c *Cache) InvalidateHTMLPrefix(ctx context.Context, urlPath string) error {
	// Escape Redis glob metacharacters in the user-influenced path before
	// appending the "*" wildcard. Without this, a path segment containing '*',
	// '?', '[' or '\' (these routes don't validate {sub}/{id}) would over-match
	// and flush unrelated pages' cache entries, thrashing the shared HTML cache.
	pattern := escapeRedisGlob(htmlKey(urlPath)) + "*"
	return c.scanDelete(ctx, pattern)
}

// escapeRedisGlob backslash-escapes the metacharacters Redis SCAN MATCH treats
// as glob syntax so the input is matched literally.
func escapeRedisGlob(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)
	return r.Replace(s)
}

// InvalidateAllHTML drops every cache:html:* entry. Called after a settings
// save because changes to site defaults affect what anonymous visitors render,
// and per-prefs hashing alone can't reach already-cached anonymous variants.
func (c *Cache) InvalidateAllHTML(ctx context.Context) error {
	return c.scanDelete(ctx, "cache:html:*")
}

func (c *Cache) scanDelete(ctx context.Context, pattern string) error {
	iter := c.client.Scan(ctx, 0, pattern, 200).Iterator()
	var batch []string
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= 500 {
			if err := c.client.Del(ctx, batch...).Err(); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err := iter.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		return c.client.Del(ctx, batch...).Err()
	}
	return nil
}

// --- Rate limit state ---

func (c *Cache) GetRateLimitState(ctx context.Context) (*RateLimitState, error) {
	pipe := c.client.Pipeline()
	remainingCmd := pipe.Get(ctx, "ratelimit:remaining")
	resetAtCmd := pipe.Get(ctx, "ratelimit:reset_at")
	usedCmd := pipe.Get(ctx, "ratelimit:used")
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}

	state := &RateLimitState{}

	if v, err := remainingCmd.Int(); err == nil {
		state.Remaining = v
	}
	if v, err := resetAtCmd.Int64(); err == nil {
		state.ResetAt = time.Unix(v, 0)
	}
	if v, err := usedCmd.Int(); err == nil {
		state.Used = v
	}

	return state, nil
}

func (c *Cache) SetRateLimitState(ctx context.Context, state *RateLimitState) error {
	pipe := c.client.Pipeline()
	pipe.Set(ctx, "ratelimit:remaining", state.Remaining, 0)
	pipe.Set(ctx, "ratelimit:reset_at", state.ResetAt.Unix(), 0)
	pipe.Set(ctx, "ratelimit:used", state.Used, 0)
	_, err := pipe.Exec(ctx)
	return err
}

// --- OAuth runtime ---

func (c *Cache) GetOAuthRuntime(ctx context.Context) (currentTokenID int, rollingOver bool, err error) {
	pipe := c.client.Pipeline()
	tokenCmd := pipe.Get(ctx, "oauth:current_token_id")
	rollingCmd := pipe.Get(ctx, "oauth:rolling_over")
	_, err = pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return 0, false, err
	}

	if v, e := tokenCmd.Int(); e == nil {
		currentTokenID = v
	}
	if v, e := rollingCmd.Result(); e == nil {
		rollingOver = v == "1"
	}

	return currentTokenID, rollingOver, nil
}

func (c *Cache) SetOAuthRuntime(ctx context.Context, currentTokenID int, rollingOver bool) error {
	rolling := "0"
	if rollingOver {
		rolling = "1"
	}
	pipe := c.client.Pipeline()
	pipe.Set(ctx, "oauth:current_token_id", currentTokenID, 0)
	pipe.Set(ctx, "oauth:rolling_over", rolling, 0)
	_, err := pipe.Exec(ctx)
	return err
}

// --- Media access batch ---

func (c *Cache) RecordMediaAccess(ctx context.Context, originalURL string) error {
	return c.client.HSet(ctx, "media:access_batch", originalURL, time.Now().Unix()).Err()
}

// FlushMediaAccess atomically retrieves and deletes the media access batch.
// Uses a Lua script to ensure no writes are lost between HGETALL and DEL.
var flushScript = redis.NewScript(`
local data = redis.call('HGETALL', KEYS[1])
if #data > 0 then
    redis.call('DEL', KEYS[1])
end
return data
`)

func (c *Cache) FlushMediaAccess(ctx context.Context) (map[string]time.Time, error) {
	raw, err := flushScript.Run(ctx, c.client, []string{"media:access_batch"}).StringSlice()
	if err == redis.Nil || len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	result := make(map[string]time.Time, len(raw)/2)
	for i := 0; i < len(raw)-1; i += 2 {
		url := raw[i]
		ts, _ := strconv.ParseInt(raw[i+1], 10, 64)
		result[url] = time.Unix(ts, 0)
	}
	return result, nil
}

// --- Generic key-value ---

func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	val, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

// --- Health check ---

func (c *Cache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

func (c *Cache) Close() error {
	return c.client.Close()
}

// Client returns the underlying redis client so callers (e.g. hrlimit)
// can share the same connection pool.
func (c *Cache) Client() *redis.Client {
	return c.client
}

// MarshalJSON/UnmarshalJSON helpers for RateLimitState
func (s *RateLimitState) MarshalBinary() ([]byte, error) {
	return json.Marshal(s)
}

func (s *RateLimitState) UnmarshalBinary(data []byte) error {
	return json.Unmarshal(data, s)
}
