package searchquery

import (
	"reflect"
	"testing"
	"time"
)

func TestParseFullExample(t *testing.T) {
	p := Parse(`linux rating:nsfw upvote>100 sub:golang sub:-sfw author:bob`)

	if got := p.TextQuery(); got != "linux" {
		t.Errorf("TextQuery = %q, want %q", got, "linux")
	}
	if p.Rating != "nsfw" {
		t.Errorf("Rating = %q, want nsfw", p.Rating)
	}
	if p.Score == nil || p.Score.Op != OpGT || p.Score.Val != 100 {
		t.Errorf("Score = %+v, want >100", p.Score)
	}
	if !reflect.DeepEqual(p.WhiteSubs, []string{"golang"}) {
		t.Errorf("WhiteSubs = %v, want [golang]", p.WhiteSubs)
	}
	if !reflect.DeepEqual(p.BlackSubs, []string{"sfw"}) {
		t.Errorf("BlackSubs = %v, want [sfw]", p.BlackSubs)
	}
	if p.Author != "bob" {
		t.Errorf("Author = %q, want bob", p.Author)
	}

	want := "subreddit:golang -subreddit:sfw author:bob nsfw:yes linux"
	if got := p.RedditQuery(); got != want {
		t.Errorf("RedditQuery = %q, want %q", got, want)
	}
}

func TestSubClause(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"linux author:bob", ""}, // no sub: clause
		{"sub:cats+dogs", "sub:cats+dogs"},
		{"sub:cats sub:dogs", "sub:cats+dogs"},        // merged
		{"sub:cats+dogs-meta", "sub:cats+dogs-meta"},  // include + exclude
		{"sub:-meta", "sub:-meta"},                    // exclude only
		{"sub:Cats sub:-META", "sub:cats-meta"},       // lowercased
		{"sub:cats+dogs sub:-cats", "sub:dogs-cats"},  // last-wins flips cats
	}
	for _, c := range cases {
		if got := Parse(c.in).SubClause(); got != c.want {
			t.Errorf("Parse(%q).SubClause() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonical(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"sub:cats+dogs-meta", "sub:cats+dogs-meta"},
		// Full query: every honored constraint round-trips in canonical order.
		{"linux rating:nsfw upvote>100 sub:golang sub:-sfw author:bob comments<=5 t:image after:2024-01-02",
			"sub:golang-sfw a:bob r:nsfw t:image u>100 c<=5 after:2024-01-02 linux"},
		// Short aliases normalize to the canonical long/short forms.
		{"u:50 c:3", "u=50 c=3"},
		{"before:2023-12-31", "before:2023-12-31"},
	}
	for _, c := range cases {
		if got := Parse(c.in).Canonical(); got != c.want {
			t.Errorf("Parse(%q).Canonical() = %q, want %q", c.in, got, c.want)
		}
		// Canonical output must itself re-parse to the same canonical form.
		if got := Parse(Parse(c.in).Canonical()).Canonical(); got != c.want {
			t.Errorf("Canonical not idempotent for %q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseSubList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"cats+dogs+memes", []string{"cats", "dogs", "memes"}},
		{"cats dogs", []string{"cats", "dogs"}},          // whitespace separates too
		{"Cats+DOGS", []string{"cats", "dogs"}},          // lowercased
		{"cats+cats+dogs", []string{"cats", "dogs"}},     // deduped
		{"r/cats+/r/dogs", []string{"cats", "dogs"}},     // r/ prefixes stripped
		{"sub:cats+dogs", []string{"cats", "dogs"}},      // tolerates pasted sub: key
		{"cats++dogs+", []string{"cats", "dogs"}},        // empty segments dropped
		{"cats-dogs", []string{"cats", "dogs"}},          // '-' also separates (NP has no excludes)
	}
	for _, c := range cases {
		got := ParseSubList(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseSubList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestJoinSubsRoundTrip(t *testing.T) {
	in := "cats+dogs+memes"
	if got := JoinSubs(ParseSubList(in)); got != in {
		t.Errorf("JoinSubs(ParseSubList(%q)) = %q, want %q", in, got, in)
	}
	// The NP store format (sub:a+b+c) round-trips back to the simple list.
	stored := "sub:" + JoinSubs(ParseSubList(in))
	if got := JoinSubs(Parse(stored).WhiteSubs); got != in {
		t.Errorf("round trip via %q = %q, want %q", stored, got, in)
	}
}

func TestParseNumericOps(t *testing.T) {
	// upvote/u/ups/upvotes target the Reddit post score (p.Score); score targets
	// the media cache eviction score (p.CacheScore). cache reports which field
	// each token must populate.
	cases := []struct {
		in    string
		op    NumOp
		val   int
		cache bool
	}{
		{"upvote>100", OpGT, 100, false},
		{"u>=50", OpGE, 50, false},
		{"upvote<10", OpLT, 10, false},
		{"upvotes<=5", OpLE, 5, false},
		{"ups=7", OpEQ, 7, false},
		{"score>100", OpGT, 100, true},
		{"score>=50", OpGE, 50, true},
		{"score:42", OpEQ, 42, true},
	}
	for _, c := range cases {
		p := Parse(c.in)
		got := p.Score
		other := p.CacheScore
		if c.cache {
			got, other = p.CacheScore, p.Score
		}
		if got == nil {
			t.Fatalf("%q: target constraint nil", c.in)
		}
		if other != nil {
			t.Errorf("%q: unexpected other constraint %+v", c.in, other)
		}
		if got.Op != c.op || got.Val != c.val {
			t.Errorf("%q: got %+v, want op=%s val=%d", c.in, got, c.op, c.val)
		}
		if len(p.Terms) != 0 {
			t.Errorf("%q: unexpected terms %v", c.in, p.Terms)
		}
	}
}

func TestParseCacheScoreDistinctFromUpvote(t *testing.T) {
	// A query carrying both must populate the two fields independently.
	p := Parse("upvote>100 score<20")
	if p.Score == nil || p.Score.Op != OpGT || p.Score.Val != 100 {
		t.Errorf("Score = %+v, want >100", p.Score)
	}
	if p.CacheScore == nil || p.CacheScore.Op != OpLT || p.CacheScore.Val != 20 {
		t.Errorf("CacheScore = %+v, want <20", p.CacheScore)
	}
	// CacheScore must never leak into the live Reddit query or the live filter.
	if got := p.RedditQuery(); got != "" {
		t.Errorf("RedditQuery = %q, want empty (cache score must not reach Reddit)", got)
	}
	// Canonical round-trips both constraints.
	rt := Parse(p.Canonical())
	if rt.CacheScore == nil || rt.CacheScore.Op != OpLT || rt.CacheScore.Val != 20 {
		t.Errorf("Canonical round-trip CacheScore = %+v, want <20 (canonical=%q)", rt.CacheScore, p.Canonical())
	}
	if rt.Score == nil || rt.Score.Op != OpGT || rt.Score.Val != 100 {
		t.Errorf("Canonical round-trip Score = %+v, want >100 (canonical=%q)", rt.Score, p.Canonical())
	}
}

func TestMatchFloat(t *testing.T) {
	nc := NumConstraint{Op: OpGT, Val: 50}
	if !nc.MatchFloat(73.4) {
		t.Errorf("MatchFloat(73.4) for >50 = false, want true")
	}
	if nc.MatchFloat(50.0) {
		t.Errorf("MatchFloat(50.0) for >50 = true, want false")
	}
	le := NumConstraint{Op: OpLE, Val: 10}
	if !le.MatchFloat(10.0) || le.MatchFloat(10.1) {
		t.Errorf("MatchFloat for <=10 mishandled boundary")
	}
}

func TestParseComments(t *testing.T) {
	p := Parse("linux comments>=50")
	if p.Comments == nil || p.Comments.Op != OpGE || p.Comments.Val != 50 {
		t.Fatalf("Comments = %+v, want >=50", p.Comments)
	}
	if p.TextQuery() != "linux" {
		t.Errorf("TextQuery = %q, want linux", p.TextQuery())
	}
}

func TestParseMultiWhite(t *testing.T) {
	p := Parse("cats sub:r/cats sub:dogs")
	if !reflect.DeepEqual(p.WhiteSubs, []string{"cats", "dogs"}) {
		t.Fatalf("WhiteSubs = %v, want [cats dogs]", p.WhiteSubs)
	}
	want := "(subreddit:cats OR subreddit:dogs) cats"
	if got := p.RedditQuery(); got != want {
		t.Errorf("RedditQuery = %q, want %q", got, want)
	}
}

func TestParseSubGreedyInclude(t *testing.T) {
	p := Parse("sub:golang+rust+python")
	if !reflect.DeepEqual(p.WhiteSubs, []string{"golang", "rust", "python"}) {
		t.Fatalf("WhiteSubs = %v, want [golang rust python]", p.WhiteSubs)
	}
	if len(p.BlackSubs) != 0 {
		t.Errorf("BlackSubs = %v, want empty", p.BlackSubs)
	}
}

func TestParseSubGreedyExclude(t *testing.T) {
	p := Parse("sub:-golang-rust")
	if !reflect.DeepEqual(p.BlackSubs, []string{"golang", "rust"}) {
		t.Fatalf("BlackSubs = %v, want [golang rust]", p.BlackSubs)
	}
	if len(p.WhiteSubs) != 0 {
		t.Errorf("WhiteSubs = %v, want empty", p.WhiteSubs)
	}
}

func TestParseSubMixedAndOverride(t *testing.T) {
	// golang first included then excluded by a later token: last write wins.
	p := Parse("sub:golang+linux sub:-golang")
	if !reflect.DeepEqual(p.WhiteSubs, []string{"linux"}) {
		t.Errorf("WhiteSubs = %v, want [linux]", p.WhiteSubs)
	}
	if !reflect.DeepEqual(p.BlackSubs, []string{"golang"}) {
		t.Errorf("BlackSubs = %v, want [golang]", p.BlackSubs)
	}
}

func TestParseShortAliases(t *testing.T) {
	p := Parse("s:golang u>100 r:nsfw")
	if !reflect.DeepEqual(p.WhiteSubs, []string{"golang"}) {
		t.Errorf("WhiteSubs = %v, want [golang]", p.WhiteSubs)
	}
	if p.Score == nil || p.Score.Op != OpGT || p.Score.Val != 100 {
		t.Errorf("Score = %+v, want >100", p.Score)
	}
	if p.Rating != "nsfw" {
		t.Errorf("Rating = %q, want nsfw", p.Rating)
	}
	if len(p.Terms) != 0 {
		t.Errorf("Terms = %v, want empty", p.Terms)
	}
}

func TestParseMediaVideoGif(t *testing.T) {
	if p := Parse("type:video"); p.MediaType != "video" {
		t.Errorf("type:video: MediaType = %q, want video", p.MediaType)
	}
	if p := Parse("t:gif"); p.MediaType != "gif" {
		t.Errorf("t:gif: MediaType = %q, want gif", p.MediaType)
	}
	if p := Parse("c>10"); p.Comments == nil || p.Comments.Val != 10 {
		t.Errorf("c>10: Comments = %+v, want >10", p.Comments)
	}
}

func TestParseQuotedFlairAndPhrase(t *testing.T) {
	p := Parse(`"linux art" flair:"oc art"`)
	if !reflect.DeepEqual(p.Terms, []string{"linux art"}) {
		t.Errorf("Terms = %v, want [linux art]", p.Terms)
	}
	if p.Flair != "oc art" {
		t.Errorf("Flair = %q, want 'oc art'", p.Flair)
	}
	if got := p.RedditQuery(); got != `flair_name:"oc art" "linux art"` {
		t.Errorf("RedditQuery = %q", got)
	}
}

func TestParseRatingSafe(t *testing.T) {
	p := Parse("rating:safe puppies")
	if p.Rating != "safe" {
		t.Fatalf("Rating = %q, want safe", p.Rating)
	}
	if got := p.RedditQuery(); got != "nsfw:no puppies" {
		t.Errorf("RedditQuery = %q, want 'nsfw:no puppies'", got)
	}
}

func TestParseDates(t *testing.T) {
	p := Parse("kernel after:2024-01-01 before:2024-06-30")
	if p.After == nil || !p.After.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("After = %v", p.After)
	}
	if p.Before == nil || !p.Before.Equal(time.Date(2024, 6, 30, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("Before = %v", p.Before)
	}
	if !p.HasLocalFilter() {
		t.Errorf("HasLocalFilter = false, want true")
	}
}

func TestParseType(t *testing.T) {
	if p := Parse("type:image"); p.MediaType != "image" {
		t.Errorf("image: MediaType = %q", p.MediaType)
	}
	if p := Parse("media:gif"); p.MediaType != "gif" {
		t.Errorf("gif: MediaType = %q", p.MediaType)
	}
}

func TestUnknownKeyStaysText(t *testing.T) {
	p := Parse("foo:bar baz")
	want := []string{"foo:bar", "baz"}
	if !reflect.DeepEqual(p.Terms, want) {
		t.Errorf("Terms = %v, want %v", p.Terms, want)
	}
}

func TestNumConstraintMatch(t *testing.T) {
	n := NumConstraint{Op: OpGT, Val: 100}
	if !n.Match(101) || n.Match(100) || n.Match(99) {
		t.Errorf("OpGT match wrong")
	}
}

func TestEmptyQuery(t *testing.T) {
	p := Parse("   ")
	if len(p.Terms) != 0 || p.RedditQuery() != "" || p.HasLocalFilter() {
		t.Errorf("empty query not empty: %+v", p)
	}
}
