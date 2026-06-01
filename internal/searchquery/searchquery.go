// Package searchquery parses RedMemo's e621-style search box syntax into a
// neutral, typed form. The same Parsed value is translated two ways:
//
//   - RedditQuery() builds an upstream Reddit `q` string (Lucene operators);
//   - the handler maps the struct fields onto a PostgreSQL archive query.
//
// Free-text words match the title/full text; everything else is a key:value (or
// key<op>value) constraint. Tokens may appear in any order. See
// docs/reddit-search.md for the full specification.
package searchquery

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NumOp is a numeric comparison operator for score/comments constraints.
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
	// Instant is set by an `ins`/`instant` segment inside a `t:` value (e.g.
	// `t:ins`, `t:ins+vid`). It is not a media type but a sibling flag asking
	// /random to return the matched post's resource (cached media bytes via
	// redirect, or the post's text body) instead of a JSON envelope. A signed
	// exclude (`-ins`/`-instant`) is meaningless and silently dropped without
	// rejecting the surrounding token.
	Instant bool
	Score     *NumConstraint
	Comments  *NumConstraint
	// CacheScore filters by the media cache *eviction* score (media_content.score,
	// 0–100, higher = evicted sooner), NOT the Reddit post score. It is meaningful
	// only for locally-cached media, so it is honored solely by the offline
	// archive/random paths and silently dropped on live Reddit search (it never
	// reaches RedditQuery or the live post-filter — see HasLocalFilter).
	CacheScore *NumConstraint
	After     *time.Time // created on/after (UTC midnight)
	Before    *time.Time // created on/before (UTC end-of-day)

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
//   - `score`/`upvote`/`upvotes`/`ups`/`u` → Reddit post score (online + offline)
//   - `cache_score` → media cache eviction score (offline archive/random only)
//   - `comments`/`comment`/`c` → comment count
var numKeyRe = regexp.MustCompile(`^(?i)(cache_score|score|upvotes|upvote|ups|u|comments|comment|c)(>=|<=|>|<|=|:)(-?\d+)$`)

// kvRe splits a generic key:value (or key=value) token. The value may be quoted.
var kvRe = regexp.MustCompile(`^(?i)([a-z_]+)[:=](.+)$`)

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
			case "comments", "comment", "c":
				p.Comments = nc
			case "cache_score":
				p.CacheScore = nc
			default: // score / upvote(s) / ups / u
				p.Score = nc
			}
			continue
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
	case "sub", "subreddit", "sr", "s":
		// Greedy: one sub: token may carry many +include / -exclude names, e.g.
		// `sub:golang+rust+python` or `sub:-sfw-meta`. Names are resolved
		// globally (last-wins) after the whole box is parsed — see resolveSubs.
		p.subEntries = append(p.subEntries, splitSignedSubs(val)...)
	case "author", "user", "a":
		p.Author = strings.TrimPrefix(strings.TrimPrefix(val, "u/"), "/u/")
	case "flair", "flair_name", "f":
		p.Flair = val
	case "type", "media", "t":
		// Allow multiple types joined by '+' (e.g. `t:gif+vid`, `type:img+vid+gif`).
		// Each segment is normalized to one of {image,video,gif}; unknown segments
		// reject the whole token so it falls back to free text.
		// Segments may be signed: `+x` (or implicit) includes, `-x` excludes.
		// With no includes the base is the full set {image,video,gif}; any
		// excludes then subtract from it (e.g. `t:-gif` = image+video). With
		// includes present, excludes subtract from the include set. One bad
		// segment rejects the whole token so it falls back to free text
		// without partially committing the good ones. An empty final set also
		// rejects (e.g. `t:-img-vid-gif`).
		fresh, instant, ok := parseSignedMediaTypes(val)
		if !ok {
			return false
		}
		if instant {
			p.Instant = true
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
	case "after", "since":
		if t := parseDate(val, false); t != nil {
			p.After = t
		} else {
			return false
		}
	case "before", "until":
		if t := parseDate(val, true); t != nil {
			p.Before = t
		} else {
			return false
		}
	default:
		return false
	}
	return true
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
// A bare `t:-gif` exclude with no includes starts from this set.
var allMediaTypes = []string{"image", "video", "gif"}

// parseSignedMediaTypes resolves a `t:`/`type:`/`media:` value with optional
// `+`/`-` segment signs into an ordered, deduped subset of allMediaTypes. The
// second return is false when any segment is an unknown alias or when the
// resolved set is empty (so the caller can fall back to free text).
func parseSignedMediaTypes(val string) ([]string, bool, bool) {
	includes := make(map[string]bool, 3)
	excludes := make(map[string]bool, 3)
	includeOrder := make([]string, 0, 3)
	instant := false
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
		// `ins`/`instant` is an output-mode flag, not a media kind. Include sets
		// the flag; a signed exclude has no meaning and is silently dropped (it
		// must not reject the surrounding token or land in free text).
		if low := strings.ToLower(seg); low == "ins" || low == "instant" {
			if include {
				instant = true
			}
			continue
		}
		norm := mediaCanonical(seg)
		if norm == "" {
			return nil, false, false
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
	// A bare `t:ins` (instant flag, no media constraint) is valid — accept it
	// with an empty media-type set. An empty result that is ALSO missing the
	// instant flag (e.g. `t:-img-vid-gif`) is still a rejection.
	if len(out) == 0 && !instant {
		return nil, false, false
	}
	return out, instant, true
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

// subKeyPrefixRe matches a leading sub-key (sub:/s:/sr:/subreddit:, = form too)
// so ParseSubList can tolerate a pasted grammar token in the simple NP field.
var subKeyPrefixRe = regexp.MustCompile(`(?i)^(?:subreddit|sub|sr|s)[:=]`)

// SubClause renders the resolved subreddit scope back into a single canonical
// `sub:` token — includes first (a+b), then excludes (-c) — or "" when no subs
// were given. The homepage feed honors only a query's sub: clause, so the
// settings page stores and echoes back exactly this "accepted" form, letting
// the Go backend (not JS) decide what survives normalization.
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

// Canonical re-serializes the whole parsed query back into the search-box
// grammar (sub: clause first, then the remaining constraints, then free text),
// producing a string that Parse round-trips. Unlike SubClause it preserves every
// constraint — author, flair, rating, media type, score/comments thresholds and
// date bounds — so the homepage filter can store and echo the full query the
// backend honors, not just its subreddit scope. An all-empty Parsed yields "".
func (p Parsed) Canonical() string {
	var parts []string
	if c := p.SubClause(); c != "" {
		parts = append(parts, c)
	}
	if p.Author != "" {
		parts = append(parts, "a:"+p.Author)
	}
	if p.Flair != "" {
		parts = append(parts, "f:"+quoteIfSpace(p.Flair))
	}
	switch p.Rating {
	case "nsfw":
		parts = append(parts, "r:nsfw")
	case "safe":
		parts = append(parts, "r:safe")
	}
	if len(p.MediaTypes) > 0 || p.Instant {
		segs := make([]string, 0, len(p.MediaTypes)+1)
		if p.Instant {
			segs = append(segs, "ins")
		}
		segs = append(segs, p.MediaTypes...)
		parts = append(parts, "t:"+strings.Join(segs, "+"))
	}
	if p.Score != nil {
		parts = append(parts, "score"+p.Score.SQLOp()+strconv.Itoa(p.Score.Val))
	}
	if p.CacheScore != nil {
		parts = append(parts, "cache_score"+p.CacheScore.SQLOp()+strconv.Itoa(p.CacheScore.Val))
	}
	if p.Comments != nil {
		parts = append(parts, "c"+p.Comments.SQLOp()+strconv.Itoa(p.Comments.Val))
	}
	if p.After != nil {
		parts = append(parts, "after:"+p.After.Format("2006-01-02"))
	}
	if p.Before != nil {
		parts = append(parts, "before:"+p.Before.Format("2006-01-02"))
	}
	for _, t := range p.Terms {
		parts = append(parts, quoteIfSpace(t))
	}
	return strings.Join(parts, " ")
}

// ParseSubList reads the simple "a+b+c" Natural-Prefetch filter format into an
// ordered, deduped, lowercased list of subreddit names. Unlike the full search
// grammar every token is a subreddit: '+', '-' and whitespace all separate
// names, a leading sub:/s:/sr:/subreddit: key or r/ prefix is stripped, and
// signs are discarded (NP only crawls includes). Subreddit names never contain
// '-', so splitting on it is safe.
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
// applied as a local post-filter instead — see docs/reddit-search.md §2.3.
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

// HasLocalFilter reports whether any constraint must be enforced client-side on
// live Reddit results (Reddit can't express these in `q`).
//
// MediaTypes and CacheScore are intentionally excluded. Reddit's search can't
// return a specific media kind, so the live post-filter lets every type through
// and silently drops the type:video/image constraint rather than emptying the
// page. CacheScore filters by the *local* media cache eviction score, which has
// no meaning for live Reddit results at all — there is no cached file to score —
// so it too is silently dropped online and honored only by the offline
// archive/random paths. Both constraints are still parsed (and CacheScore is
// honored locally via Proxy.MediaScore); they are just not enforced over live
// results.
func (p Parsed) HasLocalFilter() bool {
	return p.Score != nil || p.Comments != nil ||
		p.After != nil || p.Before != nil
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
