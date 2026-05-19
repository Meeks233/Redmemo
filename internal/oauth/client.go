package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/transport"
	"github.com/redmemo/redmemo/internal/useragent"
)

const (
	mobileClientID    = "ohXpoqrZYub1kg"
	mobileEndpoint    = "https://www.reddit.com/auth/v2/oauth/access-token/loid"
	genericWebAuth    = "Basic M1hmQkpXbGlIdnFBQ25YcmZJWWxMdzo="
	genericEndpoint   = "https://www.reddit.com/api/v1/access_token"
	authTimeout       = 5 * time.Second
	maxRetries        = 5
)

type TokenResult struct {
	AccessToken string
	ExpiresIn   int64
	Headers     map[string]string
	Identity    SpoofIdentity
}

type Client struct {
	httpClient *http.Client
	uaPool     *useragent.Pool
}

func NewClient(uaPool *useragent.Pool) *Client {
	return &Client{
		httpClient: transport.NewSpoofedClient(authTimeout),
		uaPool:     uaPool,
	}
}

func (c *Client) Authenticate(cfg config.OAuthTokenConfig) (*TokenResult, error) {
	var lastErr error

	if cfg.Backend == "" || cfg.Backend == "mobile_spoof" {
		for i := range maxRetries {
			result, err := c.mobileSpoofAuth()
			if err == nil {
				return result, nil
			}
			lastErr = fmt.Errorf("mobile_spoof attempt %d: %w", i+1, err)
		}
	}

	for i := range maxRetries {
		result, err := c.genericWebAuth()
		if err == nil {
			return result, nil
		}
		lastErr = fmt.Errorf("generic_web attempt %d: %w", i+1, err)
	}

	return nil, fmt.Errorf("all auth attempts failed: %w", lastErr)
}

func (c *Client) Refresh(cfg config.OAuthTokenConfig) (*TokenResult, error) {
	return c.Authenticate(cfg)
}

func (c *Client) mobileSpoofAuth() (*TokenResult, error) {
	identity := GenerateIdentity()

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
	identity := GenerateWebIdentity(c.uaPool)

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
