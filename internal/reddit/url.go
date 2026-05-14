package reddit

import (
	"fmt"
	"net/url"
	"regexp"
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
)

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

		replacement := fmt.Sprintf(`<img src="%s" width="%d" height="%d" class="emote">`,
			localURL, width, height)
		body = strings.ReplaceAll(body, emoteName, replacement)
	}

	return body
}
