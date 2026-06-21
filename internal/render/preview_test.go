package render

import (
	"context"
	"html/template"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/reddit"
)

func TestMarkLazyLinks(t *testing.T) {
	link := "https://github.com/golang/go"
	body := `<p>see <a href="` + link + `">` + link + `</a></p>`

	got := markLazyLinks(context.Background(), body)
	if !strings.Contains(got, `class="link-preview-lazy"`) {
		t.Fatalf("link not marked lazy: %s", got)
	}
	if !strings.Contains(got, `data-unfurl="`+link+`"`) {
		t.Errorf("missing data-unfurl hint: %s", got)
	}
	// The original link must remain intact so no-JS / failed unfurl degrades to
	// the plain link.
	if !strings.Contains(got, `href="`+link+`"`) || !strings.Contains(got, `>`+link+`</a>`) {
		t.Errorf("original link not preserved: %s", got)
	}
}

func TestMarkLazyLinksSkipsLabelled(t *testing.T) {
	body := `<a href="https://example.com/p">click here</a>`
	if got := markLazyLinks(context.Background(), body); strings.Contains(got, "link-preview-lazy") {
		t.Errorf("labelled anchor must not be marked: %s", got)
	}
}

func TestMarkLazyLinksSkipsRedditAndImages(t *testing.T) {
	cases := []string{
		`<a href="https://www.reddit.com/r/x">https://www.reddit.com/r/x</a>`,
		`<a href="https://redd.it/abc">https://redd.it/abc</a>`,
		`<a href="https://i.imgur.com/abc.png">https://i.imgur.com/abc.png</a>`,
	}
	for _, body := range cases {
		if got := markLazyLinks(context.Background(), body); strings.Contains(got, "link-preview-lazy") {
			t.Errorf("should be skipped, got: %s", got)
		}
	}
}

func TestMarkLazyLinksMultiple(t *testing.T) {
	a := "https://a.com/1"
	b := "https://b.com/2"
	body := `<p><a href="` + a + `">` + a + `</a> and <a href="` + b + `">` + b + `</a></p>`
	got := markLazyLinks(context.Background(), body)
	if n := strings.Count(got, `class="link-preview-lazy"`); n != 2 {
		t.Fatalf("expected both links marked, got %d: %s", n, got)
	}
}

func TestUniqueExternalLinks(t *testing.T) {
	a := "https://a.com/1"
	// Same URL twice (bare) + a reddit link + a labelled anchor + an image link:
	// only the one distinct external bare link counts.
	body := `<a href="` + a + `">` + a + `</a> <a href="` + a + `/">` + a + `/</a>` +
		` <a href="https://www.reddit.com/r/x">https://www.reddit.com/r/x</a>` +
		` <a href="https://b.com/p">label</a>` +
		` <a href="https://i.imgur.com/x.png">https://i.imgur.com/x.png</a>`
	if n := len(uniqueExternalLinks(body)); n != 1 {
		t.Fatalf("expected 1 unique external link, got %d", n)
	}
	two := body + ` <a href="https://c.com/2">https://c.com/2</a>`
	if n := len(uniqueExternalLinks(two)); n != 2 {
		t.Fatalf("expected 2 unique external links, got %d", n)
	}
}

// TestEmbedBodyGatesCards pins the gate: a SHORT body cards EVERY external link
// it carries (one or many); only a LONG body stays plain text. The detail page
// (embedBody) must match the listing's embedBodyCards path, which already cards
// multi-link short bodies — it previously dropped them by demanding exactly one
// link.
func TestEmbedBodyGatesCards(t *testing.T) {
	ctx := context.Background()
	one := template.HTML(`<p>see <a href="https://github.com/golang/go">https://github.com/golang/go</a></p>`)
	if got := embedBody(ctx, one); !strings.Contains(got, "link-preview-lazy") {
		t.Errorf("short single-link body should card: %s", got)
	}

	multi := template.HTML(`<p><a href="https://a.com/1">https://a.com/1</a> and <a href="https://b.com/2">https://b.com/2</a></p>`)
	if got := embedBody(ctx, multi); strings.Count(got, "link-preview-lazy") != 2 {
		t.Errorf("short multi-link body should card both links: %s", got)
	}

	long := template.HTML(`<p><a href="https://github.com/golang/go">https://github.com/golang/go</a></p>` +
		strings.Repeat("x", previewExpandThreshold+1))
	if got := embedBody(ctx, long); strings.Contains(got, "link-preview-lazy") {
		t.Errorf("long body must stay plain: %s", got)
	}
}

// TestEmbedBodySelfHostedReboot is a regression for the real /r/selfhosted post
// (1u6pxsy): a short "show & tell" self-post that links both its own site and
// its GitHub repo. Both bare auto-links must upgrade to preview cards on the
// detail page — the two-link body used to render plain because the gate demanded
// exactly one external link.
func TestEmbedBodySelfHostedReboot(t *testing.T) {
	ctx := context.Background()
	body := template.HTML(`<!-- SC_OFF --><div class="md">` +
		`<p>I've been working on a few Projects, but my original finally got a reboot.</p>` +
		`<p><a href="https://Nestarr.com">https://Nestarr.com</a></p>` +
		`<p>A self-hosted Home Inventory Program.</p>` +
		`<p>github: <a href="https://github.com/tokendad/nestarr">https://github.com/tokendad/nestarr</a></p>` +
		`<p>Looking for critique</p></div><!-- SC_ON -->`)
	if got := embedBody(ctx, body); strings.Count(got, "link-preview-lazy") != 2 {
		t.Errorf("self-post with site + repo should card both links: %s", got)
	}
}

func TestLinkPostPreviewEligible(t *testing.T) {
	mk := func(body, mediaURL string) reddit.Post {
		return reddit.Post{PostType: "link", Body: template.HTML(body), Media: reddit.Media{URL: mediaURL}}
	}
	dest := "https://github.com/goposta/posta"

	// Short post, destination only (body empty) → eligible.
	if !linkPostPreviewEligible(mk("", dest)) {
		t.Error("empty-body link post should be eligible")
	}
	// Body repeats the destination → still one unique link → eligible.
	if !linkPostPreviewEligible(mk(`<a href="`+dest+`">`+dest+`</a>`, dest)) {
		t.Error("body repeating destination should stay eligible")
	}
	// Body adds a second distinct external link → not eligible.
	if linkPostPreviewEligible(mk(`<a href="https://other.com/x">https://other.com/x</a>`, dest)) {
		t.Error("a second external link must disqualify")
	}
	// Long body → not eligible.
	if linkPostPreviewEligible(mk(strings.Repeat("x", previewExpandThreshold+1), dest)) {
		t.Error("long body must disqualify")
	}
	// Not a link post → not eligible.
	if linkPostPreviewEligible(reddit.Post{PostType: "self", Media: reddit.Media{URL: dest}}) {
		t.Error("non-link post must be ineligible")
	}
}

// TestEmbedBodyCards pins the "internal cards" mode: embedBodyCards upgrades
// EVERY external link to a card regardless of body length or link count (unlike
// the gated embedBody), since that mode is chosen precisely for long/multi posts.
func TestEmbedBodyCards(t *testing.T) {
	ctx := context.Background()
	multi := template.HTML(`<p><a href="https://a.com/1">https://a.com/1</a> and <a href="https://b.com/2">https://b.com/2</a></p>`)
	if got := embedBodyCards(ctx, multi); strings.Count(got, "link-preview-lazy") != 2 {
		t.Errorf("embedBodyCards should card all links: %s", got)
	}
	long := template.HTML(`<p><a href="https://github.com/golang/go">https://github.com/golang/go</a></p>` +
		strings.Repeat("x", previewExpandThreshold+1))
	if got := embedBodyCards(ctx, long); !strings.Contains(got, "link-preview-lazy") {
		t.Errorf("embedBodyCards should card even a long body: %s", got)
	}
	if got := embedBodyNoCards(ctx, multi); strings.Contains(got, "link-preview-lazy") {
		t.Errorf("embedBodyNoCards must never card: %s", got)
	}
}

func TestListingPreviewURL(t *testing.T) {
	dest := "https://github.com/goposta/posta"
	// Link post → its destination.
	if got := listingPreviewURL(reddit.Post{PostType: "link", Media: reddit.Media{URL: dest}}); got != dest {
		t.Errorf("link post: got %q want %q", got, dest)
	}
	// Link post with a long write-up body → no strip preview (past the fold).
	longLink := reddit.Post{PostType: "link", Media: reddit.Media{URL: dest},
		Body: template.HTML(strings.Repeat("x", previewExpandThreshold+1))}
	if got := listingPreviewURL(longLink); got != "" {
		t.Errorf("long-body link post should have no preview url, got %q", got)
	}
	// Link post to a reddit/image URL → no strip preview.
	if got := listingPreviewURL(reddit.Post{PostType: "link", Media: reddit.Media{URL: "https://i.redd.it/x.png"}}); got != "" {
		t.Errorf("image link post should have no preview url, got %q", got)
	}
	// Short self post with a single body link → that link (original casing/slash).
	blog := "https://www.bud1m.com/blog/Go-Empty-Struct/"
	self1 := reddit.Post{PostType: "self", Body: template.HTML(`<a href="` + blog + `">` + blog + `</a>`)}
	if got := listingPreviewURL(self1); got != blog {
		t.Errorf("single-link self post: got %q want %q", got, blog)
	}
	// Self post with two distinct links → no single preview.
	self2 := reddit.Post{PostType: "self", Body: template.HTML(
		`<a href="https://a.com/1">https://a.com/1</a> <a href="https://b.com/2">https://b.com/2</a>`)}
	if got := listingPreviewURL(self2); got != "" {
		t.Errorf("multi-link self post should have no preview url, got %q", got)
	}
	// Long self post → no preview even with one link.
	self3 := reddit.Post{PostType: "self", Body: template.HTML(
		`<a href="` + blog + `">` + blog + `</a>` + strings.Repeat("x", previewExpandThreshold+1))}
	if got := listingPreviewURL(self3); got != "" {
		t.Errorf("long self post should have no preview url, got %q", got)
	}
}

func TestURLHost(t *testing.T) {
	cases := map[string]string{
		"https://www.bud1m.com/blog/x": "bud1m.com",
		"https://github.com/a/b":       "github.com",
		"not a url":                    "not a url",
	}
	for in, want := range cases {
		if got := urlHost(in); got != want {
			t.Errorf("urlHost(%q) = %q, want %q", in, got, want)
		}
	}
}
