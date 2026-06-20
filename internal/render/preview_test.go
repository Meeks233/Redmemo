package render

import (
	"context"
	"strings"
	"testing"
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
