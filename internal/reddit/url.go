package reddit

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var specialValues = map[string]bool{
	"":        true,
	"self":    true,
	"default": true,
	"nsfw":    true,
	"spoiler": true,
}

// FormatURL rewrites Reddit media/content URLs to local proxy paths.
// Special values ("", "self", "default", "nsfw", "spoiler") return empty string.
func FormatURL(rawURL string) string {
	if specialValues[rawURL] {
		return ""
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	host := strings.ToLower(u.Host)
	pathAndQuery := u.Path
	if u.RawQuery != "" {
		pathAndQuery += "?" + u.RawQuery
	}
	pathAndQuery = strings.TrimPrefix(pathAndQuery, "/")

	switch host {
	case "www.reddit.com", "old.reddit.com", "np.reddit.com", "reddit.com",
		"new.reddit.com", "amp.reddit.com":
		return "/" + pathAndQuery

	case "v.redd.it":
		if m := vRedditHLS.FindStringSubmatch(rawURL); m != nil {
			return "/hls/" + m[1] + "/" + m[2]
		}
		if m := vRedditDASH.FindStringSubmatch(rawURL); m != nil {
			return "/vid/" + m[1] + "/" + m[2]
		}
		return "/vid/" + pathAndQuery

	case "i.redd.it":
		return "/img/" + pathAndQuery

	case "a.thumbs.redditmedia.com":
		return "/thumb/a/" + pathAndQuery

	case "b.thumbs.redditmedia.com":
		return "/thumb/b/" + pathAndQuery

	case "emoji.redditmedia.com":
		parts := strings.SplitN(pathAndQuery, "/", 2)
		if len(parts) == 2 {
			return "/emoji/" + parts[0] + "/" + parts[1]
		}
		return "/emoji/" + pathAndQuery

	case "preview.redd.it":
		return "/preview/pre/" + pathAndQuery

	case "external-preview.redd.it":
		return "/preview/external-pre/" + pathAndQuery

	case "styles.redditmedia.com":
		return "/style/" + pathAndQuery

	case "www.redditstatic.com":
		return "/static/" + pathAndQuery

	case "reddit-econ-prod-assets-permanent.s3.amazonaws.com":
		// Emote assets: /asset-manager/{subreddit_id}/{filename}
		if strings.HasPrefix(u.Path, "/asset-manager/") {
			trimmed := strings.TrimPrefix(u.Path, "/asset-manager/")
			return "/emote/" + trimmed
		}
		return rawURL

	default:
		return rawURL
	}
}

var (
	vRedditDASH = regexp.MustCompile(`https?://v\.redd\.it/([^/]+)/((?:DASH|CMAF)_\d{2,4}(?:\.mp4|$|\?source=fallback).*)`)
	vRedditHLS  = regexp.MustCompile(`https?://v\.redd\.it/([^/]+)/(HLSPlaylist\.m3u8.*)$`)

	// dashHeightRe captures the height in a v.redd.it rendition filename, e.g.
	// the "720" in "/vid/abc/DASH_720.mp4?source=fallback".
	dashHeightRe = regexp.MustCompile(`((?:DASH|CMAF)_)(\d{2,4})(\.mp4)`)
)

// VideoQualityHeights maps the user-facing Video quality option values to the
// v.redd.it rendition height they request. These are the standard heights
// Reddit transcodes to; the option list in settings is built from these keys.
var VideoQualityHeights = map[string]int{
	"1080": 1080,
	"720":  720,
	"480":  480,
	"360":  360,
	"240":  240,
}

// VideoQualityURL clamps a v.redd.it DASH fallback URL to a preferred maximum
// rendition height.
//
// Reddit exposes NO quality query parameter: each rendition is a distinct
// DASH_<height>.mp4 file, and the API's fallback_url is the TOP rendition the
// source was transcoded to. So selecting a quality means rewriting the height
// in the path — and we only ever rewrite DOWNWARD, because requesting a height
// above the source's top rendition would 404. quality is one of the
// VideoQualityHeights keys; "", "source", or any non-DASH/HLS URL is returned
// unchanged (the original/best quality).
func VideoQualityURL(localURL, quality string) string {
	want, ok := VideoQualityHeights[quality]
	if !ok || localURL == "" {
		return localURL
	}
	loc := dashHeightRe.FindStringSubmatchIndex(localURL)
	if loc == nil {
		return localURL
	}
	// loc[4]:loc[5] is the digits capture group.
	orig, err := strconv.Atoi(localURL[loc[4]:loc[5]])
	if err != nil || orig <= 0 || want >= orig {
		return localURL
	}
	return localURL[:loc[4]] + strconv.Itoa(want) + localURL[loc[5]:]
}

// muxedKeyPrefix is duplicated from media/mux.go to avoid an import cycle.
// CanonicalKey strips and re-applies it so the wrapped v.redd.it URL is
// canonicalized exactly like a bare one.
const muxedKeyPrefix = "muxed:"

// CanonicalKey produces a stable dedup key for a media URL by stripping the
// query string (Reddit's variant/signature params) and lowercasing the host.
// Reddit's `preview.redd.it` URLs encode resolution in `?width=` and a rotating
// signature in `?s=` — the bytes for one image arrive under N distinct URLs
// otherwise. Bare path is also the right key for i.redd.it / v.redd.it /
// thumbs / styles / static / emoji / S3 assets — the path already identifies
// the asset and any query is either absent or noise (e.g. `?source=fallback`).
// External hosts get the same path-only treatment; if that ever drops a
// meaningful query we can special-case it later.
//
// The HTTP fetch must still use the raw URL (Reddit verifies `s=`); this key
// is only for the dedup index. Returns the input unchanged when url.Parse
// fails, so a malformed URL is its own key (worst case = no dedup).
func CanonicalKey(rawURL string) string {
	inner, prefix := rawURL, ""
	if strings.HasPrefix(rawURL, muxedKeyPrefix) {
		prefix = muxedKeyPrefix
		inner = rawURL[len(muxedKeyPrefix):]
	}

	u, err := url.Parse(inner)
	if err != nil || u.Host == "" {
		return rawURL
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	host := strings.ToLower(u.Host)
	return prefix + scheme + "://" + host + u.Path
}

// UnformatURL reverses FormatURL: converts a local proxy path back to the
// original CDN URL. Returns the input unchanged if it is already absolute or
// does not match a known prefix.
func UnformatURL(localPath string) string {
	if localPath == "" {
		return ""
	}
	if strings.HasPrefix(localPath, "http://") || strings.HasPrefix(localPath, "https://") {
		return localPath
	}

	prefixes := []struct {
		local  string
		remote string
	}{
		{"/img/", "https://i.redd.it/"},
		{"/preview/external-pre/", "https://external-preview.redd.it/"},
		{"/preview/pre/", "https://preview.redd.it/"},
		{"/thumb/a/", "https://a.thumbs.redditmedia.com/"},
		{"/thumb/b/", "https://b.thumbs.redditmedia.com/"},
		{"/vid/", "https://v.redd.it/"},
		{"/hls/", "https://v.redd.it/"},
		{"/emoji/", "https://reddit-econ-prod-assets-permanent.s3.amazonaws.com/asset-manager/"},
		{"/static/", "https://www.redditstatic.com/"},
		{"/style/", "https://styles.redditmedia.com/"},
	}

	for _, p := range prefixes {
		if strings.HasPrefix(localPath, p.local) {
			return p.remote + strings.TrimPrefix(localPath, p.local)
		}
	}

	return localPath
}

// redditLinkRe matches href/src attributes pointing to reddit domains.
var redditLinkRe = regexp.MustCompile(`(href|src)="https?://(?:www\.|old\.|np\.|amp\.|new\.)?(?:reddit\.com|redd\.it)/`)

// mediaInlineRe matches inline reddit media URLs in HTML that should be rewritten.
var mediaInlineRe = regexp.MustCompile(`https?://(?:preview\.redd\.it|external-preview\.redd\.it|i\.redd\.it)/[^\s"<>]+`)

// redditStaticRe matches redditstatic.com URLs.
var redditStaticRe = regexp.MustCompile(`https?://www\.redditstatic\.com/[^\s"<>]+`)

// stylesRe matches styles.redditmedia.com URLs.
var stylesRe = regexp.MustCompile(`https?://styles\.redditmedia\.com/[^\s"<>]+`)

// RewriteURLs rewrites all Reddit URLs in HTML content to local proxy paths.
func RewriteURLs(html string) string {
	// Rewrite href/src reddit links to local paths
	s := redditLinkRe.ReplaceAllStringFunc(html, func(match string) string {
		attr := "href"
		if strings.HasPrefix(match, "src") {
			attr = "src"
		}
		return attr + `="/`
	})

	// Rewrite redditstatic.com URLs
	s = redditStaticRe.ReplaceAllStringFunc(s, func(match string) string {
		return FormatURL(match)
	})

	// Rewrite styles.redditmedia.com URLs
	s = stylesRe.ReplaceAllStringFunc(s, func(match string) string {
		return FormatURL(match)
	})

	// Clean up escaped underscores
	s = strings.ReplaceAll(s, `%5C`, "")
	s = strings.ReplaceAll(s, `\_`, "_")

	// Rewrite inline media URLs (preview.redd.it, external-preview.redd.it, i.redd.it)
	s = mediaInlineRe.ReplaceAllStringFunc(s, func(match string) string {
		return FormatURL(match)
	})

	return s
}

// commentImageAutolinkRe matches markdown auto-links to image proxy paths
// (text == href), the canonical form Reddit emits for an image pasted or
// uploaded into a comment body. Must run AFTER RewriteURLs so the hrefs are
// already local. Backreferences aren't available in RE2; the text-equals-href
// check is enforced in EmbedCommentImages.
var commentImageAutolinkRe = regexp.MustCompile(`<a href="(/(?:preview/(?:pre|external-pre)|img)/[^"]+)">([^<]+)</a>`)

// EmbedCommentImages turns comment-body auto-linked image URLs into inline
// <img> tags, so a comment whose entire body is one preview.redd.it link
// actually renders the image. Anchors whose visible text differs from the
// href are left alone — those are user-written labels, not auto-links.
func EmbedCommentImages(body string) string {
	return commentImageAutolinkRe.ReplaceAllStringFunc(body, func(match string) string {
		m := commentImageAutolinkRe.FindStringSubmatch(match)
		if len(m) != 3 || m[1] != m[2] {
			return match
		}
		return fmt.Sprintf(`<a href="%s" target="_blank" rel="nofollow noopener" class="comment_image"><img loading="lazy" alt="Comment image" src="%s"></a>`, m[1], m[1])
	})
}

// bodyImageRe matches local image proxy paths that RewriteURLs has already
// inlined into rendered post/comment body HTML — an image pasted into a self
// post's selftext (or a comment) surfaces as an href/src pointing at /img/,
// /preview/pre/, or /preview/external-pre/. These inline body images are NOT
// represented in a post's structured Media/Gallery/Thumbnail fields, so the
// archive layer never learns about them from ExtractMediaItems. Harvesting them
// here lets the cache fetch the bytes while Reddit's signed preview URL is still
// fresh; without it a self post's footer/inline image 403s the moment the `s=`
// signature expires (preview.redd.it URLs are short-lived) and the viewer sees
// the "Sorry, we missed it" placeholder for an asset we could have kept.
var bodyImageRe = regexp.MustCompile(`(?:href|src)="(/(?:img|preview/pre|preview/external-pre)/[^"]+)"`)

// ExtractBodyImageURLs returns the original Reddit CDN URLs for every inline
// image embedded in rendered body HTML (post selftext or comment body). Each
// match is HTML-unescaped (the rewritten body still carries &amp; entities in
// the signed query) and run back through UnformatURL so the result matches the
// raw URL the media proxy fetches and keys the cache on. Duplicates and any
// path that doesn't map to a known CDN prefix are dropped.
func ExtractBodyImageURLs(bodyHTML string) []string {
	if bodyHTML == "" {
		return nil
	}
	ms := bodyImageRe.FindAllStringSubmatch(bodyHTML, -1)
	if len(ms) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ms))
	var urls []string
	for _, m := range ms {
		raw := UnformatURL(html.UnescapeString(m[1]))
		// UnformatURL returns the input unchanged for an unknown prefix; the
		// regex already pins us to /img|/preview, so a still-local path here
		// means a malformed match — skip it rather than feed the proxy a
		// non-URL it would reject as a disallowed host.
		if raw == "" || strings.HasPrefix(raw, "/") || seen[raw] {
			continue
		}
		seen[raw] = true
		urls = append(urls, raw)
	}
	return urls
}

// EmbedBodyImages re-applies inline-image embedding to an already-rendered post
// Body. It exists for the archive render path: posts archived before
// EmbedCommentImages was wired into ParsePost have a Body whose inline images
// are still bare /preview/pre/... auto-links, and the stored Body is served
// verbatim (ParsePost does not re-run on view). Calling this at render time
// retroactively turns those auto-links into <img> without re-archiving.
// Idempotent — EmbedCommentImages only matches text==href auto-links, so a Body
// already embedded (or freshly parsed) passes through unchanged.
func EmbedBodyImages(body template.HTML) template.HTML {
	return template.HTML(EmbedCommentImages(string(body)))
}

// RewriteEmotes rewrites emote references in comment body HTML using media_metadata.
// mediaMetadata is the raw JSON media_metadata object from Reddit.
// body is the body_html content.
func RewriteEmotes(mediaMetadata map[string]interface{}, body string) string {
	if len(mediaMetadata) == 0 {
		return body
	}

	for _, v := range mediaMetadata {
		meta, ok := v.(map[string]interface{})
		if !ok {
			continue
		}

		id, _ := meta["id"].(string)
		if id == "" {
			continue
		}

		// Emote IDs have format "emote|SUBREDDIT_ID|NUMBER"
		parts := strings.Split(id, "|")
		if len(parts) != 3 || parts[0] != "emote" {
			continue
		}
		emoteName := ":" + parts[2] + ":"

		// Get image URL from s.u
		s, ok := meta["s"].(map[string]interface{})
		if !ok {
			continue
		}
		imgURL, _ := s["u"].(string)
		if imgURL == "" {
			continue
		}

		localURL := FormatURL(imgURL)

		// Get dimensions
		width := 20
		height := 20
		if w, ok := s["x"].(float64); ok && w > 0 {
			width = int(w)
			if width > 40 {
				width = 40
			}
		}
		if h, ok := s["y"].(float64); ok && h > 0 {
			height = int(h)
			if height > 40 {
				height = 40
			}
		}

		replacement := fmt.Sprintf(`<img loading="lazy" src="%s" width="%d" height="%d" class="emote">`,
			localURL, width, height)
		body = strings.ReplaceAll(body, emoteName, replacement)
	}

	return body
}
