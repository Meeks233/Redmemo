// Package searchquery parses RedMemo's e621-style search box syntax into a
// neutral, typed form. The same Parsed value is translated three ways:
//
//   - RedditQuery() builds an upstream Reddit `q` string (Lucene operators);
//   - SortForSearch/SortForSub/SortForArchive translate the user's free-form
//     sort word into the closest match each backend accepts;
//   - the handler maps the struct fields onto a PostgreSQL archive query.
//
// Grammar rules:
//   - Free-text words match the title/full text.
//   - `key:value` is equality (and ranges, for date: only).
//   - `key>value` / `key<value` / `key>=` / `key<=` are inequalities and never
//     take a colon (e.g. `score>100`, `date<2024-12-31`).
//   - Unknown keys fall back to free text.
//
// One canonical key + at most one short alias per concept; ambiguous single-
// letter aliases (s/a/c/f) are gone.
package searchquery

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// nowFn is the wall clock used by relative-date parsing. Production callers
// see time.Now(); tests can pin it for deterministic output.
var nowFn = time.Now

// NumOp is a numeric comparison operator for score/comments/cached constraints.
type NumOp string

const (
	OpGT NumOp = ">"
	OpLT NumOp = "<"
	OpGE NumOp = ">="
	OpLE NumOp = "<="
	OpEQ NumOp = "="
)

// NumConstraint is a parsed numeric threshold, e.g. `score>100`.
type NumConstraint struct {
	Op  NumOp
	Val int
}

// Match reports whether v satisfies the constraint.
func (n NumConstraint) Match(v int) bool {
	switch n.Op {
	case OpGT:
		return v > n.Val
	case OpLT:
		return v < n.Val
	case OpGE:
		return v >= n.Val
	case OpLE:
		return v <= n.Val
	default: // OpEQ
		return v == n.Val
	}
}

// MatchFloat reports whether v satisfies the constraint, comparing as floats.
// Used for the media cache eviction score, which is a continuous value.
func (n NumConstraint) MatchFloat(v float64) bool {
	t := float64(n.Val)
	switch n.Op {
	case OpGT:
		return v > t
	case OpLT:
		return v < t
	case OpGE:
		return v >= t
	case OpLE:
		return v <= t
	default: // OpEQ
		return v == t
	}
}

// SQLOp returns the PostgreSQL comparison operator for this constraint.
func (n NumConstraint) SQLOp() string { return string(n.Op) }

// Parsed is the neutral result of parsing a query box. Zero-value fields mean
// "no constraint" so an empty Parsed matches everything.
type Parsed struct {
	Terms     []string // free-text words/phrases
	Rating    string   // "nsfw" | "safe" | ""
	WhiteSubs []string // whitelist subreddits (lowercased, deduped)
	BlackSubs []string // blacklist subreddits (lowercased, deduped)
	Author    string   // author filter
	Flair     string   // flair text filter
	MediaTypes []string // ordered, deduped subset of {"image","video","gif"}; nil = any
	// Instant is set by `mode:raw` (or `mode:instant`). Not a media type but a
	// sibling flag asking /random to return the matched post's resource (cached
	// media bytes via redirect, or the post's text body) instead of a JSON
	// envelope.
	Instant bool
	Score     *NumConstraint
	Comments  *NumConstraint
	// CacheScore filters by the media cache *eviction* score (media_content.score,
	// 0–100, higher = evicted sooner), NOT the Reddit post score. It is meaningful
	// only for locally-cached media, so it is honored solely by the offline
	// archive/random paths and silently dropped on live Reddit search.
	CacheScore *NumConstraint
	After     *time.Time // created on/after (UTC midnight)
	Before    *time.Time // created on/before (UTC end-of-day)
	// Timeframe is Reddit's relative window keyword (hour|day|week|month|year|
	// all) — forwarded verbatim to `/search.json?t=` and `/r/X/{sort}.json?t=`.
	// Populated by `date:<keyword>`; the archive path collapses it to a
	// concrete After via ArchiveAfter().
	Timeframe string
	// Sort is the user's free-form sort word, lowercased, accepted from the
	// full union of (relevance|hot|top|new|comments|rising|controversial). The
	// SortFor* helpers translate it to the closest word each backend accepts.
	Sort string

	// subEntries accumulates every signed subreddit from all sub: tokens during
	// parsing; resolveSubs() collapses them (last-wins per name) into the
	// WhiteSubs/BlackSubs slices once the whole box has been read.
	subEntries []signedSub
}

// signedSub is one subreddit name with its include/exclude intent, captured from
// a `sub:` value like `golang+rust` (include) or `-sfw` (exclude).
type signedSub struct {
	name    string
	include bool
}

// numKeyRe splits a numeric constraint token: key, operator, integer value.
// The colon form (score:100) is treated as equality. Three distinct targets:
//   - `score`/`ups` → Reddit post score (online + offline)
//   - `cached` → media cache eviction score (offline archive/random only)
//   - `comments` → comment count
var numKeyRe = regexp.MustCompile(`^(?i)(cached|score|ups|comments)(>=|<=|>|<|=|:)(-?\d+)$`)

// dateOpRe splits a `date<>/<=/>=` inequality token. Equality forms
// (`date:2024-06`, `date:week`, `date=2024-06`) go through kvRe instead.
var dateOpRe = regexp.MustCompile(`^(?i)date(>=|<=|>|<)(.+)$`)

// kvRe splits a generic key:value (or key=value) token. The value may be quoted.
var kvRe = regexp.MustCompile(`^(?i)([a-z]+)[:=](.+)$`)

// Parse turns a raw query box string into a Parsed value.
func Parse(raw string) Parsed {
	var p Parsed
	for _, tok := range tokenize(raw) {
		if tok == "" {
			continue
		}
		if m := numKeyRe.FindStringSubmatch(tok); m != nil {
			n, err := strconv.Atoi(m[3])
			if err != nil {
				p.Terms = append(p.Terms, tok)
				continue
			}
			op := NumOp(m[2])
			if op == ":" {
				op = OpEQ
			}
			nc := &NumConstraint{Op: op, Val: n}
			switch strings.ToLower(m[1]) {
			case "comments":
				p.Comments = nc
			case "cached":
				p.CacheScore = nc
			default: // score / ups
				p.Score = nc
			}
			continue
		}
		if m := dateOpRe.FindStringSubmatch(tok); m != nil {
			if applyDateOp(&p, m[1], unquote(m[2])) {
				continue
			}
			// fall through to free text on bad value
		}
		if m := kvRe.FindStringSubmatch(tok); m != nil {
			if p.applyKV(strings.ToLower(m[1]), unquote(m[2])) {
				continue
			}
		}
		// Plain free-text term (quotes stripped).
		if t := unquote(tok); t != "" {
			p.Terms = append(p.Terms, t)
		}
	}
	p.resolveSubs()
	return p
}

// applyKV applies a key:value constraint, returning false if the key is unknown
// (so the caller can keep the token as free text).
func (p *Parsed) applyKV(key, val string) bool {
	if val == "" {
		return false
	}
	switch key {
	case "rating", "r":
		switch strings.ToLower(val) {
		case "nsfw", "explicit", "e", "x":
			p.Rating = "nsfw"
		case "safe", "sfw":
			p.Rating = "safe"
		default:
			return false
		}
	case "sub", "sr":
		// Greedy: one sub: token may carry many +include / -exclude names, e.g.
		// `sub:golang+rust+python` or `sub:-sfw-meta`. Names are resolved
		// globally (last-wins) after the whole box is parsed — see resolveSubs.
		p.subEntries = append(p.subEntries, splitSignedSubs(val)...)
	case "author", "user":
		p.Author = strings.TrimPrefix(strings.TrimPrefix(val, "u/"), "/u/")
	case "flair":
		p.Flair = val
	case "type", "media":
		// Allow multiple types joined by '+' (e.g. `type:gif+vid`).
		// Each segment is normalized to one of {image,video,gif}; unknown segments
		// reject the whole token so it falls back to free text.
		// Segments may be signed: `+x` (or implicit) includes, `-x` excludes.
		// With no includes the base is the full set {image,video,gif}; any
		// excludes then subtract from it. One bad segment rejects the whole
		// token. An empty final set also rejects.
		fresh, ok := parseSignedMediaTypes(val)
		if !ok {
			return false
		}
		seen := make(map[string]bool, len(p.MediaTypes)+len(fresh))
		for _, s := range p.MediaTypes {
			seen[s] = true
		}
		for _, s := range fresh {
			if !seen[s] {
				seen[s] = true
				p.MediaTypes = append(p.MediaTypes, s)
			}
		}
	case "mode":
		// mode:raw / mode:instant flips the /random instant flag; mode:full is
		// the explicit default (no-op). Anything else falls back to free text.
		switch strings.ToLower(val) {
		case "raw", "instant", "ins":
			p.Instant = true
		case "full", "json":
			p.Instant = false
		default:
			return false
		}
	case "sort", "order":
		// Accept any word from the union of valid sorts across all backends;
		// the per-context SortFor* helpers translate. Anything off the union
		// falls back to free text so a typo doesn't silently mis-sort.
		low := strings.ToLower(val)
		if !validAnySort[low] {
			return false
		}
		p.Sort = low
	case "date":
		return applyDateEq(p, val)
	case "after":
		if t := parseDate(val, false); t != nil {
			p.After = t
		} else if d, ok := parseRelative(val); ok {
			t := nowFn().Add(-d)
			p.After = &t
		} else {
			return false
		}
	case "before":
		if t := parseDate(val, true); t != nil {
			p.Before = t
		} else if d, ok := parseRelative(val); ok {
			t := nowFn().Add(-d)
			p.Before = &t
		} else {
			return false
		}
	default:
		return false
	}
	return true
}

// applyDateOp handles `date<X` / `date>X` / `date>=X` / `date<=X`. Inclusive
// on both sides: `date>X` = "on or after the start of X", `date<X` = "on or
// before the end of X". For single-day values start == end == midnight bounds;
// for year / year-month grains the start is the first instant and the end is
// the last second. Returns false on a bad value.
func applyDateOp(p *Parsed, op, val string) bool {
	after, before, ok := resolveDateValue(val)
	if !ok {
		return false
	}
	switch op {
	case ">", ">=":
		if after != nil {
			t := *after
			p.After = &t
		}
	case "<", "<=":
		if before != nil {
			t := *before
			p.Before = &t
		} else if after != nil { // relative offset has only start
			t := *after
			p.Before = &t
		}
	}
	return true
}

// applyDateEq handles `date:value` (and `date=value`): a relative keyword sets
// Timeframe (and the archive layer collapses it to After); an ISO range value
// sets both After and Before bounds.
func applyDateEq(p *Parsed, val string) bool {
	low := strings.ToLower(val)
	if validTimeframes[low] {
		p.Timeframe = low
		return true
	}
	after, before, ok := resolveDateValue(val)
	if !ok {
		return false
	}
	if after != nil {
		t := *after
		p.After = &t
	}
	if before != nil {
		t := *before
		p.Before = &t
	}
	return true
}

// resolveDateValue parses one date value into (start, end) bounds. ISO formats:
//
//	2024            → Jan 1 2024 00:00 .. Dec 31 2024 23:59:59
//	2024-06         → Jun 1 2024 00:00 .. Jun 30 2024 23:59:59
//	2024-06-15      → that day 00:00 .. that day 23:59:59
//
// Relative forms: 7d, 12h, 1y, 30m, 1mo — interpreted as `now - N`, returned
// as a single instant in `start` (end is nil) so the caller knows it is a
// point in time, not a range.
func resolveDateValue(val string) (after, before *time.Time, ok bool) {
	val = strings.TrimSpace(val)
	if val == "" {
		return nil, nil, false
	}
	if d, isRel := parseRelative(val); isRel {
		t := nowFn().Add(-d)
		return &t, nil, true
	}
	// YYYY-MM-DD
	if t, err := time.Parse("2006-01-02", val); err == nil {
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
		return &start, &end, true
	}
	// YYYY-MM
	if t, err := time.Parse("2006-01", val); err == nil {
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0).Add(-time.Second)
		return &start, &end, true
	}
	// YYYY
	if t, err := time.Parse("2006", val); err == nil {
		start := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(t.Year(), 12, 31, 23, 59, 59, 0, time.UTC)
		return &start, &end, true
	}
	return nil, nil, false
}

// relativeRe matches a relative offset like 7d, 12h, 1y, 1mo, 30m.
var relativeRe = regexp.MustCompile(`^(?i)(\d+)(mo|[smhdwy])$`)

// parseRelative parses a relative offset into a Duration.
func parseRelative(val string) (time.Duration, bool) {
	m := relativeRe.FindStringSubmatch(strings.TrimSpace(val))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "s":
		return time.Duration(n) * time.Second, true
	case "m":
		return time.Duration(n) * time.Minute, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	case "w":
		return time.Duration(n) * 7 * 24 * time.Hour, true
	case "mo":
		return time.Duration(n) * 30 * 24 * time.Hour, true
	case "y":
		return time.Duration(n) * 365 * 24 * time.Hour, true
	}
	return 0, false
}

// normalizeSub lowercases a subreddit name and strips a leading r/ or /r/.
func normalizeSub(raw string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(raw, "r/"), "/r/"))
}

// splitSignedSubs parses a greedy sub: value into signed names. A `+` (or the
// implicit leading name) marks an include; a `-` marks an exclude. Examples:
//
//	golang+rust+python -> +golang +rust +python
//	-sfw-meta           -> -sfw -meta
//	golang+-sfw           -> +golang -sfw
//
// The `/` in an r/<name> prefix is not a separator, so `r/golang` stays whole.
func splitSignedSubs(val string) []signedSub {
	var out []signedSub
	for i, n := 0, len(val); i < n; {
		include := true
		if val[i] == '+' || val[i] == '-' {
			include = val[i] == '+'
			i++
		}
		start := i
		for i < n && val[i] != '+' && val[i] != '-' {
			i++
		}
		if name := normalizeSub(val[start:i]); name != "" {
			out = append(out, signedSub{name: name, include: include})
		}
	}
	return out
}

// mediaCanonical normalizes a single media-type alias to one of
// {image,video,gif}, or returns "" for an unknown name.
func mediaCanonical(seg string) string {
	switch strings.ToLower(seg) {
	case "image", "img", "gallery", "pic":
		return "image"
	case "video", "vid":
		return "video"
	case "gif":
		return "gif"
	}
	return ""
}

// allMediaTypes is the closed universe of media-type tokens, in display order.
// A bare `type:-gif` exclude with no includes starts from this set.
var allMediaTypes = []string{"image", "video", "gif"}

// validAnySort is the union of every sort word any backend accepts. The
// parser only checks membership here; per-backend translation happens in
// SortForSearch/SortForSub/SortForArchive so a `sort:hot` on the search page
// becomes `relevance`, not a silent drop.
var validAnySort = map[string]bool{
	"relevance": true, "hot": true, "top": true, "new": true,
	"comments": true, "rising": true, "controversial": true, "all": true,
}

// validTimeframes is Reddit's `?t=` keyword set, accepted verbatim by
// `/search.json` and `/r/X/top.json` (and ignored by other sorts).
var validTimeframes = map[string]bool{
	"hour": true, "day": true, "week": true, "month": true, "year": true, "all": true,
}

// parseSignedMediaTypes resolves a `type:`/`media:` value with optional `+`/`-`
// segment signs into an ordered, deduped subset of allMediaTypes. The second
// return is false when any segment is an unknown alias or when the resolved
// set is empty (so the caller can fall back to free text).
func parseSignedMediaTypes(val string) ([]string, bool) {
	includes := make(map[string]bool, 3)
	excludes := make(map[string]bool, 3)
	includeOrder := make([]string, 0, 3)
	for i, n := 0, len(val); i < n; {
		include := true
		if val[i] == '+' || val[i] == '-' {
			include = val[i] == '+'
			i++
		}
		start := i
		for i < n && val[i] != '+' && val[i] != '-' {
			i++
		}
		seg := strings.TrimSpace(val[start:i])
		if seg == "" {
			continue
		}
		norm := mediaCanonical(seg)
		if norm == "" {
			return nil, false
		}
		if include {
			if !includes[norm] {
				includes[norm] = true
				includeOrder = append(includeOrder, norm)
			}
		} else {
			excludes[norm] = true
		}
	}
	base := includeOrder
	if len(base) == 0 && len(excludes) > 0 {
		// Bare exclude(s): start from the full universe.
		base = allMediaTypes
	}
	out := make([]string, 0, len(base))
	for _, t := range base {
		if !excludes[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// resolveSubs collapses every accumulated signed sub into WhiteSubs/BlackSubs.
// The same name appearing more than once is resolved last-wins (the final sign
// the user typed decides include vs exclude); first-seen order is preserved for
// deterministic output.
func (p *Parsed) resolveSubs() {
	if len(p.subEntries) == 0 {
		return
	}
	state := make(map[string]bool, len(p.subEntries))
	order := make([]string, 0, len(p.subEntries))
	for _, e := range p.subEntries {
		if _, seen := state[e.name]; !seen {
			order = append(order, e.name)
		}
		state[e.name] = e.include
	}
	for _, name := range order {
		if state[name] {
			p.WhiteSubs = append(p.WhiteSubs, name)
		} else {
			p.BlackSubs = append(p.BlackSubs, name)
		}
	}
	p.subEntries = nil
}

// subKeyPrefixRe matches a leading sub-key (sub:/sr:) so ParseSubList can
// tolerate a pasted grammar token in the simple NP field.
var subKeyPrefixRe = regexp.MustCompile(`(?i)^(?:sub|sr)[:=]`)

// SubClause renders the resolved subreddit scope back into a single canonical
// `sub:` token — includes first (a+b), then excludes (-c) — or "" when no subs
// were given.
func (p Parsed) SubClause() string {
	if len(p.WhiteSubs) == 0 && len(p.BlackSubs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("sub:")
	for i, s := range p.WhiteSubs {
		if i > 0 {
			b.WriteByte('+')
		}
		b.WriteString(s)
	}
	for _, s := range p.BlackSubs {
		b.WriteByte('-')
		b.WriteString(s)
	}
	return b.String()
}

// numClause formats a NumConstraint back into its grammar form: equality uses
// `:` (the rule "no inequality → no colon, inequality → no colon"), comparisons
// drop the colon.
func numClause(key string, nc *NumConstraint) string {
	if nc.Op == OpEQ {
		return key + ":" + strconv.Itoa(nc.Val)
	}
	return key + nc.SQLOp() + strconv.Itoa(nc.Val)
}

// Canonical re-serializes the whole parsed query back into the search-box
// grammar. Round-trips through Parse to itself.
func (p Parsed) Canonical() string {
	var parts []string
	if c := p.SubClause(); c != "" {
		parts = append(parts, c)
	}
	if p.Author != "" {
		parts = append(parts, "author:"+p.Author)
	}
	if p.Flair != "" {
		parts = append(parts, "flair:"+quoteIfSpace(p.Flair))
	}
	switch p.Rating {
	case "nsfw":
		parts = append(parts, "rating:nsfw")
	case "safe":
		parts = append(parts, "rating:safe")
	}
	if len(p.MediaTypes) > 0 {
		parts = append(parts, "type:"+strings.Join(p.MediaTypes, "+"))
	}
	if p.Instant {
		parts = append(parts, "mode:raw")
	}
	if p.Score != nil {
		parts = append(parts, numClause("score", p.Score))
	}
	if p.CacheScore != nil {
		parts = append(parts, numClause("cached", p.CacheScore))
	}
	if p.Comments != nil {
		parts = append(parts, numClause("comments", p.Comments))
	}
	if p.Sort != "" {
		parts = append(parts, "sort:"+p.Sort)
	}
	if p.Timeframe != "" {
		parts = append(parts, "date:"+p.Timeframe)
	}
	if p.After != nil {
		parts = append(parts, "date>"+p.After.Format("2006-01-02"))
	}
	if p.Before != nil {
		parts = append(parts, "date<"+p.Before.Format("2006-01-02"))
	}
	for _, t := range p.Terms {
		parts = append(parts, quoteIfSpace(t))
	}
	return strings.Join(parts, " ")
}

// ParseSubList reads the simple "a+b+c" Natural-Prefetch filter format into an
// ordered, deduped, lowercased list of subreddit names.
func ParseSubList(raw string) []string {
	seen := make(map[string]bool)
	var out []string
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '+' || r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	for _, f := range fields {
		f = subKeyPrefixRe.ReplaceAllString(f, "")
		name := normalizeSub(f)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// JoinSubs renders a name list as the "a+b+c" NP filter format.
func JoinSubs(subs []string) string { return strings.Join(subs, "+") }

// parseDate parses YYYY-MM-DD. endOfDay anchors a `before:` bound to 23:59:59.
func parseDate(val string, endOfDay bool) *time.Time {
	d, err := time.Parse("2006-01-02", val)
	if err != nil {
		return nil
	}
	if endOfDay {
		t := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
		return &t
	}
	t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	return &t
}

// RedditQuery builds the upstream Reddit `q` string. Constraints Reddit's API
// can't express (score, comments, media type, date range) are omitted here and
// applied as a local post-filter instead.
func (p Parsed) RedditQuery() string {
	var parts []string
	switch len(p.WhiteSubs) {
	case 0:
	case 1:
		parts = append(parts, "subreddit:"+p.WhiteSubs[0])
	default:
		ors := make([]string, len(p.WhiteSubs))
		for i, s := range p.WhiteSubs {
			ors[i] = "subreddit:" + s
		}
		parts = append(parts, "("+strings.Join(ors, " OR ")+")")
	}
	for _, s := range p.BlackSubs {
		parts = append(parts, "-subreddit:"+s)
	}
	if p.Author != "" {
		parts = append(parts, "author:"+p.Author)
	}
	if p.Flair != "" {
		parts = append(parts, "flair_name:"+quoteIfSpace(p.Flair))
	}
	switch p.Rating {
	case "nsfw":
		parts = append(parts, "nsfw:yes")
	case "safe":
		parts = append(parts, "nsfw:no")
	}
	for _, t := range p.Terms {
		parts = append(parts, quoteIfSpace(t))
	}
	return strings.Join(parts, " ")
}

// TextQuery returns the free-text portion (joined) for a Postgres title ILIKE.
func (p Parsed) TextQuery() string {
	return strings.Join(p.Terms, " ")
}

// --- per-backend sort translation ---------------------------------------

// SortForSearch translates p.Sort to a word `/search.json?sort=` accepts.
// Empty means "let Reddit pick its default".
func (p Parsed) SortForSearch() string {
	switch p.Sort {
	case "relevance", "top", "new", "comments":
		return p.Sort
	case "controversial":
		// /search.json has no controversial sort; "top" is the closest sibling
		// (both rank by raw vote totals rather than recency or relevance).
		return "top"
	case "hot", "rising":
		return "relevance"
	}
	return ""
}

// SortForSub translates p.Sort to a word `/r/X/<sort>.json` accepts.
// Empty means "let the caller's default win" (currently `hot`).
func (p Parsed) SortForSub() string {
	switch p.Sort {
	case "hot", "new", "top", "rising", "controversial":
		return p.Sort
	case "relevance", "comments":
		return "hot"
	}
	return ""
}

// SortForArchive translates p.Sort to a word the archive accepts
// (new/top/all). Empty means "the caller's default".
func (p Parsed) SortForArchive() string {
	switch p.Sort {
	case "new", "top", "all":
		return p.Sort
	case "hot", "rising", "relevance":
		return "new"
	case "comments", "controversial":
		return "top"
	}
	return ""
}

// ArchiveAfter returns the After bound to use against the archive: the
// explicit After if set; otherwise a derivation of Timeframe (which Reddit's
// `?t=` expresses but SQL needs as a wall-clock cutoff). Nil means "no lower
// bound".
func (p Parsed) ArchiveAfter() *time.Time {
	if p.After != nil {
		return p.After
	}
	if p.Timeframe == "" || p.Timeframe == "all" {
		return nil
	}
	now := nowFn()
	var d time.Duration
	switch p.Timeframe {
	case "hour":
		d = time.Hour
	case "day":
		d = 24 * time.Hour
	case "week":
		d = 7 * 24 * time.Hour
	case "month":
		d = 30 * 24 * time.Hour
	case "year":
		d = 365 * 24 * time.Hour
	default:
		return nil
	}
	t := now.Add(-d)
	return &t
}

// tokenize splits on whitespace while keeping double-quoted spans together. The
// surrounding quotes are preserved in the token so value parsing can detect and
// strip them.
func tokenize(s string) []string {
	var toks []string
	var b strings.Builder
	inQuote := false
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n' || r == '\r') && !inQuote:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return toks
}

// unquote strips a single pair of surrounding double quotes and trims spaces.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}

// quoteIfSpace wraps a value in double quotes when it contains whitespace, so
// multi-word Reddit operator values stay grouped.
func quoteIfSpace(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// pinNow pins the clock for tests. Returns a restore func.
func pinNow(t time.Time) func() {
	prev := nowFn
	nowFn = func() time.Time { return t }
	return func() { nowFn = prev }
}
