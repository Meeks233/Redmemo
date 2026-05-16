package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPathToCDNURL(t *testing.T) {
	cases := []struct {
		path     string
		rawQuery string
		want     string
	}{
		{"/img/abc.jpg", "", "https://i.redd.it/abc.jpg"},
		{"/img/abc.jpg", "width=100", "https://i.redd.it/abc.jpg?width=100"},
		{"/preview/pre/x.jpg", "", "https://preview.redd.it/x.jpg"},
		{"/preview/external-pre/y.jpg", "", "https://external-preview.redd.it/y.jpg"},
		{"/thumb/a/t.jpg", "", "https://a.thumbs.redditmedia.com/t.jpg"},
		{"/thumb/b/t.jpg", "", "https://b.thumbs.redditmedia.com/t.jpg"},
		{"/emoji/e.png", "", "https://emoji.redditmedia.com/e.png"},
		{"/emoji/e.png", "size=64", "https://emoji.redditmedia.com/e.png?size=64"},
		// unrecognized prefixes map to nothing
		{"/unknown/x", "", ""},
		{"/", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := pathToCDNURL(c.path, c.rawQuery); got != c.want {
			t.Errorf("pathToCDNURL(%q,%q) = %q, want %q", c.path, c.rawQuery, got, c.want)
		}
	}
}

func TestValidateFromPath(t *testing.T) {
	cases := []struct {
		from string
		want string
	}{
		// accepted: known top-level segments under a single leading slash
		{"/r/golang", "/r/golang"},
		{"/user/spez", "/user/spez"},
		{"/search?q=cats", "/search?q=cats"},
		{"/r/golang/comments/abc/title", "/r/golang/comments/abc/title"},
		// rejected: open-redirect / protocol-relative
		{"//evil.com", ""},
		{"https://evil.com", ""},
		{"r/golang", ""}, // no leading slash
		{"", ""},
		// rejected: segment not in the allowlist
		{"/comments/abc", ""},
		{"/admin", ""},
		// rejected: characters that could break out of HTML/URL context
		{"/r/go\"lang", ""},
		{"/r/go<script>", ""},
		{"/r/go>lang", ""},
		{"/r/go\\lang", ""},
		{"/r/go\x01lang", ""},
		{"/r/go\x7flang", ""},
	}
	for _, c := range cases {
		if got := validateFromPath(c.from); got != c.want {
			t.Errorf("validateFromPath(%q) = %q, want %q", c.from, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m30s"},
		{time.Hour, "60m0s"},
		{400 * time.Millisecond, "0s"}, // rounds down to the nearest second
		{500 * time.Millisecond, "1s"}, // rounds half away from zero
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1 << 10, "1.00 KB"},
		{2048, "2.00 KB"},
		{5 << 20, "5.00 MB"},
		{3 << 30, "3.00 GB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.b); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}

func TestValidSubName(t *testing.T) {
	valid := []string{"go", "golang", "AskReddit", "AskReddit_2", "a1", "ABCDEFGHIJKLMNOPQRSTU"}
	for _, s := range valid {
		if !validSubName.MatchString(s) {
			t.Errorf("validSubName rejected a valid name: %q", s)
		}
	}
	invalid := []string{
		"",                       // empty
		"a",                      // too short (needs >= 2 chars)
		"_golang",                // leading underscore
		"go-lang",                // hyphen not allowed
		"go lang",                // space
		"go.lang",                // dot
		"ABCDEFGHIJKLMNOPQRSTUV", // 22 chars — too long
	}
	for _, s := range invalid {
		if validSubName.MatchString(s) {
			t.Errorf("validSubName accepted an invalid name: %q", s)
		}
	}
}

func TestRedirectFuckReddit(t *testing.T) {
	h := &Handler{} // method touches no Handler fields

	t.Run("with from and reason", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/r/golang", nil)
		h.redirectFuckReddit(rec, req, "/r/golang", "hr_l1")

		if rec.Code != http.StatusTemporaryRedirect {
			t.Errorf("status = %d, want 307", rec.Code)
		}
		loc := rec.Header().Get("Location")
		// url.Values.Encode sorts keys: from, then reason.
		if loc != "/fuckreddit?from=%2Fr%2Fgolang&reason=hr_l1" {
			t.Errorf("Location = %q", loc)
		}
	})

	t.Run("no params", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		h.redirectFuckReddit(rec, req, "", "")

		if loc := rec.Header().Get("Location"); loc != "/fuckreddit" {
			t.Errorf("Location = %q, want /fuckreddit", loc)
		}
	})

	t.Run("reason only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		h.redirectFuckReddit(rec, req, "", "quota_exhausted")

		if loc := rec.Header().Get("Location"); loc != "/fuckreddit?reason=quota_exhausted" {
			t.Errorf("Location = %q", loc)
		}
	})
}
