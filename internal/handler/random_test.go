package handler

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/searchquery"
)

func TestRandomQueryExpr(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"empty", "/random", ""},
		{"plain spaces", "/random?q=sub:golang%20ups%3E200%20type:image", "sub:golang ups>200 type:image"},
		{"ampersand separators", "/random?q=sub:golang&ups%3E1000&type:image", "sub:golang ups>1000 type:image"},
		{"literal plus joiner", "/random?q=type:vid+gif", "type:vid+gif"},
		{"literal plus in sub joiner", "/random?q=sub:golang+linux", "sub:golang+linux"},
		{"no q prefix", "/random?sub:golang&ups%3E1000", "sub:golang ups>1000"},
		{"percent-encoded literal ampersand", "/random?q=flair:a%26b", "flair:a&b"},
		{"quoted multiword via %20", "/random?q=flair:%22male%20only%22&ups>10", `flair:"male only" ups>10`},
		{"trailing ampersand drops empty segment", "/random?q=sub:golang&", "sub:golang"},
		{"leading ampersand drops empty segment", "/random?&q=sub:golang", "sub:golang"},
		{"multiple ampersands in a row", "/random?q=sub:golang&&ups>10", "sub:golang ups>10"},
		{"mode raw flag preserved", "/random?q=sub:golang&mode:raw", "sub:golang mode:raw"},
		{"cache_score constraint", "/random?q=sub:golang&cached>50", "sub:golang cached>50"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.url, nil)
			if got := randomQueryExpr(r); got != tt.want {
				t.Errorf("randomQueryExpr(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestWriteRandomError(t *testing.T) {
	cases := []struct {
		name string
		code int
		msg  string
	}{
		{"bad request", http.StatusBadRequest, "something went wrong"},
		{"service unavailable", http.StatusServiceUnavailable, "no archived post matches"},
		{"internal error", http.StatusInternalServerError, "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeRandomError(rec, c.code, c.msg)

			if rec.Code != c.code {
				t.Errorf("status = %d, want %d", rec.Code, c.code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if body["error"] != c.msg {
				t.Errorf("error field = %q, want %q", body["error"], c.msg)
			}
		})
	}
}

func TestFrac(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{0.25, 0.25},
		{1, 0},
		{1.75, 0.75},
		{2.5, 0.5},
		{-0.25, 0.75}, // -0.25 - floor(-0.25) = -0.25 - (-1) = 0.75
	}
	for _, c := range cases {
		got := frac(c.in)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("frac(%v) = %v, want %v", c.in, got, c.want)
		}
		if got < 0 || got >= 1 {
			t.Errorf("frac(%v) = %v, must be in [0,1)", c.in, got)
		}
	}
}

func TestLockRandomWalkSerialization(t *testing.T) {
	// Two goroutines acquiring the same key must not overlap. Two distinct
	// keys must be able to hold their locks concurrently.
	const sameKey = "random:walk:test-same"

	var active int32
	var maxActive int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	track := func() func() {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		return func() {
			mu.Lock()
			active--
			mu.Unlock()
		}
	}

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := lockRandomWalk(sameKey)
			defer unlock()
			done := track()
			time.Sleep(5 * time.Millisecond)
			done()
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Errorf("same-key walks overlapped: maxActive=%d, want 1", maxActive)
	}

	// Distinct keys: should be able to overlap.
	maxActive = 0
	active = 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		key := "random:walk:distinct-" + string(rune('a'+i))
		go func(k string) {
			defer wg.Done()
			unlock := lockRandomWalk(k)
			defer unlock()
			done := track()
			time.Sleep(10 * time.Millisecond)
			done()
		}(key)
	}
	wg.Wait()
	if maxActive < 2 {
		t.Errorf("distinct-key walks didn't overlap: maxActive=%d, want >=2", maxActive)
	}
}

func TestReadWalkStateNilCache(t *testing.T) {
	// With a nil cache, readWalkState must return (0,0) and writeWalkState
	// must be a no-op (no panic).
	h := &Handler{}
	o, c := h.readWalkState(context.Background(), "any-key")
	if o != 0 || c != 0 {
		t.Errorf("readWalkState(nil cache) = (%v,%v), want (0,0)", o, c)
	}
	// must not panic
	h.writeWalkState(context.Background(), "any-key", 0.5, 0.25)
}

func TestRandomWalkConstants(t *testing.T) {
	if randomMediaPoolSize <= 0 {
		t.Errorf("randomMediaPoolSize must be positive, got %d", randomMediaPoolSize)
	}
	if randomFilterMaxPages <= 0 {
		t.Errorf("randomFilterMaxPages must be positive, got %d", randomFilterMaxPages)
	}
	if randomWalkTTL <= 0 {
		t.Errorf("randomWalkTTL must be positive, got %v", randomWalkTTL)
	}
}

func TestParsedToArchiveOpts(t *testing.T) {
	// Sanity-check the mapping that /random hands to PostStore. All filter
	// surfaces /random exposes through the query box must propagate.
	parsed := searchquery.Parse("sub:golang ups>100 author:alice flair:OC type:image rating:nsfw")
	opts := parsedToArchiveOpts(parsed)

	if len(opts.WhiteSubs) == 0 || opts.WhiteSubs[0] != "golang" {
		t.Errorf("WhiteSubs = %v, want [golang]", opts.WhiteSubs)
	}
	if opts.Author != "alice" {
		t.Errorf("Author = %q", opts.Author)
	}
	if opts.Flair != "OC" {
		t.Errorf("Flair = %q", opts.Flair)
	}
	if opts.NSFW != "nsfw" {
		t.Errorf("NSFW = %q", opts.NSFW)
	}
	if opts.Score == nil || *opts.Score != 100 || opts.ScoreOp != ">" {
		t.Errorf("Score = %v op=%q, want (100, '>')", opts.Score, opts.ScoreOp)
	}
	if len(opts.Media) != 1 || opts.Media[0] != "image" {
		t.Errorf("Media = %v, want [image]", opts.Media)
	}

	// rating:safe → "sfw"
	if got := parsedToArchiveOpts(searchquery.Parse("rating:safe")).NSFW; got != "sfw" {
		t.Errorf("rating:safe NSFW = %q, want sfw", got)
	}
	// no rating → empty
	if got := parsedToArchiveOpts(searchquery.Parse("")).NSFW; got != "" {
		t.Errorf("no rating NSFW = %q, want empty", got)
	}
	// comments constraint
	pc := parsedToArchiveOpts(searchquery.Parse("comments>=5"))
	if pc.Comments == nil || *pc.Comments != 5 || pc.CommentsOp != ">=" {
		t.Errorf("Comments = %v op=%q, want (5, '>=')", pc.Comments, pc.CommentsOp)
	}
}

func TestPrimaryMediaURL(t *testing.T) {
	// Empty post → empty.
	if got := primaryMediaURL(&reddit.Post{}); got != "" {
		t.Errorf("empty post = %q, want empty", got)
	}
	// Media.URL takes precedence over gallery.
	p := &reddit.Post{}
	p.Media.URL = "https://i.example.com/a.jpg"
	p.Gallery = []reddit.GalleryMedia{{URL: "https://i.example.com/g.jpg"}}
	if got := primaryMediaURL(p); got != "https://i.example.com/a.jpg" {
		t.Errorf("Media-takes-precedence = %q", got)
	}
	// Gallery fallback when Media.URL empty.
	p2 := &reddit.Post{Gallery: []reddit.GalleryMedia{{URL: "https://i.example.com/g.jpg"}}}
	if got := primaryMediaURL(p2); got != "https://i.example.com/g.jpg" {
		t.Errorf("Gallery fallback = %q", got)
	}
}

func TestRandomQueryExprInstantMode(t *testing.T) {
	// The mode:raw / mode:instant flag must survive the rewrite so the
	// /random handler sees parsed.Instant == true.
	r := httptest.NewRequest(http.MethodGet, "/random?q=sub:golang&mode:raw", nil)
	parsed := searchquery.Parse(randomQueryExpr(r))
	if !parsed.Instant {
		t.Errorf("mode:raw did not set parsed.Instant")
	}
}
