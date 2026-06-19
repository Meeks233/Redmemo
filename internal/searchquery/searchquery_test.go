package searchquery

import (
	"reflect"
	"testing"
	"time"
)

func TestParseFullExample(t *testing.T) {
	p := Parse(`linux rating:nsfw score>100 sub:golang sub:-sfw author:bob`)

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
		{"linux author:bob", ""},
		{"sub:cats+dogs", "sub:cats+dogs"},
		{"sub:cats sub:dogs", "sub:cats+dogs"},
		{"sub:cats+dogs-meta", "sub:cats+dogs-meta"},
		{"sub:-meta", "sub:-meta"},
		{"sub:Cats sub:-META", "sub:cats-meta"},
		{"sub:cats+dogs sub:-cats", "sub:dogs-cats"},
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
		// New rule: inequalities never carry a colon; equality always uses a colon.
		{"linux rating:nsfw score>100 sub:golang sub:-sfw author:bob comments<=5 type:image date>2024-01-02",
			"sub:golang-sfw author:bob rating:nsfw type:image score>100 comments<=5 date>2024-01-02 linux"},
		// Short aliases normalize to the canonical long forms.
		{"sr:golang ups:50 comments:3", "sub:golang score:50 comments:3"},
		{"date<2023-12-31", "date<2023-12-31"},
	}
	for _, c := range cases {
		if got := Parse(c.in).Canonical(); got != c.want {
			t.Errorf("Parse(%q).Canonical() = %q, want %q", c.in, got, c.want)
		}
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
		{"cats dogs", []string{"cats", "dogs"}},
		{"Cats+DOGS", []string{"cats", "dogs"}},
		{"cats+cats+dogs", []string{"cats", "dogs"}},
		{"r/cats+/r/dogs", []string{"cats", "dogs"}},
		{"sub:cats+dogs", []string{"cats", "dogs"}},
		{"cats++dogs+", []string{"cats", "dogs"}},
		{"cats-dogs", []string{"cats", "dogs"}},
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
	stored := "sub:" + JoinSubs(ParseSubList(in))
	if got := JoinSubs(Parse(stored).WhiteSubs); got != in {
		t.Errorf("round trip via %q = %q, want %q", stored, got, in)
	}
}

func TestParseNumericOps(t *testing.T) {
	// score/ups → Reddit post score; cached → media cache eviction score.
	cases := []struct {
		in    string
		op    NumOp
		val   int
		cache bool
	}{
		{"score>100", OpGT, 100, false},
		{"score>=50", OpGE, 50, false},
		{"score<10", OpLT, 10, false},
		{"score<=5", OpLE, 5, false},
		{"score=7", OpEQ, 7, false},
		{"score:42", OpEQ, 42, false},
		{"ups>=50", OpGE, 50, false},
		{"cached>100", OpGT, 100, true},
		{"cached>=50", OpGE, 50, true},
		{"cached:42", OpEQ, 42, true},
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

func TestNumericOpRejectsColonWithSign(t *testing.T) {
	// Rule: inequalities never carry a colon. `score:>42` must fall back to
	// free text, not silently parse.
	for _, in := range []string{"score:>42", "score:<=10", "date:>2024-01-01"} {
		p := Parse(in)
		if p.Score != nil || p.CacheScore != nil || p.Comments != nil ||
			p.After != nil || p.Before != nil {
			t.Errorf("%q: expected free-text fallback, got parsed constraint", in)
		}
		if len(p.Terms) != 1 || p.Terms[0] != in {
			t.Errorf("%q: terms=%v, want [%q]", in, p.Terms, in)
		}
	}
}

func TestParseCacheScoreDistinctFromScore(t *testing.T) {
	p := Parse("score>100 cached<20")
	if p.Score == nil || p.Score.Op != OpGT || p.Score.Val != 100 {
		t.Errorf("Score = %+v, want >100", p.Score)
	}
	if p.CacheScore == nil || p.CacheScore.Op != OpLT || p.CacheScore.Val != 20 {
		t.Errorf("CacheScore = %+v, want <20", p.CacheScore)
	}
	// CacheScore must never leak into the live Reddit query.
	if got := p.RedditQuery(); got != "" {
		t.Errorf("RedditQuery = %q, want empty (cached: must not reach Reddit)", got)
	}
	rt := Parse(p.Canonical())
	if rt.CacheScore == nil || rt.CacheScore.Op != OpLT || rt.CacheScore.Val != 20 {
		t.Errorf("round-trip CacheScore = %+v, want <20 (canonical=%q)", rt.CacheScore, p.Canonical())
	}
	if rt.Score == nil || rt.Score.Op != OpGT || rt.Score.Val != 100 {
		t.Errorf("round-trip Score = %+v (canonical=%q)", rt.Score, p.Canonical())
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
	p := Parse("sub:golang+linux sub:-golang")
	if !reflect.DeepEqual(p.WhiteSubs, []string{"linux"}) {
		t.Errorf("WhiteSubs = %v, want [linux]", p.WhiteSubs)
	}
	if !reflect.DeepEqual(p.BlackSubs, []string{"golang"}) {
		t.Errorf("BlackSubs = %v, want [golang]", p.BlackSubs)
	}
}

func TestParseRetainedShortAliases(t *testing.T) {
	// sr / ups / user / media / r / order are the short aliases that survived
	// the trim. The single-letter ambiguous ones (s/a/c/f/u) are gone.
	p := Parse("sr:golang ups>100 r:nsfw media:gif user:bob order:top")
	if !reflect.DeepEqual(p.WhiteSubs, []string{"golang"}) {
		t.Errorf("WhiteSubs = %v, want [golang]", p.WhiteSubs)
	}
	if p.Score == nil || p.Score.Op != OpGT || p.Score.Val != 100 {
		t.Errorf("Score = %+v, want >100", p.Score)
	}
	if p.Rating != "nsfw" {
		t.Errorf("Rating = %q, want nsfw", p.Rating)
	}
	if !reflect.DeepEqual(p.MediaTypes, []string{"gif"}) {
		t.Errorf("MediaTypes = %v, want [gif]", p.MediaTypes)
	}
	if p.Author != "bob" {
		t.Errorf("Author = %q, want bob", p.Author)
	}
	if p.Sort != "top" {
		t.Errorf("Sort = %q, want top", p.Sort)
	}
	if len(p.Terms) != 0 {
		t.Errorf("Terms = %v, want empty", p.Terms)
	}
}

func TestDroppedAliasesAreFreeText(t *testing.T) {
	// Aliases that were retired must NOT silently re-attach to their concepts.
	for _, in := range []string{
		"s:golang",         // dropped: ambiguous with score
		"a:bob",            // dropped: ambiguous with after
		"c:5",              // dropped: ambiguous with cached
		"f:art",            // dropped: low value
		"upvote>100",       // dropped: redundant with score
		"upvotes>100",      // dropped: redundant with score
		"u:50",             // dropped: ambiguous single letter
		"comment:5",        // dropped: redundant with comments
		"cache_score:50",   // dropped: renamed to cached
		"subreddit:golang", // dropped: redundant with sub
		"time:week",        // dropped: merged into date
		"timeframe:week",   // dropped
		"tf:week",          // dropped
		"since:2024-01-01", // dropped: redundant with after
		"until:2024-12-31", // dropped: redundant with before
		"flair_name:art",   // dropped
	} {
		p := Parse(in)
		if len(p.Terms) != 1 || p.Terms[0] != in {
			t.Errorf("%q: expected free-text fallback, got terms=%v parsed=%+v", in, p.Terms, p)
		}
	}
}

func TestParseMedia(t *testing.T) {
	if p := Parse("type:video"); !reflect.DeepEqual(p.MediaTypes, []string{"video"}) {
		t.Errorf("type:video: MediaTypes = %v, want [video]", p.MediaTypes)
	}
	if p := Parse("media:gif"); !reflect.DeepEqual(p.MediaTypes, []string{"gif"}) {
		t.Errorf("media:gif: MediaTypes = %v, want [gif]", p.MediaTypes)
	}
	if p := Parse("type:gif+vid"); !reflect.DeepEqual(p.MediaTypes, []string{"gif", "video"}) {
		t.Errorf("type:gif+vid: MediaTypes = %v, want [gif video]", p.MediaTypes)
	}
	if p := Parse("type:img+vid"); !reflect.DeepEqual(p.MediaTypes, []string{"image", "video"}) {
		t.Errorf("type:img+vid: MediaTypes = %v, want [image video]", p.MediaTypes)
	}
	if p := Parse("type:img+image+pic"); !reflect.DeepEqual(p.MediaTypes, []string{"image"}) {
		t.Errorf("type:img+image+pic: MediaTypes = %v, want [image]", p.MediaTypes)
	}
	if p := Parse("type:img+bogus"); len(p.MediaTypes) != 0 {
		t.Errorf("type:img+bogus: MediaTypes = %v, want empty (token rejected)", p.MediaTypes)
	}
	if got := Parse("type:gif+vid").Canonical(); got != "type:gif+video" {
		t.Errorf("Canonical(type:gif+vid) = %q, want 'type:gif+video'", got)
	}
}

func TestParseMediaExcludes(t *testing.T) {
	if p := Parse("type:-gif"); !reflect.DeepEqual(p.MediaTypes, []string{"image", "video"}) {
		t.Errorf("type:-gif: MediaTypes = %v, want [image video]", p.MediaTypes)
	}
	if p := Parse("type:-gif-vid"); !reflect.DeepEqual(p.MediaTypes, []string{"image"}) {
		t.Errorf("type:-gif-vid: MediaTypes = %v, want [image]", p.MediaTypes)
	}
	if p := Parse("type:img-gif"); !reflect.DeepEqual(p.MediaTypes, []string{"image"}) {
		t.Errorf("type:img-gif: MediaTypes = %v, want [image]", p.MediaTypes)
	}
	if p := Parse("type:img+vid-vid"); !reflect.DeepEqual(p.MediaTypes, []string{"image"}) {
		t.Errorf("type:img+vid-vid: MediaTypes = %v, want [image]", p.MediaTypes)
	}
	if p := Parse("type:-img-vid-gif"); len(p.MediaTypes) != 0 {
		t.Errorf("type:-img-vid-gif: MediaTypes = %v, want empty (token rejected)", p.MediaTypes)
	}
	if got := Parse("type:-gif").Canonical(); got != "type:image+video" {
		t.Errorf("Canonical(type:-gif) = %q, want 'type:image+video'", got)
	}
}

func TestParseMode(t *testing.T) {
	// mode:raw / mode:instant flip the Instant flag.
	for _, in := range []string{"mode:raw", "mode:instant", "mode:ins"} {
		if p := Parse(in); !p.Instant {
			t.Errorf("%q: Instant=false, want true", in)
		}
	}
	// mode:full is the explicit default — accepted but no-op.
	if p := Parse("mode:full"); p.Instant {
		t.Errorf("mode:full: Instant=true, want false")
	}
	// Unknown mode value falls back to free text.
	if p := Parse("mode:bogus"); len(p.Terms) != 1 || p.Terms[0] != "mode:bogus" {
		t.Errorf("mode:bogus terms=%v, want [mode:bogus]", p.Terms)
	}
	// Canonical emits mode:raw when Instant is set.
	if got := Parse("type:video mode:raw").Canonical(); got != "type:video mode:raw" {
		t.Errorf("Canonical = %q, want 'type:video mode:raw'", got)
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

func TestParseAfterBeforeBackcompat(t *testing.T) {
	p := Parse("kernel after:2024-01-01 before:2024-06-30")
	if p.After == nil || !p.After.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("After = %v", p.After)
	}
	if p.Before == nil || !p.Before.Equal(time.Date(2024, 6, 30, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("Before = %v", p.Before)
	}
}

func TestParseDateInequalities(t *testing.T) {
	// date>X uses an inequality, so no colon (per the rule).
	p := Parse("date>2024-01-02 date<2024-12-30")
	if p.After == nil || !p.After.Equal(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("After = %v", p.After)
	}
	if p.Before == nil || !p.Before.Equal(time.Date(2024, 12, 30, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("Before = %v", p.Before)
	}
}

func TestParseDateRangeEquality(t *testing.T) {
	// `date:2024` expands to a full-year After/Before window.
	p := Parse("date:2024")
	if p.After == nil || !p.After.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("After = %v, want 2024-01-01", p.After)
	}
	if p.Before == nil || !p.Before.Equal(time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("Before = %v, want 2024-12-31", p.Before)
	}
	// `date:2024-06` expands to a full-month window.
	p = Parse("date:2024-06")
	if p.After == nil || !p.After.Equal(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month After = %v", p.After)
	}
	if p.Before == nil || !p.Before.Equal(time.Date(2024, 6, 30, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("month Before = %v", p.Before)
	}
}

func TestParseDateTimeframeKeyword(t *testing.T) {
	// date:week sets Timeframe (for Reddit ?t=) and the archive helper derives After.
	defer pinNow(time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC))()
	p := Parse("date:week")
	if p.Timeframe != "week" {
		t.Errorf("Timeframe = %q, want week", p.Timeframe)
	}
	if p.After != nil {
		t.Errorf("After = %v, want nil (Timeframe carries the cutoff)", p.After)
	}
	got := p.ArchiveAfter()
	want := time.Date(2024, 6, 8, 12, 0, 0, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Errorf("ArchiveAfter = %v, want %v", got, want)
	}
}

func TestParseDateRelativeOffset(t *testing.T) {
	defer pinNow(time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC))()
	p := Parse("date>7d")
	want := time.Date(2024, 6, 8, 12, 0, 0, 0, time.UTC)
	if p.After == nil || !p.After.Equal(want) {
		t.Errorf("date>7d After = %v, want %v", p.After, want)
	}
	if p.Before != nil {
		t.Errorf("date>7d Before = %v, want nil", p.Before)
	}
}

func TestUnknownKeyStaysText(t *testing.T) {
	p := Parse("foo:bar baz")
	want := []string{"foo:bar", "baz"}
	if !reflect.DeepEqual(p.Terms, want) {
		t.Errorf("Terms = %v, want %v", p.Terms, want)
	}
}

func TestEmptyQuery(t *testing.T) {
	p := Parse("   ")
	if len(p.Terms) != 0 || p.RedditQuery() != "" {
		t.Errorf("empty query not empty: %+v", p)
	}
}

func TestSortShim(t *testing.T) {
	cases := []struct {
		sort       string
		wantSearch string
		wantSub    string
		wantArch   string
	}{
		{"top", "top", "top", "top"},
		{"new", "new", "new", "new"},
		{"hot", "relevance", "hot", "new"},
		{"rising", "relevance", "rising", "new"},
		{"relevance", "relevance", "hot", "new"},
		{"comments", "comments", "hot", "top"},
		{"controversial", "top", "controversial", "top"},
		{"all", "", "", "all"},
		{"", "", "", ""},
		{"bogus", "", "", ""}, // never set; ought to round-trip as empty
	}
	for _, c := range cases {
		p := Parsed{Sort: c.sort}
		if got := p.SortForSearch(); got != c.wantSearch {
			t.Errorf("Sort=%q SortForSearch=%q want %q", c.sort, got, c.wantSearch)
		}
		if got := p.SortForSub(); got != c.wantSub {
			t.Errorf("Sort=%q SortForSub=%q want %q", c.sort, got, c.wantSub)
		}
		if got := p.SortForArchive(); got != c.wantArch {
			t.Errorf("Sort=%q SortForArchive=%q want %q", c.sort, got, c.wantArch)
		}
	}
}
