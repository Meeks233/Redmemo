package render

import (
	"strings"
	"testing"
)

// TestFlairEmojiStyle_Allowlist verifies the url('...') wrapper can't be broken
// out of, while every value Reddit legitimately produces still renders. The
// common case is a SAME-ORIGIN proxy path (FormatURL rewrites
// emoji.redditmedia.com to /emoji/<id>/<name>), so relative paths must pass;
// absolute passthrough URLs must too. Any value carrying a url()-breakout
// character (quote, paren, backslash, whitespace, angle bracket) yields an empty
// style so the background-image declaration is dropped entirely.
func TestFlairEmojiStyle_Allowlist(t *testing.T) {
	safe := []string{
		"/emoji/t5_2qiyn/snoo-123",                                       // the real, relative form FormatURL emits
		"/emoji/t5_abc/flag_de.png",                                      // relative with extension
		"https://emoji.redditmedia.com/abc123/name",                      // absolute passthrough
		"https://example.com/a?b=c&d=e",                                  // query chars are inert inside the quoted url
		"/emote/t5_2qiyn/asset_name.png",                                 // /emote proxy path
		"https://reddit-emoji.s3.amazonaws.com/foo/bar_baz-1.png?v=2&x=y", // absolute with signature-style query
	}
	for _, u := range safe {
		got := string(flairEmojiStyle(u))
		want := "background-image:url('" + u + "');"
		if got != want {
			t.Errorf("flairEmojiStyle(%q) = %q, want %q", u, got, want)
		}
	}

	unsafe := []string{
		"",                                  // empty
		"/emoji/x'); } body{display:none}",  // single-quote + paren breakout
		"/emoji/x.png)",                     // bare ) closes url() early
		"/emoji/x.png\\29 ",                 // CSS-escaped ) via backslash (also has space)
		`/emoji/x" onload="`,                // double quote
		"/emoji/ x.png",                     // whitespace
		"https://evil/x'); background:red", // absolute breakout too
		"javascript:alert(1)",               // parens make this a breakout regardless of scheme
	}
	for _, u := range unsafe {
		if got := string(flairEmojiStyle(u)); got != "" {
			t.Errorf("flairEmojiStyle(%q) = %q, want empty (rejected)", u, got)
		}
	}
}

// TestFlairEmojiStyle_NoUnescapedBreakout is a belt-and-suspenders check that no
// accepted value can contain a raw single quote or closing paren — the two
// characters that would let an attacker escape url('...').
func TestFlairEmojiStyle_NoUnescapedBreakout(t *testing.T) {
	for _, u := range []string{
		"https://emoji.redditmedia.com/ok/name.png",
		"https://evil/x')",
		"https://evil/x)",
	} {
		out := string(flairEmojiStyle(u))
		if out == "" {
			continue // rejected — safe
		}
		// Whatever survives must keep exactly one opening url(' and one ');
		if strings.Count(out, "url('") != 1 || !strings.HasSuffix(out, "');") {
			t.Errorf("flairEmojiStyle(%q) = %q has a malformed url() wrapper", u, out)
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(out, "background-image:url('"), "');")
		if strings.ContainsAny(inner, "')") {
			t.Errorf("flairEmojiStyle(%q) leaked a breakout char in %q", u, inner)
		}
	}
}
