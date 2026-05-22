package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
	"github.com/redmemo/redmemo/internal/useragent"
)

// httpDoer is the subset of tls_client.HttpClient the OAuth client depends on,
// narrowed so tests can inject a plain fhttp client.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

const (
	mobileClientID  = "ohXpoqrZYub1kg"
	mobileEndpoint  = "https://www.reddit.com/auth/v2/oauth/access-token/loid"
	genericWebAuth  = "Basic M1hmQkpXbGlIdnFBQ25YcmZJWWxMdzo="
	genericEndpoint = "https://www.reddit.com/api/v1/access_token"
	authTimeout     = 5 * time.Second
	maxRetries      = 5
)

type TokenResult struct {
	AccessToken string
	ExpiresIn   int64
	Headers     map[string]string
	Identity    SpoofIdentity
}

type Client struct {
	httpClient httpDoer
	browserUA  *useragent.Pool

	mu      sync.RWMutex
	profile *store.DeviceProfile
}

func NewClient(browserUA *useragent.Pool, profile *store.DeviceProfile) *Client {
	return &Client{
		httpClient: transport.NewSpoofedClient(authTimeout),
		browserUA:  browserUA,
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

// Authenticate mints a fresh access token. mobile_spoof is the only active
// backend; the generic_web (browser) path is intentionally decoupled — see
// genericWebAuth, kept as standby code for a future browser-emulation backend.
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

func (c *Client) genericWebAuth() (*TokenResult, error) {
	identity := genericWebIdentity(c.browserUA)

	formBody := "grant_type=https%3A%2F%2Foauth.reddit.com%2Fgrants%2Finstalled_client&device_id=" + identity.DeviceID
	req, err := http.NewRequest("POST", genericEndpoint, strings.NewReader(formBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", "www.reddit.com")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Authorization", genericWebAuth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-GPC", "1")
	req.Header.Set("Connection", "keep-alive")
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
		return nil, fmt.Errorf("generic web: status %d: %s", resp.StatusCode, truncate(data, 200))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("generic web: parse response: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("generic web: empty access_token in response: %s", truncate(data, 200))
	}

	headers := map[string]string{
		"Origin": "https://www.reddit.com",
	}
	if v := resp.Header.Get("x-reddit-loid"); v != "" {
		headers["x-reddit-loid"] = v
	}
	if v := resp.Header.Get("x-reddit-session"); v != "" {
		headers["x-reddit-session"] = v
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

func truncate(data []byte, maxLen int) string {
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "..."
}
