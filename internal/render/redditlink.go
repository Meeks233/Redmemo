package render

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// redditBase is the canonical off-site Reddit origin used when a body link must
// point back at reddit.com (upstream access disabled). It mirrors the literal
// every "view on reddit" trigger in the templates already uses, so the modal /
// no-JS navigation target stays identical across surfaces.
const redditBase = "https://www.reddit.com"

// anchorHrefRe captures an <a> opening tag's href attribute so reviseRedditLinks
// can swap only the URL while leaving every other attribute (and the link text)
// untouched. Group 1 is everything up to and including `href="`, group 2 is the
// (HTML-escaped) attribute value, group 3 is the remainder of the opening tag.
// The leading `\s` before `href` keeps this from matching `data-href` and the
// like.
var anchorHrefRe = regexp.MustCompile(`(?i)(<a\b[^>]*?\shref=")([^"]*)("[^>]*>)`)

// reviseRedditLinks applies the upstream-aware "intel" policy to every Reddit
// *content* link (subreddit / user / permalink) inside an already-rendered
// post or comment body. It is the render-time counterpart to the parse-time
// reddit.RewriteURLs, which unconditionally localizes those links before they
// are cached — so by the time a body reaches here a Reddit link is already a
// bare local path like "/r/sub/comments/id/slug". This function re-decides per
// request:
//
//   - upstreamAllowed  → keep the on-site path ("本站链接"): clicking stays on
//     RedMemo, which can fetch/serve the thread on demand. (No-op for the common
//     already-local case, so it is idempotent.)
//   - !upstreamAllowed → the instance is pinned cache-only, so an on-site path
//     would dead-end at a 404 / fuckreddit page for anything not archived. Point
//     the link back at reddit.com instead, where the content actually lives. The
//     global leave-to-Reddit confirmation (redditModal.js) then guards the click.
//
// Only the three Reddit content routes are touched (/r/, /u/, /user/). Media
// proxy paths (/img, /preview, /vid) and RedMemo's own chrome (/settings,
// /search, …) never match, and non-Reddit external links are left for the lazy
// preview-card path. Host validation on absolute URLs goes through net/url so a
// look-alike host ("reddit.com.evil.test") can never be mistaken for Reddit.
func reviseRedditLinks(body string, upstreamAllowed bool) string {
	if body == "" || !strings.Contains(body, "<a") {
		return body
	}
	return anchorHrefRe.ReplaceAllStringFunc(body, func(match string) string {
		m := anchorHrefRe.FindStringSubmatch(match)
		if len(m) != 4 {
			return match
		}
		// m[2] is the escaped attribute value (e.g. contains &amp;); decode it
		// before URL classification, then re-escape the result once on the way
		// back into the attribute so the markup stays well-formed.
		path, ok := redditContentPath(html.UnescapeString(m[2]))
		if !ok {
			return match
		}
		out := path
		if !upstreamAllowed {
			out = redditBase + path
		}
		return m[1] + html.EscapeString(out) + m[3]
	})
}

// redditContentPath classifies an anchor href as a Reddit content link and, when
// it is one, returns its canonical on-site path (leading slash, query/fragment
// preserved). Two shapes are recognized:
//
//   - an already-localized path emitted by reddit.RewriteURLs ("/r/…", "/u/…",
//     "/user/…"), the overwhelmingly common case after parse-time rewriting; and
//   - a stray absolute reddit.com URL (defensive — e.g. a body that bypassed the
//     parse-time rewrite) whose host is validated as Reddit-owned and whose path
//     is one of the same content routes.
//
// Anything else — media subdomains, redd.it short links, RedMemo chrome, or
// foreign hosts — returns ok=false and is left exactly as it was.
func redditContentPath(href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", false
	}
	if strings.Contains(href, "://") {
		u, err := url.Parse(href)
		if err != nil || !isRedditSiteHost(u.Host) || !isLocalRedditPath(u.Path) {
			return "", false
		}
		p := u.Path
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		if u.Fragment != "" {
			p += "#" + u.Fragment
		}
		return p, true
	}
	if isLocalRedditPath(href) {
		return href, true
	}
	return "", false
}

// isRedditSiteHost reports whether host is the reddit.com *site* (page content),
// excluding the redd.it media subdomains (i/preview/v.redd.it) which are inline
// media, not navigable Reddit pages. The port, if any, is ignored.
func isRedditSiteHost(host string) bool {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host == "reddit.com" || strings.HasSuffix(host, ".reddit.com")
}

// isLocalRedditPath reports whether p is one of the Reddit content routes that
// RewriteURLs produces and RedMemo serves locally. Deliberately narrow so it
// never collides with media proxy paths or RedMemo's own UI routes.
func isLocalRedditPath(p string) bool {
	return strings.HasPrefix(p, "/r/") ||
		strings.HasPrefix(p, "/u/") ||
		strings.HasPrefix(p, "/user/")
}
