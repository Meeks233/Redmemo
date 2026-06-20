package render

import (
	"context"
	"html"
	"regexp"
	"strings"
)

// markLazyLinks annotates every bare external auto-link in an already-rendered
// post/comment body so the client-side loader (linkPreview.js) can upgrade it
// into a preview card lazily — only once the link scrolls into view.
//
// Why lazy + client-driven (vs. unfurling at render time): a body full of links
// (e.g. a "Small Projects" megathread of GitHub repos) otherwise fired a burst
// of cross-site fetches from the server IP at render time, which hosts like
// GitHub rate-limited — so links past the first handful degraded to plain text.
// Now nothing is fetched at render: each link carries a data-unfurl hint, the
// browser asks /api/unfurl for one preview at a time as the user scrolls (and
// loads the preview image/video directly, off RedMemo's back), and a link that
// can't be unfurled simply stays the plain link it already is.
//
// Only bare auto-links (visible text == href) are marked; a user-written
// labelled anchor keeps its text and is never touched. The anchor is emitted as
// the original link (so no-JS / fetch-failure degrades gracefully) plus the
// data-unfurl hint and a class the observer keys on.
func markLazyLinks(_ context.Context, body string) string {
	if body == "" || !strings.Contains(body, "<a ") {
		return body
	}
	return externalAutolinkRe.ReplaceAllStringFunc(body, func(match string) string {
		m := externalAutolinkRe.FindStringSubmatch(match)
		if len(m) != 3 {
			return match
		}
		href := html.UnescapeString(m[1])
		if strings.TrimSpace(href) != strings.TrimSpace(html.UnescapeString(m[2])) {
			return match // labelled anchor, not a bare auto-link
		}
		if isRedditOrImage(href) {
			return match
		}
		// m[1] is the already-escaped href; reuse it verbatim for both href and
		// the data hint so the attribute value stays well-formed.
		return `<a class="link-preview-lazy" data-unfurl="` + m[1] + `" href="` + m[1] +
			`" target="_blank" rel="nofollow noopener noreferrer">` + m[2] + `</a>`
	})
}

// isRedditOrImage reports whether a bare link should be left alone rather than
// marked for a preview card: Reddit-owned links (rendered in-site) and direct
// image/video URLs (already inline media).
func isRedditOrImage(href string) bool {
	low := strings.ToLower(href)
	for _, h := range []string{"reddit.com/", "redd.it/", "redditmedia.com/", "redditstatic.com/"} {
		if strings.Contains(low, "://"+h) || strings.Contains(low, "."+h) {
			return true
		}
	}
	return imageURLRe.MatchString(href)
}

// externalAutolinkRe matches an absolute-URL bare auto-link anchor in body HTML.
var externalAutolinkRe = regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*>([^<]+)</a>`)

// imageURLRe matches a URL whose path ends in a still-image/media extension —
// those are inline media, not link-preview candidates.
var imageURLRe = regexp.MustCompile(`(?i)\.(jpg|jpeg|png|gif|webp|bmp|svg|mp4|webm|mov|m4v)(\?|$)`)
