package proxy

import (
	"regexp"
	"strconv"
	"strings"
)

var resetTimeRe = regexp.MustCompile(`(?i)reset\s+in[:\s]+(\d+)`)

// IsRateLimited reports whether the redlib response indicates rate limiting.
func IsRateLimited(statusCode int, body []byte) bool {
	if statusCode == 429 {
		return true
	}
	if statusCode == 200 || statusCode == 403 {
		s := strings.ToLower(string(body))
		return strings.Contains(s, "reddit rate limit exceeded") ||
			strings.Contains(s, "too many requests") ||
			strings.Contains(s, "rate limit")
	}
	return false
}

// ExtractResetTime attempts to extract the rate limit reset time in seconds
// from a redlib error page body. Returns 0 if extraction fails.
func ExtractResetTime(body []byte) int {
	m := resetTimeRe.FindSubmatch(body)
	if len(m) < 2 {
		return 0
	}
	secs, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return secs
}

// IsServerError reports whether the redlib response indicates a server error.
func IsServerError(statusCode int, body []byte) bool {
	if statusCode >= 500 {
		return true
	}
	s := string(body)
	return strings.Contains(s, "Reddit is having issues") ||
		strings.Contains(s, "Failed to parse page JSON data") ||
		strings.Contains(s, "expected value at line 1 column 1")
}
