package render

import (
	"context"
	"strings"
	"testing"
)

// TestReviseRedditLinks_LocalKeptWhenUpstreamAllowed: the common case — a
// parse-time-localized Reddit content link stays on-site when upstream is
// reachable, so the click lands on RedMemo's own page.
func TestReviseRedditLinks_LocalKeptWhenUpstreamAllowed(t *testing.T) {
	in := `<p>see <a href="/r/selfhosted/comments/1gugnku/weddingshare/">https://www.reddit.com/r/selfhosted/comments/1gugnku/weddingshare/</a></p>`
	got := reviseRedditLinks(in, true)
	if got != in {
		t.Fatalf("upstream-allowed local link should be untouched.\n got: %s\nwant: %s", got, in)
	}
}

// TestReviseRedditLinks_LocalRevertedWhenUpstreamDisabled: cache-only mode must
// point Reddit content links back at reddit.com (where the content actually
// lives) rather than dead-ending at a local 404.
func TestReviseRedditLinks_LocalRevertedWhenUpstreamDisabled(t *testing.T) {
	in := `<a href="/r/selfhosted/comments/1gugnku/x/">label</a>`
	got := reviseRedditLinks(in, false)
	want := `<a href="https://www.reddit.com/r/selfhosted/comments/1gugnku/x/">label</a>`
	if got != want {
		t.Fatalf("upstream-disabled local link should revert to reddit.com.\n got: %s\nwant: %s", got, want)
	}
}

// TestReviseRedditLinks_UserAndSubRoutes covers the /u/ and /user/ content
// routes alongside /r/.
func TestReviseRedditLinks_UserAndSubRoutes(t *testing.T) {
	in := `<a href="/u/spez">u/spez</a> and <a href="/user/spez">/user/spez</a> in <a href="/r/golang">r/golang</a>`
	got := reviseRedditLinks(in, false)
	for _, want := range []string{
		`href="https://www.reddit.com/u/spez"`,
		`href="https://www.reddit.com/user/spez"`,
		`href="https://www.reddit.com/r/golang"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

// TestReviseRedditLinks_AbsoluteRedditLocalizedWhenAllowed: a stray absolute
// reddit.com URL that bypassed parse-time rewriting is brought on-site.
func TestReviseRedditLinks_AbsoluteRedditLocalizedWhenAllowed(t *testing.T) {
	in := `<a href="https://old.reddit.com/r/golang/comments/abc/title/">x</a>`
	got := reviseRedditLinks(in, true)
	want := `<a href="/r/golang/comments/abc/title/">x</a>`
	if got != want {
		t.Fatalf("absolute reddit link should localize when allowed.\n got: %s\nwant: %s", got, want)
	}
}

// TestReviseRedditLinks_QueryPreserved keeps a trailing query (e.g. ?context=3)
// across the local↔reddit flip.
func TestReviseRedditLinks_QueryPreserved(t *testing.T) {
	in := `<a href="/r/a/comments/b/c/?context=3">x</a>`
	got := reviseRedditLinks(in, false)
	want := `<a href="https://www.reddit.com/r/a/comments/b/c/?context=3">x</a>`
	if got != want {
		t.Fatalf("query should survive.\n got: %s\nwant: %s", got, want)
	}
}

// TestReviseRedditLinks_LeavesNonReddit ensures foreign hosts, media proxy
// paths, and RedMemo chrome routes are never rewritten — in either mode.
func TestReviseRedditLinks_LeavesNonReddit(t *testing.T) {
	cases := []string{
		`<a href="https://github.com/x/y">repo</a>`,
		`<a href="/img/abc.jpg">img</a>`,
		`<a href="/preview/pre/x.jpg">img</a>`,
		`<a href="/settings">settings</a>`,
		`<a href="/search?q=x">search</a>`,
		`<a href="https://i.redd.it/abc.jpg">media</a>`,
		`<a href="https://reddit.com.evil.test/r/x">phish</a>`,
	}
	for _, in := range cases {
		for _, allowed := range []bool{true, false} {
			if got := reviseRedditLinks(in, allowed); got != in {
				t.Fatalf("non-reddit-content link mutated (allowed=%v).\n got: %s\nwant: %s", allowed, got, in)
			}
		}
	}
}

// TestReviseRedditLinks_Idempotent: running twice yields the same result in
// both modes (allowed leaves local untouched; disabled produces an absolute
// reddit.com URL that is no longer a local content path, so it is left alone the
// second time).
func TestReviseRedditLinks_Idempotent(t *testing.T) {
	in := `<a href="/r/a/comments/b/c/">x</a>`
	for _, allowed := range []bool{true, false} {
		once := reviseRedditLinks(in, allowed)
		twice := reviseRedditLinks(once, allowed)
		if once != twice {
			t.Fatalf("not idempotent (allowed=%v).\n once: %s\ntwice: %s", allowed, once, twice)
		}
	}
}

// TestReviseRedditLinks_AmpersandEscaping verifies an escaped query in the
// attribute round-trips as valid HTML (no &amp; → & corruption).
func TestReviseRedditLinks_AmpersandEscaping(t *testing.T) {
	in := `<a href="/r/a/comments/b/c/?x=1&amp;y=2">x</a>`
	got := reviseRedditLinks(in, false)
	want := `<a href="https://www.reddit.com/r/a/comments/b/c/?x=1&amp;y=2">x</a>`
	if got != want {
		t.Fatalf("ampersand escaping broke.\n got: %s\nwant: %s", got, want)
	}
}

// TestReviseRedditLinks_MultipleAnchors rewrites every reddit link in a body
// while leaving interleaved non-reddit links alone.
func TestReviseRedditLinks_MultipleAnchors(t *testing.T) {
	in := `<a href="/r/a">a</a> <a href="https://x.test">x</a> <a href="/u/b">b</a>`
	got := reviseRedditLinks(in, false)
	if strings.Count(got, "https://www.reddit.com/") != 2 {
		t.Fatalf("expected 2 reddit rewrites, got: %s", got)
	}
	if !strings.Contains(got, `href="https://x.test"`) {
		t.Fatalf("foreign link mutated: %s", got)
	}
}

// TestReviseRedditLinks_NoAnchorsFastPath: bodies without anchors pass straight
// through.
func TestReviseRedditLinks_NoAnchorsFastPath(t *testing.T) {
	in := `<p>plain text, no links here</p>`
	if got := reviseRedditLinks(in, false); got != in {
		t.Fatalf("anchor-free body mutated: %s", got)
	}
}

// TestUpstreamAllowed_ContextRoundTrip locks the ctx helper contract embedBody
// relies on: default true, explicit values preserved.
func TestUpstreamAllowed_ContextRoundTrip(t *testing.T) {
	if !upstreamAllowed(context.Background()) {
		t.Fatal("default should be true (links localized)")
	}
	if upstreamAllowed(withUpstream(context.Background(), false)) {
		t.Fatal("false flag not honored")
	}
	if !upstreamAllowed(withUpstream(context.Background(), true)) {
		t.Fatal("true flag not honored")
	}
}
