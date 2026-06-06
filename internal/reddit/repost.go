package reddit

import (
	"sort"
	"strconv"
	"strings"
)

// minRepostTitleLen is the title-length floor below which the SQL repost_key
// generated column emits NULL (no exact-string folding at the database
// layer). The in-memory Jaccard clustering uses a token-count floor instead
// (minClusterTokens) so the two layers don't have to agree on the same
// character length.
const minRepostTitleLen = 12

// minClusterTokens is the minimum number of distinctive tokens (post-stopword,
// post-length-filter) a title must produce before FoldReposts will consider
// it for clustering. Below this floor titles like "rule" / "python" / "M4M"
// would only be one or two tokens — too thin a fingerprint, and bucketing
// them would collide unrelated content.
const minClusterTokens = 3

// jaccardThreshold is the token-set similarity floor for treating two titles
// as the same content. Calibrated on a real spam page:
//   - typo variants ("knaughty" vs "knaughtty")            ~0.94
//   - prefix swaps ("[Encouragement]" vs "[Gay Linux Porn]") ~0.90
//   - word-order with tag drops ("Musk Python Golang" vs "Python Comic") ~0.75
//   - cross-sub crossposts with minor formatting drift             ~0.80
// 0.72 catches the word-order + tag-drop case while sitting comfortably
// above the topic-only similarity of legitimately distinct posts that share
// the query terms (typically ~0.15 — 0.30 since two random caption posts
// agree mainly on "gay/linux/python/porn" and otherwise diverge).
const jaccardThreshold = 0.72

// JaccardThreshold exposes the cluster similarity cutoff so callers in other
// packages (notably the cross-page session dedup) can share the same value
// without re-declaring it.
func JaccardThreshold() float64 { return jaccardThreshold }

// RepostKey returns the SQL-side normalization used by the v30 generated
// column: whitespace-collapsed lowercase title with a length floor. The
// in-memory FoldReposts path does NOT call this — it runs token-set Jaccard
// clustering for fuzzy similarity. RepostKey is kept exposed so the storage
// layer and any future exact-match code paths share one definition.
//
// The first parameter is reserved for future per-author keying; it's
// currently ignored so the SQL column (title-only) matches.
func RepostKey(_unused, title string) string {
	return NormalizeTitle(title)
}

// NormalizeTitle is RepostKey's title-only normalization, exposed for
// callers that want the literal pre-clustering key without the length-floor
// guard logic.
func NormalizeTitle(title string) string {
	t := strings.ToLower(strings.Join(strings.Fields(title), " "))
	if len(t) < minRepostTitleLen {
		return ""
	}
	return t
}

// TitleTokens produces the sorted, deduplicated, stopword-filtered token
// list used for Jaccard similarity clustering. Punctuation is mapped to
// spaces, the result is lowercased, tokens shorter than 3 chars and common
// English stopwords are dropped. The returned slice is sorted so callers
// can do set operations via a single linear merge.
//
// Returns nil when the post-filter set is below minClusterTokens — the
// caller treats that as "don't cluster, render as-is".
func TitleTokens(title string) []string {
	if title == "" {
		return nil
	}
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte(' ')
		}
	}
	fields := strings.Fields(b.String())
	if len(fields) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 3 {
			continue
		}
		if _, drop := repostStopwords[f]; drop {
			continue
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	if len(out) < minClusterTokens {
		return nil
	}
	sort.Strings(out)
	return out
}

// repostStopwords are tokens too generic to identify a piece of content.
// Every common English function word a Reddit title is likely to carry,
// plus a few Reddit-specific filler markers ("post", "comment"). Kept
// modest: removing too many words leaves only the topic tokens, which then
// collide across unrelated posts on the same topic.
var repostStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "you": {}, "are": {}, "was": {},
	"with": {}, "that": {}, "this": {}, "have": {}, "has": {}, "but": {},
	"not": {}, "can": {}, "all": {}, "got": {}, "why": {}, "what": {},
	"who": {}, "how": {}, "when": {}, "where": {}, "his": {}, "her": {},
	"she": {}, "him": {}, "they": {}, "them": {}, "their": {}, "our": {},
	"its": {}, "into": {}, "from": {}, "more": {}, "most": {}, "some": {},
	"any": {}, "will": {}, "would": {}, "could": {}, "should": {},
	"just": {}, "like": {}, "only": {}, "also": {}, "very": {}, "much": {},
	"many": {}, "one": {}, "two": {}, "out": {}, "off": {}, "now": {},
	"new": {}, "old": {}, "yes": {}, "let": {}, "may": {},
	"see": {}, "way": {}, "use": {}, "did": {}, "had": {},
	"hey": {}, "ive": {}, "youre": {}, "dont": {},
	"yours": {}, "mine": {}, "ours": {}, "ourselves": {},
	"theirs": {}, "themselves": {}, "myself": {}, "yourself": {},
	"himself": {}, "herself": {}, "itself": {},
	"post": {}, "comment": {}, "reddit": {}, "sub": {}, "subreddit": {},
}

// JaccardTokens returns the Jaccard similarity (|A∩B|/|A∪B|) of two SORTED
// deduplicated token slices. Both inputs must come from TitleTokens (or
// equivalently normalized) — the merge assumes sorted order. Returns 0 on
// either-empty input so a missing-tokens post never folds into a cluster.
func JaccardTokens(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	i, j, inter := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			inter++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// PrimaryMediaKey returns a stable identity for the post's primary media
// resource — the post's main media URL, or the first gallery image URL.
// Returns "" for text-only posts (selftext, link cards without preview).
// The Reddit URL is used in its canonical UnformatURL form so two paths
// pointing at the same upstream asset compare equal regardless of CDN
// host / size suffix variation. This is the key dedup logic consults to
// decide "same content?" — two posts with similar titles but different
// media keys are different content (a real distinct repost or unrelated
// item that happens to share words), and must NOT be folded together.
func PrimaryMediaKey(p *Post) string {
	if p.Media.URL != "" {
		return UnformatURL(p.Media.URL)
	}
	if len(p.Gallery) > 0 && p.Gallery[0].URL != "" {
		return UnformatURL(p.Gallery[0].URL)
	}
	return ""
}

// FoldReposts groups posts whose titles are token-set Jaccard-similar above
// jaccardThreshold AND whose primary media key (PrimaryMediaKey) matches
// into single cluster cards. The media check is the safety valve for
// title-only similarity: two posts that read alike but point at different
// images/videos are genuinely different content (real reposts of two
// separate items, or unrelated posts that happen to share generic words)
// and stay as separate cards. Text-only posts (empty media key on both
// sides) still fold by title alone — there is no media to disambiguate.
//
// The surviving "head" of each cluster is the highest-scoring member;
// every variant (including the head itself) is recorded in the head's
// RepostMembers so the renderer can show per-sub navigation rows around
// the single shared media element. Posts whose TitleTokens floors out
// (too few distinctive tokens) are kept as singleton clusters and never
// absorbed.
//
// Survivor order matches first-appearance in the input — no re-sorting —
// so the caller's pagination / relevance order is preserved.
func FoldReposts(posts []Post) []Post {
	if len(posts) <= 1 {
		if len(posts) == 1 {
			posts[0].RepostCount = 1
			posts[0].RepostMembers = []RepostMember{memberFromPost(posts[0])}
		}
		return posts
	}

	type cluster struct {
		head     int      // index in posts whose card represents this cluster
		tokens   []string // token set of the current head (used for matching)
		mediaKey string   // primary media URL of the head; "" for text-only
		members  []RepostMember
	}
	clusters := make([]cluster, 0, len(posts))
	// clusterOf[i] = index in `clusters` for posts[i], or -1 if no cluster
	// (e.g. token-floored post that becomes a singleton). isHead[i] flags
	// the indices we emit; it changes when a higher-scoring variant arrives
	// and steals the head slot.
	clusterOf := make([]int, len(posts))
	isHead := make([]bool, len(posts))

	for i := range posts {
		tokens := TitleTokens(posts[i].Title)
		mk := PrimaryMediaKey(&posts[i])
		matched := -1
		if tokens != nil {
			for ci := range clusters {
				if clusters[ci].tokens == nil {
					continue
				}
				// Both title-similar AND media-equal. Different media ⇒
				// different content, even if titles are word-for-word
				// identical. A genuine repost of the SAME media will
				// share PrimaryMediaKey (Reddit echoes the original CDN
				// URL for crossposts and image-mirror reposts).
				if clusters[ci].mediaKey != mk {
					continue
				}
				if JaccardTokens(tokens, clusters[ci].tokens) >= jaccardThreshold {
					matched = ci
					break
				}
			}
		}
		if matched < 0 {
			clusterOf[i] = len(clusters)
			clusters = append(clusters, cluster{
				head:     i,
				tokens:   tokens,
				mediaKey: mk,
				members:  []RepostMember{memberFromPost(posts[i])},
			})
			isHead[i] = true
			continue
		}
		clusterOf[i] = matched
		clusters[matched].members = append(clusters[matched].members, memberFromPost(posts[i]))
		if scoreGT(posts[i].Score[1], posts[clusters[matched].head].Score[1]) {
			isHead[clusters[matched].head] = false
			clusters[matched].head = i
			clusters[matched].tokens = tokens
			// mediaKey stays the same — head swap only happens when the
			// new candidate's mediaKey already matched the cluster's.
			isHead[i] = true
		}
	}

	out := posts[:0]
	for i := range posts {
		if !isHead[i] {
			continue
		}
		c := clusters[clusterOf[i]]
		posts[i].RepostCount = len(c.members)
		posts[i].RepostMembers = c.members
		out = append(out, posts[i])
	}
	return out
}

func memberFromPost(p Post) RepostMember {
	return RepostMember{
		Community: p.Community,
		Permalink: p.Permalink,
		Title:     p.Title,
		Author:    p.Author,
		Score:     p.Score,
		RelTime:   p.RelTime,
		Created:   p.Created,
	}
}

// scoreGT reports whether raw score string a represents a strictly larger
// integer than b. Non-numeric values sort as smaller than any real number,
// so a valid score always beats an empty / malformed one.
func scoreGT(a, b string) bool {
	ai, aok := parseScore(a)
	bi, bok := parseScore(b)
	switch {
	case aok && !bok:
		return true
	case !aok:
		return false
	}
	return ai > bi
}

func parseScore(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
