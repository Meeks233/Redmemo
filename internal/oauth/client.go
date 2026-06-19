package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
)

// httpDoer is the subset of tls_client.HttpClient the OAuth client depends on,
// narrowed so tests can inject a plain fhttp client.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

const (
	mobileClientID = "ohXpoqrZYub1kg"
	mobileEndpoint = "https://www.reddit.com/auth/v2/oauth/access-token/loid"
	authTimeout    = 5 * time.Second
	maxRetries     = 5
)

type TokenResult struct {
	AccessToken string
	ExpiresIn   int64
	Headers     map[string]string
	Identity    SpoofIdentity
}

type Client struct {
	httpClient httpDoer

	mu      sync.RWMutex
	profile *store.DeviceProfile
}

func NewClient(profile *store.DeviceProfile) *Client {
	return &Client{
		httpClient: transport.NewSpoofedClient(authTimeout),
		profile:    profile,
	}
}

// DeviceIdentity returns a SpoofIdentity built from the pinned device profile.
// A Client constructed without a profile (e.g. in tests) gets a generated one.
func (c *Client) DeviceIdentity() SpoofIdentity {
	c.mu.RLock()
	p := c.profile
	c.mu.RUnlock()
	if p != nil {
		return IdentityFromProfile(p)
	}
	return GenerateIdentity()
}

// Profile returns a copy of the current pinned device profile, or nil if the
// Client was constructed without one.
func (c *Client) Profile() *store.DeviceProfile {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.profile == nil {
		return nil
	}
	cp := *c.profile
	return &cp
}

// SetProfile swaps in a rotated device profile; subsequent auth requests mint
// tokens with the new version identity.
func (c *Client) SetProfile(p *store.DeviceProfile) {
	c.mu.Lock()
	c.profile = p
	c.mu.Unlock()
}

// Authenticate mints a fresh access token. mobile_spoof is the only backend:
// the former generic_web (browser-emulation) path was removed because emitting
// a browser User-Agent from the same session IP as the mobile client is a
// stealth tell. The config arg is retained for interface compatibility.
func (c *Client) Authenticate(_ config.OAuthTokenConfig) (*TokenResult, error) {
	var lastErr error

	for i := range maxRetries {
		result, err := c.mobileSpoofAuth()
		if err == nil {
			return result, nil
		}
		lastErr = fmt.Errorf("mobile_spoof attempt %d: %w", i+1, err)
	}

	return nil, fmt.Errorf("all auth attempts failed: %w", lastErr)
}

func (c *Client) Refresh(cfg config.OAuthTokenConfig) (*TokenResult, error) {
	return c.Authenticate(cfg)
}

func (c *Client) mobileSpoofAuth() (*TokenResult, error) {
	identity := c.DeviceIdentity()

	body := `{"scopes":["*","email","pii"]}`
	req, err := http.NewRequest("POST", mobileEndpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, v := range identity.Headers {
		req.Header.Set(k, v)
	}

	auth := "Basic " + basicAuth(mobileClientID, "")
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	transport.ApplyHeaderOrder(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mobile spoof: status %d: %s", resp.StatusCode, truncate(data, 200))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("mobile spoof: parse response: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("mobile spoof: empty access_token in response: %s", truncate(data, 200))
	}

	headers := make(map[string]string)
	if v := resp.Header.Get("x-reddit-loid"); v != "" {
		headers["x-reddit-loid"] = v
	}
	if v := resp.Header.Get("x-reddit-session"); v != "" {
		headers["x-reddit-session"] = v
	}
	for k, v := range identity.Headers {
		headers[k] = v
	}

	return &TokenResult{
		AccessToken: parsed.AccessToken,
		ExpiresIn:   parsed.ExpiresIn,
		Headers:     headers,
		Identity:    identity,
	}, nil
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// secretFieldRe matches JSON string fields that may carry token/session
// material so they can be redacted before a token-endpoint response body is
// echoed into an error/log line.
var secretFieldRe = regexp.MustCompile(`("(?:access_token|refresh_token|loid|session|session_tracker)"\s*:\s*)"[^"]*"`)

// truncate redacts known sensitive JSON fields and then caps the body length.
// Token endpoints occasionally return secret material in the body; never echo
// it verbatim into logs.
func truncate(data []byte, maxLen int) string {
	s := secretFieldRe.ReplaceAllString(string(data), `$1"<redacted>"`)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
