package render

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/redmemo/redmemo/internal/reddit"
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

// normLink canonicalises a URL for de-duplication: trimmed, lower-cased, and
// with trailing slashes dropped so ".../posta" and ".../posta/" collapse. Must
// stay in lock-step with linkPreview.js's normURL so the server's link count and
// the browser's per-URL card dedup agree.
func normLink(href string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(href)), "/")
}

// uniqueExternalLinks returns the distinct bare external (non-reddit, non-image)
// auto-links in already-rendered body HTML — exactly the links markLazyLinks
// would upgrade into preview cards. The map is keyed by a normalised URL (so
// ".../x" and ".../x/" collapse) and values are the original href, so callers
// can both count uniques and recover a real URL to unfurl. It exists to gate
// carding: a body earns a card only when it carries a single such link.
func uniqueExternalLinks(body string) map[string]string {
	set := make(map[string]string)
	if body == "" || !strings.Contains(body, "<a ") {
		return set
	}
	for _, m := range externalAutolinkRe.FindAllStringSubmatch(body, -1) {
		if len(m) != 3 {
			continue
		}
		href := html.UnescapeString(m[1])
		if strings.TrimSpace(href) != strings.TrimSpace(html.UnescapeString(m[2])) {
			continue // labelled anchor, not a bare auto-link
		}
		if isRedditOrImage(href) {
			continue
		}
		set[normLink(href)] = href
	}
	return set
}

// listingPreviewURL returns the single external link a LISTING card should
// preview in its right-hand thumbnail strip, or "" if the post has none. For a
// link post it's the destination; for a SHORT self post it's the lone body link.
// This is what lets a single-link self post show its preview on the right strip
// (mirroring link posts) instead of as an inline body card. Long or multi-link
// posts return "" — no strip preview, no clutter.
func listingPreviewURL(post reddit.Post) string {
	// A post past the fold threshold gets no strip preview at all — long posts
	// stay plain, matching the inline-card gate. Applies to link AND self posts:
	// a link post with a long write-up (e.g. a "show & tell" with a full README in
	// the body) should not sprout a tall cropped thumbnail on the right.
	if isLongBody(post.Body) {
		return ""
	}
	switch post.PostType {
	case "link":
		if post.Media.URL != "" && !isRedditOrImage(post.Media.URL) {
			return post.Media.URL
		}
	case "self":
		if set := uniqueExternalLinks(string(reddit.EmbedBodyImages(post.Body))); len(set) == 1 {
			for _, orig := range set {
				return orig
			}
		}
	}
	return ""
}

// urlHost is the display host for a preview link's domain label — the bare
// hostname with a leading "www." dropped, falling back to the raw string if it
// doesn't parse.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
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
