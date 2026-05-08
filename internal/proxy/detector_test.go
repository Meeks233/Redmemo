package proxy

import "testing"

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{"429 status", 429, "", true},
		{"429 with body", 429, "some body", true},
		{"200 with rate limit message", 200, `<h1>Reddit rate limit exceeded</h1>`, true},
		{"200 with too many requests", 200, `Error: Too Many Requests`, true},
		{"200 with rate limit lowercase", 200, `hit a rate limit, try again`, true},
		{"403 with rate limit", 403, `Reddit rate limit exceeded`, true},
		{"200 normal page", 200, `<html><body>Hello world</body></html>`, false},
		{"301 redirect", 301, "", false},
		{"500 server error", 500, "internal error", false},
		{"200 empty body", 200, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRateLimited(tt.statusCode, []byte(tt.body))
			if got != tt.want {
				t.Errorf("IsRateLimited(%d, %q) = %v, want %v", tt.statusCode, tt.body, got, tt.want)
			}
		})
	}
}

func TestExtractResetTime(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"reset in colon", "Rate limit will reset in: 342", 342},
		{"reset in space", "reset in 60 seconds", 60},
		{"no match", "some random page content", 0},
		{"empty", "", 0},
		{"large number", "reset in: 9999", 9999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractResetTime([]byte(tt.body))
			if got != tt.want {
				t.Errorf("ExtractResetTime(%q) = %d, want %d", tt.body, got, tt.want)
			}
		})
	}
}

func TestIsServerError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{"500", 500, "", true},
		{"502", 502, "bad gateway", true},
		{"503", 503, "", true},
		{"200 with issues text", 200, `<p>Reddit is having issues</p>`, true},
		{"200 normal", 200, "all good", false},
		{"404 not found", 404, "not found", false},
		{"429 rate limit", 429, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsServerError(tt.statusCode, []byte(tt.body))
			if got != tt.want {
				t.Errorf("IsServerError(%d, %q) = %v, want %v", tt.statusCode, tt.body, got, tt.want)
			}
		})
	}
}
