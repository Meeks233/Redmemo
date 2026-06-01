package media

import (
	"net/url"
	"path"
	"strings"
	"unicode"
)

// maxTitleRunes caps the sanitized title to keep the resulting Content-Disposition
// filename well below filesystem and HTTP header limits, and to avoid runaway
// titles producing pathological names. The cap is in runes, not bytes, so
// CJK-heavy titles (common in this codebase's user base) get a sensible length
// rather than a one-or-two-char truncation a byte cap would force.
const maxTitleRunes = 60

// BuildDownloadFilename builds a "title_uid.ext" filename for the
// Content-Disposition header on a video/gif response. Spaces in the title
// become underscores, characters unsafe for filenames are dropped, the title
// is capped at maxTitleRunes runes, the uid is derived from the original media
// URL (so it stays unique per resource), and the extension comes from the
// served MIME type. Falls back to a safe default when every component is empty.
func BuildDownloadFilename(title, originalURL, mime string) string {
	safeTitle := sanitizeTitle(title)
	uid := uidFromURL(originalURL)
	ext := extFromMIME(mime)

	var name string
	switch {
	case safeTitle != "" && uid != "":
		name = safeTitle + "_" + uid
	case safeTitle != "":
		name = safeTitle
	case uid != "":
		name = uid
	default:
		name = "media"
	}
	return name + ext
}

func sanitizeTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		switch {
		case unicode.IsSpace(r):
			b.WriteByte('_')
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	out := collapseUnderscores(b.String())
	runes := []rune(out)
	if len(runes) > maxTitleRunes {
		runes = runes[:maxTitleRunes]
	}
	return strings.Trim(string(runes), "_-")
}

func collapseUnderscores(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, r := range s {
		if r == '_' {
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
		} else {
			prevUnderscore = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// uidFromURL derives a stable per-resource identifier from a Reddit media URL.
// For v.redd.it (/<id>/DASH_720.mp4) it returns "<id>_DASH_720" so two
// renditions of the same source don't collide; for i.redd.it/etc. it returns
// the base name without extension. The result is filesystem-safe.
func uidFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		raw = u.Path
	}
	raw = strings.TrimSuffix(raw, "/")
	base := path.Base(raw)
	base = strings.TrimSuffix(base, path.Ext(base))
	dir := path.Base(path.Dir(raw))

	var uid string
	if dir != "" && dir != "." && dir != "/" && looksLikeRedditID(dir) {
		uid = dir + "_" + base
	} else {
		uid = base
	}
	return sanitizeUID(uid)
}

// looksLikeRedditID accepts the alphanumeric (and a few separator) shapes
// Reddit uses for video / image identifiers: lowercase letters, digits,
// underscores, dashes. Path components that look like file names (carry a
// trailing extension that survived the earlier strip pass) are not folded in.
func looksLikeRedditID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func sanitizeUID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "_-")
}

func extFromMIME(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "image/gif":
		return ".gif"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// WantsDownloadName reports whether a response with the given MIME type should
// carry a friendly title-based filename. Limited to video and animated-image
// content per the product spec — still images keep the bare proxy URL.
func WantsDownloadName(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return strings.HasPrefix(m, "video/") || m == "image/gif"
}

// EncodeContentDisposition formats an RFC 5987 inline Content-Disposition value
// for filename. UTF-8 is encoded via filename*=UTF-8'' so non-ASCII titles
// (CJK, accents) survive the trip to the browser intact.
func EncodeContentDisposition(filename string) string {
	return `inline; filename*=UTF-8''` + url.PathEscape(filename)
}
