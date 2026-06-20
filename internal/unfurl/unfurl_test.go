package unfurl

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

// fakeDoer answers requests from a URL→response table so Fetch can be exercised
// without any network or TLS. It records the URLs it was asked for so tests can
// assert the host-fixup rewrite and the Jina fallback ordering.
type fakeDoer struct {
	pages     map[string]fakePage
	requested []string
}

type fakePage struct {
	status int
	body   string
}

func (f *fakeDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	url := req.URL.String()
	f.requested = append(f.requested, url)
	p, ok := f.pages[url]
	if !ok {
		return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("nope")), Header: fhttp.Header{}}, nil
	}
	return &fhttp.Response{
		StatusCode: p.status,
		Body:       io.NopCloser(strings.NewReader(p.body)),
		Header:     fhttp.Header{},
	}, nil
}

const ogHTML = `<!doctype html><html><head>
<meta property="og:title" content="Hello &amp; World"/>
<meta property="og:description" content="A nice description"/>
<meta property="og:image" content="https://cdn.example.com/img.png"/>
<meta property="og:site_name" content="Example"/>
<title>fallback title</title>
</head><body>ignored</body></html>`

func TestParseMeta(t *testing.T) {
	m := parseMeta(strings.NewReader(ogHTML))
	if m["og:title"] != "Hello & World" {
		t.Errorf("og:title = %q (entity should be unescaped)", m["og:title"])
	}
	if m["og:image"] != "https://cdn.example.com/img.png" {
		t.Errorf("og:image = %q", m["og:image"])
	}
	if m["__title__"] != "fallback title" {
		t.Errorf("__title__ = %q", m["__title__"])
	}
}

func TestFetchDirectOG(t *testing.T) {
	const page = "https://news.example.com/story"
	f := &fetcher{client: &fakeDoer{pages: map[string]fakePage{
		page: {200, ogHTML},
	}}}
	p, err := f.Fetch(context.Background(), page)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !p.Usable() {
		t.Fatal("expected usable preview")
	}
	if p.URL != page {
		t.Errorf("URL = %q, want original %q", p.URL, page)
	}
	if p.Title != "Hello & World" || p.ImageURL != "https://cdn.example.com/img.png" || p.SiteName != "Example" {
		t.Errorf("unexpected preview: %+v", p)
	}
}

// TestFetchTwitterFixup checks x.com is fetched via the fixupx mirror while the
// returned card still points at the original x.com URL.
func TestFetchTwitterFixup(t *testing.T) {
	const original = "https://x.com/jack/status/20"
	const mirror = "https://fixupx.com/jack/status/20"
	doer := &fakeDoer{pages: map[string]fakePage{
		mirror: {200, `<head><meta property="og:title" content="jack"><meta property="og:image" content="https://pbs.twimg.com/x.jpg"></head>`},
	}}
	f := &fetcher{client: doer}
	p, err := f.Fetch(context.Background(), original)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !p.Usable() || p.Title != "jack" {
		t.Fatalf("unexpected preview: %+v", p)
	}
	if p.URL != original {
		t.Errorf("card URL = %q, must stay original x.com link", p.URL)
	}
	if len(doer.requested) == 0 || doer.requested[0] != mirror {
		t.Errorf("expected first fetch against fixupx mirror, got %v", doer.requested)
	}
}

// TestFetchJinaFallback checks that when the direct fetch returns a non-2xx
// (Cloudflare interstitial), the Jina reader is consulted and its metadata used.
func TestFetchJinaFallback(t *testing.T) {
	const page = "https://blocked.example.com/q/1"
	jina := "https://r.jina.ai/" + page
	doer := &fakeDoer{pages: map[string]fakePage{
		page: {403, "Just a moment..."},
		jina: {200, `{"data":{"title":"Recovered Title","description":"recovered","metadata":{"og:image":"https://blocked.example.com/og.png","og:site_name":"Blocked"}}}`},
	}}
	f := &fetcher{client: doer, jinaFallback: true}
	p, err := f.Fetch(context.Background(), page)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !p.Usable() || p.Title != "Recovered Title" {
		t.Fatalf("expected Jina-recovered preview, got %+v", p)
	}
	if p.ImageURL != "https://blocked.example.com/og.png" || p.SiteName != "Blocked" {
		t.Errorf("unexpected jina preview: %+v", p)
	}
}

// TestFetchJinaDisabled confirms the fallback is gated: with jinaFallback off, a
// blocked page yields no preview rather than silently hitting a third party.
func TestFetchJinaDisabled(t *testing.T) {
	const page = "https://blocked.example.com/q/1"
	doer := &fakeDoer{pages: map[string]fakePage{
		page: {403, "blocked"},
	}}
	f := &fetcher{client: doer, jinaFallback: false}
	p, err := f.Fetch(context.Background(), page)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if p.Usable() {
		t.Errorf("expected no preview when Jina disabled, got %+v", p)
	}
	for _, u := range doer.requested {
		if strings.Contains(u, "r.jina.ai") {
			t.Errorf("Jina was contacted despite being disabled: %v", doer.requested)
		}
	}
}

func TestFetchWideImageAndVideo(t *testing.T) {
	const page = "https://github.com/golang/go"
	doer := &fakeDoer{pages: map[string]fakePage{
		page: {200, `<head>
			<meta property="og:title" content="golang/go">
			<meta property="og:image" content="https://opengraph.githubassets.com/abc/golang/go">
			<meta property="og:image:width" content="1200">
			<meta property="og:image:height" content="600">
			<meta name="twitter:card" content="summary_large_image">
			<meta property="og:video" content="https://video.example.com/clip.mp4">
		</head>`},
	}}
	f := &fetcher{client: doer}
	p, err := f.Fetch(context.Background(), page)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !p.ImageWide {
		t.Error("expected ImageWide=true for summary_large_image / 1200x600")
	}
	if p.VideoURL != "https://video.example.com/clip.mp4" {
		t.Errorf("VideoURL = %q", p.VideoURL)
	}
}

func TestCleanText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"line one<br><br>line two", "line one\n\nline two"},
		{"a <br/> b", "a \n b"},
		{"<b>bold</b> and <i>x</i>", "bold and x"},
		{"  spaced  ", "  spaced  "}, // no tags → untouched
	}
	for _, c := range cases {
		if got := cleanText(c.in); got != c.want {
			t.Errorf("cleanText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDirectVideo(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"youtube embed (text/html) → none",
			map[string]string{"og:video:url": "https://www.youtube.com/embed/abc", "og:video:type": "text/html"}, ""},
		{"direct mp4 by type",
			map[string]string{"og:video:url": "https://v.fixupx.com/x.mp4", "og:video:type": "video/mp4"}, "https://v.fixupx.com/x.mp4"},
		{"direct mp4 by extension, no type",
			map[string]string{"og:video": "https://cdn.example.com/clip.mp4"}, "https://cdn.example.com/clip.mp4"},
		{"hls m3u8",
			map[string]string{"og:video:url": "https://cdn.example.com/v.m3u8", "og:video:type": "application/x-mpegURL"}, "https://cdn.example.com/v.m3u8"},
		{"none", map[string]string{}, ""},
	}
	for _, c := range cases {
		if got := directVideo(c.meta, "https://site.example.com/p"); got != c.want {
			t.Errorf("%s: directVideo = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFetchYouTubeFallsBackToImage verifies a YouTube-style page (HTML-embed
// og:video) yields an image card, not a broken inline <video>.
func TestFetchYouTubeFallsBackToImage(t *testing.T) {
	const page = "https://www.youtube.com/watch?v=abc"
	doer := &fakeDoer{pages: map[string]fakePage{
		page: {200, `<head>
			<meta property="og:title" content="Cool Video">
			<meta property="og:image" content="https://i.ytimg.com/vi/abc/maxresdefault.jpg">
			<meta property="og:video:url" content="https://www.youtube.com/embed/abc">
			<meta property="og:video:type" content="text/html">
		</head>`},
	}}
	f := &fetcher{client: doer}
	p, err := f.Fetch(context.Background(), page)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if p.VideoURL != "" {
		t.Errorf("YouTube HTML embed must NOT be a playable video, got %q", p.VideoURL)
	}
	if p.ImageURL == "" || p.Title != "Cool Video" {
		t.Errorf("expected image card with title, got %+v", p)
	}
}

func TestIsWideImage(t *testing.T) {
	cases := []struct {
		name  string
		meta  map[string]string
		image string
		want  bool
	}{
		// The AuthPlane bug: GitHub sets summary_large_image on a profile whose
		// og:image is a square avatar — must NOT be treated as a banner.
		{"github avatar profile", map[string]string{"twitter:card": "summary_large_image"},
			"https://avatars.githubusercontent.com/u/276113988?s=280&v=4", false},
		{"github repo card", map[string]string{},
			"https://opengraph.githubassets.com/abc/golang/go", true},
		{"github repo image host", map[string]string{},
			"https://repository-images.githubusercontent.com/123/x", true},
		{"explicit landscape dims", map[string]string{"og:image:width": "1280", "og:image:height": "640"},
			"https://news.example.com/hero.jpg", true},
		{"square dims (logo)", map[string]string{"og:image:width": "200", "og:image:height": "200"},
			"https://x.com/logo.png", false},
		{"summary_large_image alone, no dims/host", map[string]string{"twitter:card": "summary_large_image"},
			"https://blog.example.com/img.png", false},
		{"empty", map[string]string{}, "", false},
	}
	for _, c := range cases {
		if got := isWideImage(c.meta, c.image); got != c.want {
			t.Errorf("%s: isWideImage = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFetchRejectsPrivateHost(t *testing.T) {
	doer := &fakeDoer{pages: map[string]fakePage{}}
	f := &fetcher{client: doer, jinaFallback: true}
	for _, u := range []string{
		"http://127.0.0.1/secret",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/x",
		"ftp://8.8.8.8/x",
	} {
		p, _ := f.Fetch(context.Background(), u)
		if p.Usable() {
			t.Errorf("SSRF: %q should not be fetched, got %+v", u, p)
		}
	}
	if len(doer.requested) != 0 {
		t.Errorf("no outbound request should be made for private hosts, got %v", doer.requested)
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"10.0.0.5", false},
		{"172.16.3.4", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"::1", false},
		{"fc00::1", false},
	}
	for _, c := range cases {
		if got := isPublicIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isPublicIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestAbsImage(t *testing.T) {
	cases := []struct{ img, page, want string }{
		{"https://a.com/x.png", "https://b.com/p", "https://a.com/x.png"},
		{"/rel/x.png", "https://b.com/dir/p", "https://b.com/rel/x.png"},
		{"", "https://b.com/p", ""},
	}
	for _, c := range cases {
		if got := absImage(c.img, c.page); got != c.want {
			t.Errorf("absImage(%q,%q) = %q, want %q", c.img, c.page, got, c.want)
		}
	}
}
