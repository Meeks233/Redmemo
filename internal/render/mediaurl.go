package render

import "strings"

// WithDownloadTitle returns rawURL with a `dl_title` query parameter appended.
// The media proxy uses it to set a friendly Content-Disposition filename for
// video and GIF responses (see internal/media/filename.go). title is sent
// raw — the proxy sanitizes it server-side (spaces → underscores, unsafe
// characters dropped, length-capped) so templates don't have to.
//
// Returns rawURL unchanged when either side is empty, when rawURL already
// carries a dl_title, or when rawURL has a fragment (we'd corrupt it by
// appending after the #).
func WithDownloadTitle(rawURL, title string) string {
	if rawURL == "" || title == "" {
		return rawURL
	}
	if strings.Contains(rawURL, "dl_title=") {
		return rawURL
	}
	if strings.Contains(rawURL, "#") {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "dl_title=" + queryEscape(title)
}

// WithLongMark appends `long=1` to rawURL — a marker the media proxy reads to
// drop the priority of the resulting CDN download to the bottom of the gate
// (Priority.Long). Used only for clips exceeding the user-configured
// unlocked via the long-video gate. Returns rawURL unchanged on empty input
// or when the marker is already present.
func WithLongMark(rawURL string) string {
	if rawURL == "" || strings.Contains(rawURL, "long=1") {
		return rawURL
	}
	if strings.Contains(rawURL, "#") {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "long=1"
}

// queryEscape percent-encodes the bytes that must not appear in a query value.
// We avoid net/url.QueryEscape so spaces become %20 (not +) — the proxy treats
// either as a separator-to-underscore, but %20 reads cleaner in source view.
func queryEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
