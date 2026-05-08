package reddit

import (
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
	if !strings.Contains(result, `<img src="`) {
		t.Errorf("RewriteEmotes should replace emote text with img tag, got: %q", result)
	}
	if strings.Contains(result, ":smile:") {
		t.Errorf("RewriteEmotes should replace :smile:, got: %q", result)
	}
}

func TestRewriteEmotes_Empty(t *testing.T) {
	result := RewriteEmotes(nil, "no emotes here")
	if result != "no emotes here" {
		t.Errorf("RewriteEmotes with nil metadata should return body unchanged, got: %q", result)
	}
}
