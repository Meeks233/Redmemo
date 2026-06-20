// Package unfurl turns a bare external link into a Telegram-style preview card
// by fetching the target's OpenGraph/Twitter-card metadata — the same mechanism
// every chat app (Telegram, Discord, Slack) uses to "unfurl" a pasted URL.
//
// We deliberately reuse the industry's existing plumbing instead of scraping
// ad-hoc:
//
//   - OpenGraph protocol (https://ogp.me) — og:title/description/image/site_name,
//     with the twitter:* card tags and <title> as graceful fallbacks. This is
//     what 99% of sites already emit for exactly this purpose.
//   - fxtwitter / fixupx — the community embed proxies that re-expose X/Twitter
//     OG tags to crawlers (Twitter itself serves none). Hostile hosts are
//     rewritten to their embed mirror, then unfurled like any other OG page.
//   - Jina AI Reader (r.jina.ai) — a public "fetch the web for a bot" endpoint
//     that renders JS and defeats the Cloudflare/anti-bot interstitials a raw
//     fetch hits (e.g. Stack Overflow's "Just a moment…"), returning the page's
//     parsed title/description/metadata as JSON. Used only as a last resort so
//     the privacy-preserving direct fetch is always tried first.
package unfurl

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/transport"
	"golang.org/x/net/html"
)

// Preview is the unfurled metadata for one external link — the fields a chat
// app surfaces in its link card. A Preview is "usable" once it carries at least
// a Title or an ImageURL; an empty one means the link could not be unfurled.
//
// ImageWide / VideoURL drive the card's display variant on the client:
//   - VideoURL set      → a playable embed (X/Twitter via fixupx's og:video),
//     streamed directly by the viewer's browser.
//   - ImageWide true    → a "summary_large_image" card: the preview image spans
//     the full card width on top (GitHub's 1280×640 repo card, news hero shots).
//   - otherwise         → a compact card with a small square thumbnail (a site
//     logo / favicon, e.g. Stack Overflow's apple-touch-icon).
type Preview struct {
	URL         string // the original link the card points at (never the proxy mirror)
	Title       string
	Description string
	ImageURL    string // absolute og:image, or "" when the page exposes none
	SiteName    string
	ImageWide   bool   // render the image as a full-width banner, not a thumbnail
	VideoURL    string // absolute og:video (X/Twitter via fixupx), or ""
}

// Usable reports whether the preview carries enough to render a card.
func (p *Preview) Usable() bool {
	return p != nil && (p.Title != "" || p.ImageURL != "" || p.VideoURL != "")
}

// httpDoer is the subset of tls_client.HttpClient we depend on, narrowed so
// tests can inject a plain fhttp client pointed at a mock server.
type httpDoer interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}

// crawlerUA is the User-Agent every outbound unfurl fetch presents. It names
// the same crawler agents (facebookexternalhit / Twitterbot / TelegramBot)
// sites whitelist for OpenGraph — and that the fxtwitter/fixupx mirrors key on
// to decide whether to serve OG tags versus redirect a human to the real page.
const crawlerUA = "facebookexternalhit/1.1 Twitterbot/1.0 (+https://github.com/Meeks233/redmemo) TelegramBot (like TwitterBot)"

// maxBodyBytes caps how much of a target page we read while hunting for <meta>
// tags. OG/Twitter tags live in <head>, so 768 KiB is generous; it stops a
// hostile or pathological page from streaming unbounded bytes into the parser.
const maxBodyBytes = 768 << 10

// hostFixups rewrites bot-hostile social hosts to the embed mirror that exposes
// OpenGraph tags to crawlers. Path and query are preserved verbatim, so
// x.com/jack/status/20 → fixupx.com/jack/status/20. This is the exact trick
// Telegram/Discord previews rely on for Twitter/X links, which serve no OG of
// their own.
var hostFixups = map[string]string{
	"twitter.com":        "fixupx.com",
	"www.twitter.com":    "fixupx.com",
	"mobile.twitter.com": "fixupx.com",
	"x.com":              "fixupx.com",
	"www.x.com":          "fixupx.com",
	"nitter.net":         "fixupx.com",
}

// fetcher fetches and parses one URL's preview. It is the pure core: a httpDoer
// plus an optional Jina fallback toggle, with no caching or persistence — the
// Service layer wraps it with the DB cache and single-flight.
type fetcher struct {
	client       httpDoer
	jinaFallback bool
}

// Fetch unfurls rawURL through the failover chain: host-fixup mirror (for X/
// Twitter) or the original host first, then — only if that yields nothing
// usable and jinaFallback is on — the Jina reader. The returned Preview always
// carries URL == rawURL (the mirror is an implementation detail the card must
// never leak). A nil Preview with nil error means "fetched but nothing usable".
func (f *fetcher) Fetch(ctx context.Context, rawURL string) (*Preview, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil
	}
	// SSRF boundary: never fetch (nor hand to the Jina reader) a link that
	// resolves to a private/loopback/metadata address.
	if !isPublicHTTPURL(rawURL) {
		return nil, nil
	}
	host := strings.ToLower(u.Hostname())

	fetchURL := rawURL
	if mirror, ok := hostFixups[host]; ok {
		mu := *u
		mu.Host = mirror
		fetchURL = mu.String()
	}

	if p, err := f.fetchOG(ctx, fetchURL, rawURL); err == nil && p.Usable() {
		return p, nil
	}

	if f.jinaFallback {
		if p, err := f.fetchJina(ctx, rawURL); err == nil && p.Usable() {
			return p, nil
		}
	}
	return nil, nil
}

// fetchOG GETs fetchURL with the crawler identity and parses its OpenGraph /
// Twitter-card / <title> metadata into a Preview whose URL is displayURL.
func (f *fetcher) fetchOG(ctx context.Context, fetchURL, displayURL string) (*Preview, error) {
	body, err := f.get(ctx, fetchURL, "text/html,application/xhtml+xml")
	if err != nil {
		return nil, err
	}
	defer body.Close()

	meta := parseMeta(io.LimitReader(body, maxBodyBytes))
	image := absImage(firstNonEmpty(meta["og:image"], meta["og:image:url"], meta["twitter:image"], meta["twitter:image:src"]), fetchURL)
	p := &Preview{
		URL:         displayURL,
		Title:       cleanText(firstNonEmpty(meta["og:title"], meta["twitter:title"], meta["__title__"])),
		Description: cleanText(firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"])),
		ImageURL:    image,
		SiteName:    firstNonEmpty(meta["og:site_name"], hostLabel(displayURL)),
		ImageWide:   isWideImage(meta, image),
		VideoURL:    directVideo(meta, fetchURL),
	}
	return p, nil
}

// directVideo returns an og:video URL only when it is a video the browser can
// actually play inline in a <video> element — a direct media file, not an HTML
// embed. Most sites (YouTube, Vimeo) set og:video:url to an /embed/ page with
// og:video:type=text/html; rendering that as <video src> just yields a broken
// black box, so those fall back to the thumbnail (image) card the way Telegram
// shows a YouTube link. Only a `video/*` type (or a direct video file extension)
// — what fxtwitter/fixupx expose for real tweet videos — is treated as playable.
func directVideo(meta map[string]string, base string) string {
	raw := firstNonEmpty(meta["og:video:secure_url"], meta["og:video:url"], meta["og:video"])
	if raw == "" {
		return ""
	}
	typ := strings.ToLower(strings.TrimSpace(meta["og:video:type"]))
	if strings.HasPrefix(typ, "video/") || strings.HasPrefix(typ, "application/x-mpegurl") || strings.HasPrefix(typ, "application/vnd.apple.mpegurl") {
		return absImage(raw, base)
	}
	if typ == "" && videoFileRe.MatchString(raw) {
		return absImage(raw, base)
	}
	return "" // HTML embed (text/html) or unknown — not inline-playable
}

// videoFileRe matches a URL path ending in a direct video file extension.
var videoFileRe = regexp.MustCompile(`(?i)\.(mp4|webm|mov|m4v|m3u8)(\?|#|$)`)

var brRe = regexp.MustCompile(`(?i)<br\s*/?>`)
var htmlTagRe = regexp.MustCompile(`<[^>]+>`)
var blankLinesRe = regexp.MustCompile(`\n{3,}`)

// cleanText normalizes an OG title/description for display as plain text. Some
// embed mirrors (notably fxtwitter/fixupx) put literal HTML in og:description —
// `<br>` line breaks and stray tags — which, rendered via textContent, would show
// as ugly literal "<br>" markup. Convert <br> to newlines and strip any other
// tags so the card text reads cleanly.
func cleanText(s string) string {
	if s == "" || !strings.Contains(s, "<") {
		return s
	}
	s = brRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, "")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// isWideImage is the server's INITIAL hint for the banner-vs-thumbnail layout;
// the client makes the final call from the real loaded image's aspect ratio (the
// only fully reliable signal — see linkPreview.js). We deliberately do NOT trust
// `twitter:card = summary_large_image` alone: GitHub sets it even on org/user
// profile pages whose og:image is a square avatar, which would stretch a logo
// into a banner. So the hint is wide only when there's positive evidence of a
// landscape image:
//   - explicit og:image:width that is large and clearly landscape, or
//   - a known wide social-card host (GitHub's opengraph/repository-images),
//
// and explicitly NOT wide for known square-avatar hosts.
func isWideImage(meta map[string]string, imageURL string) bool {
	host := strings.ToLower(hostLabel(imageURL))
	switch {
	case strings.Contains(host, "avatars.githubusercontent.com"):
		return false // square avatar (org/user profile, gravatar) — a logo
	case strings.Contains(host, "opengraph.githubassets.com"),
		strings.Contains(host, "repository-images.githubusercontent.com"):
		return true // GitHub repo social card, always 1280×640
	}
	w := atoiSafe(meta["og:image:width"])
	h := atoiSafe(meta["og:image:height"])
	if w >= 600 && h > 0 && w*10 >= h*13 { // large AND width ≥ 1.3× height
		return true
	}
	return false
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// jinaResponse is the shape r.jina.ai returns with Accept: application/json.
type jinaResponse struct {
	Data struct {
		Title       string            `json:"title"`
		Description string            `json:"description"`
		URL         string            `json:"url"`
		Metadata    map[string]string `json:"metadata"`
	} `json:"data"`
}

// fetchJina asks the Jina reader to fetch+render rawURL and hands back the
// title/description/og:image it extracted — the escape hatch for pages a direct
// crawl can't reach (anti-bot interstitials, JS-only metadata).
func (f *fetcher) fetchJina(ctx context.Context, rawURL string) (*Preview, error) {
	body, err := f.get(ctx, "https://r.jina.ai/"+rawURL, "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var jr jinaResponse
	if err := json.NewDecoder(io.LimitReader(body, maxBodyBytes)).Decode(&jr); err != nil {
		return nil, err
	}
	md := jr.Data.Metadata
	image := absImage(firstNonEmpty(md["og:image"], md["og:image:url"], md["twitter:image"]), rawURL)
	p := &Preview{
		URL:         rawURL,
		Title:       cleanText(firstNonEmpty(jr.Data.Title, md["og:title"], md["twitter:title"])),
		Description: cleanText(firstNonEmpty(jr.Data.Description, md["og:description"], md["twitter:description"])),
		ImageURL:    image,
		SiteName:    firstNonEmpty(md["og:site_name"], hostLabel(rawURL)),
		ImageWide:   isWideImage(md, image),
		VideoURL:    directVideo(md, rawURL),
	}
	return p, nil
}

// get issues a single GET with the spoofed uTLS fingerprint and the crawler UA,
// returning the response body for the caller to read+close. Non-2xx responses
// are surfaced as an error (with the body already drained+closed) so a fetcher
// never parses a 403 interstitial as if it were the page.
func (f *fetcher) get(ctx context.Context, target, accept string) (io.ReadCloser, error) {
	req, err := fhttp.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", crawlerUA)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	transport.ApplyHeaderOrder(req)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &statusError{code: resp.StatusCode}
	}
	return resp.Body, nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "unfurl: upstream status " + itoa(e.code) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// parseMeta tokenizes an HTML head and collects the OG/Twitter/description meta
// tags plus the document <title> (under the synthetic key "__title__"). Keys are
// lowercased; the first occurrence of each wins. Tokenizing (not full-tree
// parsing) keeps it allocation-light and immune to malformed markup.
func parseMeta(r io.Reader) map[string]string {
	out := make(map[string]string, 8)
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			switch tag {
			case "meta":
				var key, content string
				var ok bool
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch strings.ToLower(string(k)) {
					case "property", "name":
						key = strings.ToLower(string(v))
						ok = true
					case "content":
						content = string(v)
					}
				}
				if ok && content != "" {
					if _, seen := out[key]; !seen {
						out[key] = strings.TrimSpace(html.UnescapeString(content))
					}
				}
			case "title":
				if _, seen := out["__title__"]; !seen {
					if tt2 := z.Next(); tt2 == html.TextToken {
						out["__title__"] = strings.TrimSpace(html.UnescapeString(string(z.Text())))
					}
				}
			case "body":
				// OG/Twitter tags live in <head>; once <body> opens there is
				// nothing left worth scanning. Stop early.
				return out
			}
		}
	}
}

// absImage resolves a possibly-relative og:image against the page URL so the
// card's <img> always gets an absolute URL the proxy can fetch.
func absImage(img, pageURL string) string {
	img = strings.TrimSpace(img)
	if img == "" {
		return ""
	}
	if strings.HasPrefix(img, "http://") || strings.HasPrefix(img, "https://") {
		return img
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(img)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// hostLabel returns the bare registrable-ish host ("stackoverflow.com") used as
// the card's site name when the page exposes no og:site_name.
func hostLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
