package reddit

import (
	"html/template"
	"strings"
	"testing"
)

func TestFormatURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Special values → empty
		{"empty", "", ""},
		{"self", "self", ""},
		{"default", "default", ""},
		{"nsfw", "nsfw", ""},
		{"spoiler", "spoiler", ""},

		// Reddit domains → local path
		{"www.reddit.com", "https://www.reddit.com/r/golang/comments/abc", "/r/golang/comments/abc"},
		{"old.reddit.com", "https://old.reddit.com/r/test", "/r/test"},
		{"np.reddit.com", "https://np.reddit.com/r/news/comments/xyz", "/r/news/comments/xyz"},

		// i.redd.it → /img/
		{"i.redd.it", "https://i.redd.it/abc123.jpg", "/img/abc123.jpg"},
		{"i.redd.it with query", "https://i.redd.it/photo.png?s=abc", "/img/photo.png?s=abc"},

		// preview.redd.it → /preview/pre/
		{"preview.redd.it", "https://preview.redd.it/img123.jpg?width=640", "/preview/pre/img123.jpg?width=640"},

		// external-preview.redd.it → /preview/external-pre/
		{"external-preview", "https://external-preview.redd.it/img.jpg", "/preview/external-pre/img.jpg"},

		// thumbs
		{"a.thumbs", "https://a.thumbs.redditmedia.com/thumb.jpg", "/thumb/a/thumb.jpg"},
		{"b.thumbs", "https://b.thumbs.redditmedia.com/thumb.jpg", "/thumb/b/thumb.jpg"},

		// emoji.redditmedia.com
		{"emoji", "https://emoji.redditmedia.com/sub123/emoji.png", "/emoji/sub123/emoji.png"},

		// styles.redditmedia.com
		{"styles", "https://styles.redditmedia.com/t5_abc/styles/banner.jpg", "/style/t5_abc/styles/banner.jpg"},

		// redditstatic.com
		{"redditstatic", "https://www.redditstatic.com/icon.png", "/static/icon.png"},

		// Non-reddit URL → unchanged
		{"external", "https://example.com/image.jpg", "https://example.com/image.jpg"},
		{"youtube", "https://www.youtube.com/watch?v=abc", "https://www.youtube.com/watch?v=abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatURL(tt.input)
			if got != tt.want {
				t.Errorf("FormatURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCanonicalKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// preview.redd.it — width + signature stripped, two variants collapse
		{
			"preview width 640",
			"https://preview.redd.it/abc.jpg?width=640&s=signature1",
			"https://preview.redd.it/abc.jpg",
		},
		{
			"preview width 320",
			"https://preview.redd.it/abc.jpg?width=320&s=signature2",
			"https://preview.redd.it/abc.jpg",
		},
		{
			"preview re-signed",
			"https://preview.redd.it/abc.jpg?width=640&s=NEWSIG",
			"https://preview.redd.it/abc.jpg",
		},
		// external-preview behaves the same
		{
			"external-preview",
			"https://external-preview.redd.it/xyz.png?width=108&crop=smart&auto=webp&s=abc",
			"https://external-preview.redd.it/xyz.png",
		},
		// i.redd.it — already canonical, query (if any) dropped
		{
			"i.redd.it no query",
			"https://i.redd.it/photo.jpg",
			"https://i.redd.it/photo.jpg",
		},
		{
			"i.redd.it with query",
			"https://i.redd.it/photo.jpg?s=abc",
			"https://i.redd.it/photo.jpg",
		},
		// v.redd.it DASH — drop ?source=fallback
		{
			"v.redd.it dash",
			"https://v.redd.it/abc123/DASH_720.mp4?source=fallback",
			"https://v.redd.it/abc123/DASH_720.mp4",
		},
		// host case insensitivity
		{
			"mixed case host",
			"https://Preview.Redd.IT/abc.jpg?width=640",
			"https://preview.redd.it/abc.jpg",
		},
		// muxed: prefix preserved, inner canonicalized
		{
			"muxed prefix",
			"muxed:https://v.redd.it/abc123/DASH_720.mp4?source=fallback",
			"muxed:https://v.redd.it/abc123/DASH_720.mp4",
		},
		// thumbs
		{
			"a.thumbs",
			"https://a.thumbs.redditmedia.com/thumb.jpg?s=x",
			"https://a.thumbs.redditmedia.com/thumb.jpg",
		},
		// external host — same path-only rule
		{
			"imgur",
			"https://i.imgur.com/abc.jpg?1",
			"https://i.imgur.com/abc.jpg",
		},
		// Empty / malformed — input returned unchanged
		{
			"empty",
			"",
			"",
		},
		{
			"no scheme no host",
			"just-a-string",
			"just-a-string",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanonicalKey(tt.input)
			if got != tt.want {
				t.Errorf("CanonicalKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCanonicalKey_VariantsCollapse(t *testing.T) {
	// The dedup property: any two URLs that should refer to the same logical
	// asset must produce the same canonical key.
	a := CanonicalKey("https://preview.redd.it/img.jpg?width=640&s=A")
	b := CanonicalKey("https://preview.redd.it/img.jpg?width=320&s=B")
	c := CanonicalKey("https://preview.redd.it/img.jpg?width=108&crop=smart&auto=webp&s=C")
	if a != b || b != c {
		t.Errorf("three preview variants should canonicalize to one key, got %q / %q / %q", a, b, c)
	}
}

func TestRewriteURLs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(string) bool
		desc  string
	}{
		{
			"rewrite reddit link href",
			`<a href="https://www.reddit.com/r/golang">link</a>`,
			func(s string) bool { return strings.Contains(s, `href="/r/golang"`) },
			"should rewrite to local path",
		},
		{
			"rewrite old.reddit href",
			`<a href="https://old.reddit.com/r/test/comments/abc">link</a>`,
			func(s string) bool { return strings.Contains(s, `href="/`) },
			"should rewrite old.reddit.com",
		},
		{
			"rewrite inline media URL",
			`<img src="https://preview.redd.it/img.jpg?width=640">`,
			func(s string) bool { return strings.Contains(s, "/preview/pre/") },
			"should rewrite preview.redd.it inline",
		},
		{
			"no change to external URL",
			`<a href="https://example.com/page">ext</a>`,
			func(s string) bool { return strings.Contains(s, "https://example.com/page") },
			"should leave external URLs alone",
		},
		{
			"clean escaped underscores",
			`hello\_world`,
			func(s string) bool { return strings.Contains(s, "hello_world") },
			"should clean escaped underscores",
		},
		{
			"empty string",
			"",
			func(s string) bool { return s == "" },
			"empty input should return empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteURLs(tt.input)
			if !tt.check(got) {
				t.Errorf("RewriteURLs: %s\ninput:  %q\noutput: %q", tt.desc, tt.input, got)
			}
		})
	}
}

func TestRewriteEmotes(t *testing.T) {
	metadata := map[string]interface{}{
		"emote1": map[string]interface{}{
			"id": "emote|sub123|smile",
			"s": map[string]interface{}{
				"u": "https://preview.redd.it/emote.png",
				"x": float64(20),
				"y": float64(20),
			},
		},
	}
	body := "Hello :smile: world"
	result := RewriteEmotes(metadata, body)
	if !strings.Contains(result, `<img `) || !strings.Contains(result, `src="/preview/pre/emote.png"`) {
		t.Errorf("RewriteEmotes should replace emote text with img tag, got: %q", result)
	}
	if !strings.Contains(result, `loading="lazy"`) {
		t.Errorf("RewriteEmotes should mark emote img loading=lazy, got: %q", result)
	}
	if strings.Contains(result, ":smile:") {
		t.Errorf("RewriteEmotes should replace :smile:, got: %q", result)
	}
}

func TestEmbedCommentImages(t *testing.T) {
	body := `<p><a href="/preview/pre/abc.jpeg?width=720&amp;s=xyz">/preview/pre/abc.jpeg?width=720&amp;s=xyz</a></p>`
	got := EmbedCommentImages(body)
	if !strings.Contains(got, "<img") || !strings.Contains(got, `src="/preview/pre/abc.jpeg?width=720&amp;s=xyz"`) {
		t.Errorf("auto-linked image should embed <img>, got: %q", got)
	}
}

func TestEmbedCommentImages_NonAutoLink(t *testing.T) {
	// Text differs from href — user-written label, leave alone.
	body := `<a href="/preview/pre/abc.jpeg?s=1">click here</a>`
	got := EmbedCommentImages(body)
	if got != body {
		t.Errorf("user-labeled anchor should be untouched, got: %q", got)
	}
}

func TestEmbedCommentImages_IRedditImg(t *testing.T) {
	body := `<a href="/img/foo.png">/img/foo.png</a>`
	got := EmbedCommentImages(body)
	if !strings.Contains(got, `<img loading="lazy"`) {
		t.Errorf("i.redd.it img auto-link should be embedded, got: %q", got)
	}
}

func TestExtractBodyImageURLs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "empty body",
			body: "",
			want: nil,
		},
		{
			name: "no images",
			body: `<p>just some <a href="/r/golang">text</a></p>`,
			want: nil,
		},
		{
			// The reported "Remove Turnkey footer" case: a signed preview link
			// pasted into a self post's selftext. The rewritten body still
			// carries &amp; entities in the query; we must un-escape them so the
			// reconstructed CDN URL matches what the proxy actually fetches.
			name: "preview footer image with entity-encoded query",
			body: `<p>footer:</p>` +
				`<a href="/preview/pre/ahmk357bs38h1.png?width=370&amp;format=png&amp;auto=webp&amp;s=ea1035f1">link</a>`,
			want: []string{"https://preview.redd.it/ahmk357bs38h1.png?width=370&format=png&auto=webp&s=ea1035f1"},
		},
		{
			name: "img src and external-preview",
			body: `<img src="/img/foo.png"> and ` +
				`<a href="/preview/external-pre/bar.jpg?s=z">x</a>`,
			want: []string{
				"https://i.redd.it/foo.png",
				"https://external-preview.redd.it/bar.jpg?s=z",
			},
		},
		{
			// Same asset under two width variants collapses on the canonical
			// key, but ExtractBodyImageURLs itself only de-dups exact raw URLs;
			// distinct queries are distinct raws (both worth fetching — the
			// store's variant-upgrade rule keeps the bigger). Exact dupes drop.
			name: "exact duplicate dropped",
			body: `<a href="/img/dup.png">a</a><a href="/img/dup.png">b</a>`,
			want: []string{"https://i.redd.it/dup.png"},
		},
		{
			// A non-image proxy link (a rewritten reddit.com permalink) must not
			// be harvested as cacheable media.
			name: "non-image local link ignored",
			body: `<a href="/r/sub/comments/abc/title">perma</a>`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractBodyImageURLs(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("ExtractBodyImageURLs() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ExtractBodyImageURLs()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEmbedBodyImages_RetroAndIdempotent(t *testing.T) {
	// Old archived self-post Body: a bare /preview/pre auto-link (text==href).
	old := `<p>footer:</p><a href="/preview/pre/ahmk357bs38h1.png?width=370&amp;s=z">/preview/pre/ahmk357bs38h1.png?width=370&amp;s=z</a>`
	embedded := string(EmbedBodyImages(template.HTML(old)))
	if !strings.Contains(embedded, "<img") || !strings.Contains(embedded, `class="comment_image"`) {
		t.Fatalf("archived auto-link should embed <img>, got: %q", embedded)
	}
	// Idempotent: running it again must not double-wrap or change the output.
	again := string(EmbedBodyImages(template.HTML(embedded)))
	if again != embedded {
		t.Errorf("EmbedBodyImages not idempotent:\n first: %q\nsecond: %q", embedded, again)
	}
}

func TestRewriteEmotes_Empty(t *testing.T) {
	result := RewriteEmotes(nil, "no emotes here")
	if result != "no emotes here" {
		t.Errorf("RewriteEmotes with nil metadata should return body unchanged, got: %q", result)
	}
}

func TestVideoQualityURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		quality string
		want    string
	}{
		// "source" / empty / unknown quality → unchanged
		{"source keeps original", "/vid/abc/DASH_720.mp4", "source", "/vid/abc/DASH_720.mp4"},
		{"empty quality keeps original", "/vid/abc/DASH_720.mp4", "", "/vid/abc/DASH_720.mp4"},
		{"unknown quality keeps original", "/vid/abc/DASH_720.mp4", "999", "/vid/abc/DASH_720.mp4"},

		// Downward clamp rewrites the height in place
		{"720 to 480", "/vid/abc/DASH_720.mp4", "480", "/vid/abc/DASH_480.mp4"},
		{"1080 to 240", "/vid/abc/DASH_1080.mp4", "240", "/vid/abc/DASH_240.mp4"},
		{"preserves fallback query", "/vid/abc/DASH_720.mp4?source=fallback", "360", "/vid/abc/DASH_360.mp4?source=fallback"},
		{"CMAF rendition", "/vid/abc/CMAF_1080.mp4", "720", "/vid/abc/CMAF_720.mp4"},

		// Never upscale beyond the source (fallback) rendition
		{"want above source unchanged", "/vid/abc/DASH_480.mp4", "1080", "/vid/abc/DASH_480.mp4"},
		{"want equals source unchanged", "/vid/abc/DASH_720.mp4", "720", "/vid/abc/DASH_720.mp4"},

		// Non-DASH URLs untouched
		{"hls untouched", "/hls/abc/HLSPlaylist.m3u8", "480", "/hls/abc/HLSPlaylist.m3u8"},
		{"image untouched", "/img/abc.jpg", "480", "/img/abc.jpg"},
		{"empty url", "", "480", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VideoQualityURL(tt.url, tt.quality); got != tt.want {
				t.Errorf("VideoQualityURL(%q, %q) = %q, want %q", tt.url, tt.quality, got, tt.want)
			}
		})
	}
}

// TestExtractCommentImageURLs pins that inline images pasted into comment
// bodies — including nested replies — are harvested for L2 caching, while
// non-image links are ignored and the same asset under different signed
// queries collapses on the canonical key.
func TestExtractCommentImageURLs(t *testing.T) {
	comments := []Comment{
		{
			Body: template.HTML(`<a href="/preview/pre/top.png?width=640&amp;s=sig1">link</a>`),
			Replies: []Comment{
				{Body: template.HTML(`<img src="/img/reply.jpg">`)},
				// Same asset as the top comment under a different signature — must
				// collapse on the canonical key, not double-queue.
				{Body: template.HTML(`<a href="/preview/pre/top.png?width=320&amp;s=sig2">again</a>`)},
				// A plain reddit permalink must not be harvested as media.
				{Body: template.HTML(`<a href="/r/sub/comments/x/title">perma</a>`)},
			},
		},
		{Body: template.HTML(`<p>no media here</p>`)},
	}

	got := ExtractCommentImageURLs(comments)
	want := []string{
		"https://preview.redd.it/top.png?width=640&s=sig1",
		"https://i.redd.it/reply.jpg",
	}
	if len(got) != len(want) {
		t.Fatalf("ExtractCommentImageURLs() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ExtractCommentImageURLs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if got := ExtractCommentImageURLs(nil); got != nil {
		t.Errorf("ExtractCommentImageURLs(nil) = %v, want nil", got)
	}
}
