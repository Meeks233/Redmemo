package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

type PostStore struct {
	db *sql.DB
}

func NewPostStore(db *sql.DB) *PostStore {
	return &PostStore{db: db}
}

// nsfwExcludeSQL is the single source of truth for the WHERE fragment that
// drops NSFW (over_18) rows from a posts query. Callers append it conditionally
// based on the user's show_nsfw preference. Kept here so any future column
// rename or storage shape change touches one place.
const nsfwExcludeSQL = " AND COALESCE((json_data->>'over_18')::boolean, false) = false"

func (s *PostStore) Get(urlPath string) (*StoredPost, error) {
	p := &StoredPost{}
	err := s.db.QueryRow(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done,
		       upstream_removed
		FROM posts WHERE url_path = $1`, urlPath,
	).Scan(
		&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
		&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source, &p.MediaDone,
		&p.UpstreamRemoved,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get post: %w", err)
	}
	return p, nil
}

// GetByID resolves an archived post by its Reddit post ID, ignoring the
// permalink slug. Reddit addresses posts by ID alone — the trailing
// "/psa_oracle_is_changing.../" slug is cosmetic and Reddit happily serves the
// post for any slug (or none). A visitor arriving via a share link with a
// mangled, edited, or truncated slug therefore yields a url_path that never
// equals the stored permalink, so the exact-match Get misses. This ID-keyed
// fallback re-binds such requests to the archived row using idx_posts_post_id.
// Post IDs are globally unique on Reddit, so post_id alone is unambiguous;
// LIMIT 1 guards against the theoretical case of a duplicate row.
func (s *PostStore) GetByID(postID string) (*StoredPost, error) {
	p := &StoredPost{}
	err := s.db.QueryRow(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done,
		       upstream_removed
		FROM posts WHERE post_id = $1
		ORDER BY last_updated DESC
		LIMIT 1`, postID,
	).Scan(
		&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
		&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source, &p.MediaDone,
		&p.UpstreamRemoved,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get post by id: %w", err)
	}
	return p, nil
}

// MarkUpstreamRemoved flips the sticky upstream_removed verdict for a permalink.
// It is a no-op when the row is missing — the archive layer calls this only
// after confirming the row exists, so a row that disappeared between the read
// and write is treated as already-gone. last_updated bumps so the post page's
// freshness display reflects when we last *checked* upstream, even though we
// did not overwrite the JSON.
func (s *PostStore) MarkUpstreamRemoved(urlPath string) error {
	_, err := s.db.Exec(`
		UPDATE posts SET upstream_removed = TRUE, last_updated = NOW()
		WHERE url_path = $1`, urlPath)
	if err != nil {
		return fmt.Errorf("mark upstream removed: %w", err)
	}
	return nil
}

func (s *PostStore) Save(post *StoredPost) error {
	_, err := s.db.Exec(`
		INSERT INTO posts (url_path, subreddit, post_id, title, json_data, rendered_html,
		                   author, score, created_utc, source, listing_rank, listing_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (url_path) DO UPDATE SET
			json_data     = EXCLUDED.json_data,
			rendered_html = EXCLUDED.rendered_html,
			title         = EXCLUDED.title,
			author        = EXCLUDED.author,
			score         = EXCLUDED.score,
			created_utc   = EXCLUDED.created_utc,
			source        = EXCLUDED.source,
			-- Preserve the prior hot-listing position when this write carries none
			-- (an on-demand re-archive), so L3 ordering never loses a post's rank.
			listing_rank    = COALESCE(EXCLUDED.listing_rank, posts.listing_rank),
			listing_seen_at = COALESCE(EXCLUDED.listing_seen_at, posts.listing_seen_at),
			last_updated  = NOW()`,
		post.URLPath, post.Subreddit, post.PostID, post.Title,
		post.JSONData, post.RenderedHTML,
		post.Author, post.Score, post.CreatedUTC, post.Source,
		post.ListingRank, post.ListingSeenAt,
	)
	if err != nil {
		return fmt.Errorf("save post: %w", err)
	}
	return nil
}

func (s *PostStore) ListBySubreddit(sub string, limit, offset int, excludeNSFW bool) ([]*StoredPost, error) {
	where := "LOWER(subreddit) = LOWER($1)"
	if excludeNSFW {
		where += nsfwExcludeSQL
	}
	q := fmt.Sprintf(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE %s
		ORDER BY created_utc DESC
		LIMIT $2 OFFSET $3`, where)
	rows, err := s.db.Query(q, sub, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list posts by subreddit: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListRecent(limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		ORDER BY created_utc DESC
		LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recent posts: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListRecentBySubs(subs []string, limit int) ([]*StoredPost, error) {
	if len(subs) == 0 {
		return nil, nil
	}
	query := `
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE subreddit = ANY($1)
		ORDER BY created_utc DESC
		LIMIT $2`
	rows, err := s.db.Query(query, pq.Array(subs), limit)
	if err != nil {
		return nil, fmt.Errorf("list recent posts by subs: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListRecentExcludingSubs(subs []string, limit int) ([]*StoredPost, error) {
	if len(subs) == 0 {
		return s.ListRecent(limit)
	}
	query := `
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE subreddit != ALL($1)
		ORDER BY created_utc DESC
		LIMIT $2`
	rows, err := s.db.Query(query, pq.Array(subs), limit)
	if err != nil {
		return nil, fmt.Errorf("list recent posts excluding subs: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListNewlyArchived(limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE first_seen >= NOW() - INTERVAL '30 days'
		ORDER BY first_seen DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list newly archived: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListTopScored(limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE first_seen >= NOW() - INTERVAL '30 days'
		ORDER BY score DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list top scored: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListNotorious(limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE first_seen >= NOW() - INTERVAL '30 days'
		  AND score < 0
		ORDER BY score ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list notorious: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

// ListHomepage serves the curated homepage feed. The sort keyword drives the
// time-window/order baseline (new/new_archive/top/notorious); the opts carry the
// homepage filter, which is now the FULL unified search grammar — sub: scope,
// author, media type, score/comments thresholds, date bounds and NSFW rating —
// applied through the same WHERE builder as the /archive local search so the two
// honour identical constraint semantics. opts.Limit/opts.Offset page the feed.
func (s *PostStore) ListHomepage(sort string, opts ArchiveSearchOpts) ([]*StoredPost, error) {
	var baseWhere, orderBy string
	switch sort {
	case "new_archive":
		baseWhere = "first_seen >= NOW() - INTERVAL '30 days'"
		orderBy = "first_seen DESC"
	case "top":
		baseWhere = "first_seen >= NOW() - INTERVAL '30 days'"
		orderBy = "score DESC"
	case "notorious":
		baseWhere = "first_seen >= NOW() - INTERVAL '30 days' AND score < 0"
		orderBy = "score ASC"
	default:
		baseWhere = "1=1"
		orderBy = "created_utc DESC"
	}

	extra, args, argN := archiveFilterClauses(opts, 1)
	where := baseWhere + extra

	limitN, offsetN := argN, argN+1
	args = append(args, opts.Limit, opts.Offset)
	q := fmt.Sprintf(`SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`, where, orderBy, limitN, offsetN)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list homepage (%s): %w", sort, err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

// GoldenRatio is the fractional golden ratio (√5−1)/2 — the additive low-
// discrepancy step (x → frac(x + φ)) the random walk uses to rotate each new
// round's origin, applied alongside the on-wrap Reshuffle. Successive rounds of
// the same filter subset therefore enter a freshly-redrawn shuffle_key
// permutation at maximally-spread points (a Weyl/Kronecker equidistributed
// sequence), so consecutive full sweeps are maximally decorrelated.
const GoldenRatio = 0.6180339887498949

// RandomWalk returns up to n posts matching opts (plus media_done when mediaOnly)
// in ascending shuffle_key order, traversing the circular arc that begins just
// after `cursor` and ends back at `origin`. shuffle_key is a fixed random
// permutation of the rows in [0,1); advancing a monotonic cursor across it is
// sampling WITHOUT replacement — a full arc visits every matching row exactly
// once — and each page is a plain btree range scan (WHERE shuffle_key > cursor
// ORDER BY shuffle_key LIMIT n), O(log N), not the O(N log N) full sort that
// ORDER BY RANDOM() pays.
//
// The arc is walked as up to two linear segments: the upper segment (keys above
// cursor, climbing toward 1.0) and, once it is exhausted, the lower segment
// ([0, origin], the part below the round's origin that wraps around past 1.0).
// It returns the consumed posts, the new cursor (the largest key handed out) and
// roundDone=true once the arc back to origin is spent — the caller then opens a
// fresh round by rotating origin via GoldenRatio.
//
// mediaOnly is accepted for callsite clarity but does NOT add a SQL filter:
// media_done flips to true only after every media item (poster + preview +
// gallery + main) succeeded, so a video whose muxed mp4 is genuinely on disk
// but whose poster fetch 404'd reads media_done=false — and the /random media
// path would then never see it. The per-candidate IsResident check in
// serveRandomMedia (media_content.score >= 0) is the ground truth and already
// gates the redirect, so the SQL pre-filter would only ever hide otherwise
// servable posts.
func (s *PostStore) RandomWalk(opts ArchiveSearchOpts, mediaOnly bool, origin, cursor float64, n int) (posts []*StoredPost, newCursor float64, roundDone bool, err error) {
	if n < 1 {
		n = 1
	}
	newCursor = cursor

	// Phase A: cursor sits at/above origin → we are still on the upper segment,
	// climbing from cursor toward the top of the permutation.
	if cursor >= origin {
		upper, lastKey, err := s.randomWalkPage(opts, cursor, false, 0, n)
		if err != nil {
			return nil, cursor, false, err
		}
		posts = upper
		if len(upper) > 0 {
			newCursor = lastKey
		}
		if len(posts) >= n {
			return posts, newCursor, false, nil
		}
		// Upper segment exhausted. With origin == 0 there is nothing below it, so
		// the whole permutation was the upper arc → the round is complete.
		if origin <= 0 {
			return posts, newCursor, true, nil
		}
		// Spill into the lower segment [0, origin] to top up the page.
		need := n - len(posts)
		lower, lowKey, err := s.randomWalkPage(opts, -1, true, origin, need)
		if err != nil {
			return posts, newCursor, false, err
		}
		posts = append(posts, lower...)
		if len(lower) > 0 {
			newCursor = lowKey
		}
		// A short lower fill means the lower arc is spent too → round complete.
		return posts, newCursor, len(lower) < need, nil
	}

	// Phase B: cursor already wrapped below origin → climb the lower segment up to
	// origin, then the round closes.
	lower, lastKey, err := s.randomWalkPage(opts, cursor, true, origin, n)
	if err != nil {
		return nil, cursor, false, err
	}
	posts = lower
	if len(lower) > 0 {
		newCursor = lastKey
	}
	return posts, newCursor, len(lower) < n, nil
}

// Reshuffle redraws the entire shuffle_key permutation in one statement
// (UPDATE posts SET shuffle_key = random()). The random walk calls it on every
// round wrap-around so that a fresh sweep is a genuinely new permutation, not a
// replay of the previous round's order — combined with the golden-ratio origin
// rotation this maximises inter-round decorrelation. It is the one O(N) write in
// the design; it fires only at the end of a full no-replacement sweep, never per
// page.
func (s *PostStore) Reshuffle() error {
	if _, err := s.db.Exec(`UPDATE posts SET shuffle_key = random()`); err != nil {
		return fmt.Errorf("reshuffle posts: %w", err)
	}
	return nil
}

// randomWalkPage runs one keyset page of the random walk: the opts filter, plus
// shuffle_key > low and (when hasHigh) shuffle_key <= high, ordered ascending and
// capped at n. It returns the page and the largest shuffle_key it handed out.
// mediaOnly is intentionally not threaded down here: the SQL gate was dropped
// (see RandomWalk doc) in favor of per-candidate IsResident checks in
// serveRandomMedia, which are the ground truth.
func (s *PostStore) randomWalkPage(opts ArchiveSearchOpts, low float64, hasHigh bool, high float64, n int) ([]*StoredPost, float64, error) {
	extra, args, argN := archiveFilterClauses(opts, 1)
	where := "1=1" + extra
	where += fmt.Sprintf(" AND shuffle_key > $%d", argN)
	args = append(args, low)
	argN++
	if hasHigh {
		where += fmt.Sprintf(" AND shuffle_key <= $%d", argN)
		args = append(args, high)
		argN++
	}
	q := fmt.Sprintf(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done, shuffle_key
		FROM posts
		WHERE %s
		ORDER BY shuffle_key ASC
		LIMIT $%d`, where, argN)
	args = append(args, n)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("random walk page: %w", err)
	}
	defer rows.Close()
	return scanPostsWithKey(rows)
}

// escapeLike escapes the LIKE/ILIKE wildcard metacharacters (\ % _) so a user's
// free-text search term is matched literally rather than acting as wildcards.
// Pair with `ESCAPE '\'` in the SQL.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}

func (s *PostStore) Search(query string, limit int, excludeNSFW bool) ([]*StoredPost, error) {
	where := `title ILIKE '%' || $1 || '%' ESCAPE '\'`
	query = escapeLike(query)
	if excludeNSFW {
		where += nsfwExcludeSQL
	}
	q := fmt.Sprintf(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE %s
		ORDER BY created_utc DESC
		LIMIT $2`, where)
	rows, err := s.db.Query(q, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search posts: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

// ArchiveSearchOpts filters the local posts archive for the /archive page's
// purely-local search box. All fields are optional; zero/empty values are
// skipped so an all-empty opts set matches every archived post. The fields are
// populated from RedMemo's e621-style query syntax (see docs/reddit-search.md).
type ArchiveSearchOpts struct {
	Query      string     // title ILIKE %query%
	After      *time.Time // created_utc >= After
	Before     *time.Time // created_utc <= Before
	NSFW       string     // "" (any) | "nsfw" | "sfw"
	Media      []string   // nil = any; otherwise ordered subset of {"image","video","gif"} ORed together
	Author     string     // exact, case-insensitive author; empty = any
	Flair      string     // exact, case-insensitive flair text; empty = any
	WhiteSubs  []string   // case-insensitive whitelist; empty = any
	BlackSubs  []string   // case-insensitive blacklist; empty = none excluded
	Sort       string     // "new" (default, created_utc) | "top" (score) | "all" (first_seen)
	Score      *int       // score threshold; nil = any
	ScoreOp    string     // SQL comparison op: ">" "<" ">=" "<=" "="
	Comments   *int       // comment-count threshold; nil = any
	CommentsOp string     // SQL comparison op: ">" "<" ">=" "<=" "="
	Limit      int
	Offset     int
}

// safeSQLCmp whitelists a comparison operator so it can be interpolated into a
// query safely; anything unexpected falls back to ">".
func safeSQLCmp(op string) string {
	switch op {
	case ">", "<", ">=", "<=", "=":
		return op
	default:
		return ">"
	}
}

// mediaPostTypes expands a parsed media-type set into the PostType literals
// stored in json_data: "image" → image+gallery, "video" → video (is_gif=false),
// "gif" → gif (is_gif=true). Unknown tokens are skipped; duplicates de-duped.
// Returns nil when the set carries no constraint.
func mediaPostTypes(media []string) []string {
	if len(media) == 0 {
		return nil
	}
	seen := make(map[string]bool, 4)
	var out []string
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, m := range media {
		switch m {
		case "image":
			add("image")
			add("gallery")
		case "video":
			add("video")
		case "gif":
			add("gif")
		}
	}
	return out
}

// archiveFilterClauses builds the shared " AND ..." WHERE fragment (and its bind
// args) for every constraint expressible in ArchiveSearchOpts — title text, date
// bounds, NSFW rating, media type, author, flair, score/comments thresholds and
// the sub: include/exclude scope. startArg is the first positional placeholder index
// to use; the returned argN is the next free index. Both ArchiveSearch (the
// /archive local search) and ListHomepage (the homepage feed) compose this onto
// their own baseline so the two honour identical filter semantics.
func archiveFilterClauses(opts ArchiveSearchOpts, startArg int) (string, []any, int) {
	var where strings.Builder
	var args []any
	argN := startArg

	if q := strings.TrimSpace(opts.Query); q != "" {
		fmt.Fprintf(&where, ` AND title ILIKE '%%' || $%d || '%%' ESCAPE '\'`, argN)
		args = append(args, escapeLike(q))
		argN++
	}
	if opts.After != nil {
		fmt.Fprintf(&where, " AND created_utc >= $%d", argN)
		args = append(args, *opts.After)
		argN++
	}
	if opts.Before != nil {
		fmt.Fprintf(&where, " AND created_utc <= $%d", argN)
		args = append(args, *opts.Before)
		argN++
	}
	switch opts.NSFW {
	case "nsfw":
		where.WriteString(" AND COALESCE((json_data->>'over_18')::boolean, false) = true")
	case "sfw":
		where.WriteString(nsfwExcludeSQL)
	}
	if pts := mediaPostTypes(opts.Media); len(pts) > 0 {
		// Multiple media tokens (e.g. t:gif+vid) OR their PostType sets together.
		quoted := make([]string, len(pts))
		for i, t := range pts {
			quoted[i] = "'" + t + "'"
		}
		fmt.Fprintf(&where, " AND (json_data->>'PostType') IN (%s)", strings.Join(quoted, ","))
	}
	if opts.Author != "" {
		fmt.Fprintf(&where, " AND LOWER(author) = LOWER($%d)", argN)
		args = append(args, opts.Author)
		argN++
	}
	if opts.Flair != "" {
		// Flair text lives at json_data->'Flair'->>'Text'; match it exactly but
		// case-insensitively, mirroring the upstream flair_name: operator.
		fmt.Fprintf(&where, " AND LOWER(json_data->'Flair'->>'Text') = LOWER($%d)", argN)
		args = append(args, opts.Flair)
		argN++
	}
	if opts.Score != nil {
		fmt.Fprintf(&where, " AND score %s $%d", safeSQLCmp(opts.ScoreOp), argN)
		args = append(args, *opts.Score)
		argN++
	}
	if opts.Comments != nil {
		// Comments is stored as a [formatted, raw] string pair inside json_data;
		// the raw value (index 1) is the plain integer. Guard the cast so a
		// non-numeric value can't error the whole query.
		fmt.Fprintf(&where,
			" AND (json_data->'Comments'->>1) ~ '^[0-9]+$' AND (json_data->'Comments'->>1)::int %s $%d",
			safeSQLCmp(opts.CommentsOp), argN)
		args = append(args, *opts.Comments)
		argN++
	}
	if len(opts.WhiteSubs) > 0 {
		lower := make([]string, len(opts.WhiteSubs))
		for i, sub := range opts.WhiteSubs {
			lower[i] = strings.ToLower(sub)
		}
		fmt.Fprintf(&where, " AND LOWER(subreddit) = ANY($%d)", argN)
		args = append(args, pq.Array(lower))
		argN++
	}
	if len(opts.BlackSubs) > 0 {
		lower := make([]string, len(opts.BlackSubs))
		for i, sub := range opts.BlackSubs {
			lower[i] = strings.ToLower(sub)
		}
		fmt.Fprintf(&where, " AND LOWER(subreddit) != ALL($%d)", argN)
		args = append(args, pq.Array(lower))
		argN++
	}

	return where.String(), args, argN
}

// ArchiveSearch runs a purely-local search over the posts archive. It never
// touches Reddit; it only queries PostgreSQL. It returns the requested page of
// posts (newest first) plus the total number of distinct repost clusters for
// pagination.
//
// Repost-spam folding (v29): rows that share a non-null `repost_key` — same
// author + same normalized title — collapse into a single survivor at the SQL
// layer. The survivor is the highest-scoring row in the cluster; its returned
// RepostCount carries the size of the hidden tail (>1 ⇒ the renderer shows a
// "+N reposts" badge). Anonymous / [deleted] authors have a NULL repost_key
// and bucket per-row (via COALESCE on url_path) so they stay visible. See
// reddit.RepostKey for the matching Go-side normalization used on the live
// upstream path.
func (s *PostStore) ArchiveSearch(opts ArchiveSearchOpts) ([]*StoredPost, int64, error) {
	extra, args, argN := archiveFilterClauses(opts, 1)
	where := "1=1" + extra

	// Total counts distinct repost clusters, not raw rows, so the pagination
	// footer matches the deduped result set the user actually sees.
	var total int64
	countQ := "SELECT COUNT(DISTINCT COALESCE(repost_key, url_path)) FROM posts WHERE " + where
	if err := s.db.QueryRow(countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("archive search count: %w", err)
	}

	orderBy := "created_utc DESC"
	switch opts.Sort {
	case "top":
		orderBy = "score DESC"
	case "all":
		orderBy = "first_seen DESC"
	}

	limitN, offsetN := argN, argN+1
	args = append(args, opts.Limit, opts.Offset)
	// DISTINCT ON keeps the highest-scoring row per cluster; the inner window
	// COUNT propagates the cluster size onto that survivor so the badge can
	// render. The outer SELECT re-sorts by the caller's order (DISTINCT ON
	// forces its own ORDER BY for the picker), then pages.
	q := fmt.Sprintf(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done,
		       repost_count
		FROM (
			SELECT DISTINCT ON (COALESCE(repost_key, url_path))
			       url_path, subreddit, post_id, title, json_data, rendered_html,
			       author, score, created_utc, first_seen, last_updated, source, media_done,
			       COUNT(*) OVER (PARTITION BY COALESCE(repost_key, url_path)) AS repost_count
			FROM posts
			WHERE %s
			ORDER BY COALESCE(repost_key, url_path), score DESC NULLS LAST, created_utc DESC
		) folded
		ORDER BY %s
		LIMIT $%d OFFSET $%d`, where, orderBy, limitN, offsetN)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("archive search: %w", err)
	}
	defer rows.Close()
	posts, err := scanPostsWithRepostCount(rows)
	return posts, total, err
}

func (s *PostStore) CountBySubreddit(sub string, excludeNSFW bool) (int64, error) {
	where := "LOWER(subreddit) = LOWER($1)"
	if excludeNSFW {
		where += nsfwExcludeSQL
	}
	var count int64
	err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM posts WHERE %s`, where), sub).Scan(&count)
	return count, err
}

func (s *PostStore) Count() (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&count)
	return count, err
}

func (s *PostStore) DistinctSubreddits() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT subreddit FROM posts ORDER BY subreddit`)
	if err != nil {
		return nil, fmt.Errorf("distinct subreddits: %w", err)
	}
	defer rows.Close()
	var subs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

func (s *PostStore) SubredditCount() (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(DISTINCT subreddit) FROM posts`).Scan(&count)
	return count, err
}

type SubredditStat struct {
	Name      string
	PostCount int64
}

func (s *PostStore) SubredditStats(minPosts, limit int) ([]SubredditStat, error) {
	rows, err := s.db.Query(`
		SELECT subreddit, COUNT(*) AS cnt
		FROM posts
		GROUP BY subreddit
		HAVING COUNT(*) >= $1
		ORDER BY cnt DESC
		LIMIT $2`, minPosts, limit)
	if err != nil {
		return nil, fmt.Errorf("subreddit stats: %w", err)
	}
	defer rows.Close()
	var stats []SubredditStat
	for rows.Next() {
		var s SubredditStat
		if err := rows.Scan(&s.Name, &s.PostCount); err != nil {
			return nil, fmt.Errorf("scan subreddit stat: %w", err)
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

func (s *PostStore) SubredditCounts(names []string) (map[string]int, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT subreddit, COUNT(*) AS cnt
		FROM posts
		WHERE LOWER(subreddit) = ANY(
			SELECT LOWER(unnest) FROM unnest($1::text[])
		)
		GROUP BY subreddit`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("subreddit counts: %w", err)
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var name string
		var cnt int
		if err := rows.Scan(&name, &cnt); err != nil {
			return nil, err
		}
		result[name] = cnt
	}
	return result, rows.Err()
}

type ArchivedSub struct {
	Name        string
	PostCount   int64
	LastUpdated time.Time
}

func (s *PostStore) scanArchivedSubs(rows *sql.Rows) ([]ArchivedSub, error) {
	defer rows.Close()
	var subs []ArchivedSub
	for rows.Next() {
		var a ArchivedSub
		var lu sql.NullTime
		if err := rows.Scan(&a.Name, &a.PostCount, &lu); err != nil {
			return nil, fmt.Errorf("scan archived sub: %w", err)
		}
		if lu.Valid {
			a.LastUpdated = lu.Time
		}
		subs = append(subs, a)
	}
	return subs, rows.Err()
}

// ArchivedSubsByNew returns subs with strictly more than minPosts archived posts,
// ordered by most-recent archive activity first.
func (s *PostStore) ArchivedSubsByNew(minPosts int) ([]ArchivedSub, error) {
	rows, err := s.db.Query(`
		SELECT subreddit, COUNT(*) AS cnt, MAX(last_updated) AS lu
		FROM posts
		GROUP BY subreddit
		HAVING COUNT(*) > $1
		ORDER BY MAX(last_updated) DESC`, minPosts)
	if err != nil {
		return nil, fmt.Errorf("archived subs by new: %w", err)
	}
	return s.scanArchivedSubs(rows)
}

// ArchivedSubsByTop returns subs with strictly more than minPosts archived posts,
// ordered by post count descending.
func (s *PostStore) ArchivedSubsByTop(minPosts int) ([]ArchivedSub, error) {
	rows, err := s.db.Query(`
		SELECT subreddit, COUNT(*) AS cnt, MAX(last_updated) AS lu
		FROM posts
		GROUP BY subreddit
		HAVING COUNT(*) > $1
		ORDER BY cnt DESC, LOWER(subreddit) ASC`, minPosts)
	if err != nil {
		return nil, fmt.Errorf("archived subs by top: %w", err)
	}
	return s.scanArchivedSubs(rows)
}

// DetectNSFWForSubs scans the posts table for the given sub names and returns,
// for each, whether at least one archived post is over_18=true. Names without
// any posts are absent from the result.
func (s *PostStore) DetectNSFWForSubs(names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT subreddit, BOOL_OR(COALESCE((json_data->>'over_18')::boolean, false))
		FROM posts
		WHERE LOWER(subreddit) = ANY(SELECT LOWER(unnest) FROM unnest($1::text[]))
		GROUP BY subreddit`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("detect nsfw: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		var v bool
		if err := rows.Scan(&name, &v); err != nil {
			return nil, fmt.Errorf("scan detect nsfw: %w", err)
		}
		out[name] = v
	}
	return out, rows.Err()
}

// ArchivedSubsAlphabetical returns all archived subs sorted alphabetically (case-insensitive).
func (s *PostStore) ArchivedSubsAlphabetical() ([]ArchivedSub, error) {
	rows, err := s.db.Query(`
		SELECT subreddit, COUNT(*) AS cnt, MAX(last_updated) AS lu
		FROM posts
		GROUP BY subreddit
		ORDER BY LOWER(subreddit) ASC`)
	if err != nil {
		return nil, fmt.Errorf("archived subs alphabetical: %w", err)
	}
	return s.scanArchivedSubs(rows)
}

func (s *PostStore) SetMediaDone(urlPath string) error {
	_, err := s.db.Exec(`UPDATE posts SET media_done = true WHERE url_path = $1`, urlPath)
	if err != nil {
		return fmt.Errorf("set media done: %w", err)
	}
	return nil
}

// ClearMediaDone resets a post to the media-needed state so the next L2 wave
// re-harvests it. Used when an L3 comment fetch surfaces inline comment-body
// images the L2 media pass — which only saw the post body and structured media
// — never queued. L2, not L3, downloads the bytes; this only re-arms the queue.
func (s *PostStore) ClearMediaDone(urlPath string) error {
	_, err := s.db.Exec(`UPDATE posts SET media_done = false WHERE url_path = $1`, urlPath)
	if err != nil {
		return fmt.Errorf("clear media done: %w", err)
	}
	return nil
}

func (s *PostStore) ListNeedingMedia(sub string, limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE LOWER(subreddit) = LOWER($1) AND media_done = false
		ORDER BY created_utc DESC
		LIMIT $2`, sub, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list needing media: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

// ListL3Candidates returns posts in sub eligible for an L3 (comments) fetch
// under the cycle-freeze + min-comments rules:
//
//   - Cycle freeze: a post whose most recent successful L3 fetch was during the
//     *current* L3 cycle (currentCycleID) or the *previous* one (prevCycleID for
//     this sub) is skipped — so a post archived in L3 cycle N stays frozen during
//     N+1 and naturally unfreezes at N+2. The cycle ids are now L3's own lineage
//     (L3:<tf>:<sub>:<unix>), decoupled from L1/L2.
//   - Min comments: a post whose stored num_comments is < minComments is
//     skipped at the SQL layer; the count is parsed from the archived
//     reddit.Post JSON (Comments[1] is the raw numeric string). 0 disables the
//     filter.
//
// Pass "" for prevCycleID when no prior L1 cycle exists yet (fresh sub). The
// query never returns posts whose subreddit name does not match (case-insensitive).
func (s *PostStore) ListL3Candidates(sub, currentCycleID, prevCycleID string, limit, minComments int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		WITH last_l3 AS (
			SELECT DISTINCT ON (post_id) post_id, cycle_id,
			       COALESCE(NULLIF(payload->>'num_comments', '')::int, -1) AS last_nc
			FROM prefetch_runs
			WHERE layer = 'L3'
			  AND post_id IS NOT NULL
			  AND status = 'ok'
			  AND LOWER(subreddit) = LOWER($1)
			ORDER BY post_id, scheduled_at DESC
		)
		SELECT p.url_path, p.subreddit, p.post_id, p.title, p.json_data, p.rendered_html,
		       p.author, p.score, p.created_utc, p.first_seen, p.last_updated, p.source, p.media_done
		FROM posts p
		LEFT JOIN last_l3 ll ON ll.post_id = p.post_id
		WHERE LOWER(p.subreddit) = LOWER($1)
		  AND ($5 = 0
		       OR COALESCE(NULLIF(p.json_data->'Comments'->>1, '')::int, 0) >= $5)
		  AND (ll.cycle_id IS NULL
		       OR (ll.cycle_id <> $2 AND ll.cycle_id <> NULLIF($3, ''))
		       -- Rumination override: a post otherwise frozen by the
		       -- current/previous-cycle dedup is re-admitted the moment its
		       -- upstream-reported comment count exceeds what we recorded at the
		       -- last L3 fetch. New replies on a still-hot post must not wait out
		       -- the freeze. Requires a recorded baseline (last_nc >= 0); rows
		       -- predating num_comments payloads (-1) keep the plain freeze.
		       OR (ll.last_nc >= 0
		           AND COALESCE(NULLIF(p.json_data->'Comments'->>1, '')::int, 0) > ll.last_nc))
		-- Drain in upstream hot-listing order so the posts a homepage visitor sees
		-- first get their comments first: most recent listing snapshot leads
		-- (listing_seen_at DESC), and within that snapshot the API's own order
		-- top-to-bottom (listing_rank ASC, 0 = first). On-demand archives carry no
		-- listing position (NULLs) and trail; created_utc breaks any remaining tie.
		ORDER BY p.listing_seen_at DESC NULLS LAST,
		         p.listing_rank ASC NULLS LAST,
		         p.created_utc DESC
		LIMIT $4`, sub, currentCycleID, prevCycleID, limit, minComments,
	)
	if err != nil {
		return nil, fmt.Errorf("list L3 candidates: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) SaveHTML(urlPath string, html []byte) error {
	htmlStr := string(html)
	_, err := s.db.Exec(`
		UPDATE posts SET rendered_html = $2, last_updated = NOW()
		WHERE url_path = $1`, urlPath, htmlStr,
	)
	return err
}

// scanPostsWithKey scans rows that carry a trailing shuffle_key column and
// returns the posts plus the largest shuffle_key seen. Because the random walk
// orders ascending, that is simply the last row's key; 0 when the page is empty.
func scanPostsWithKey(rows *sql.Rows) ([]*StoredPost, float64, error) {
	var posts []*StoredPost
	var lastKey float64
	for rows.Next() {
		p := &StoredPost{}
		var key float64
		if err := rows.Scan(
			&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
			&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source, &p.MediaDone, &key,
		); err != nil {
			return nil, 0, fmt.Errorf("scan post with key: %w", err)
		}
		posts = append(posts, p)
		lastKey = key
	}
	return posts, lastKey, rows.Err()
}

// scanPostsWithRepostCount scans rows with a trailing repost_count column —
// the cluster-size value DISTINCT ON queries surface via a window COUNT.
func scanPostsWithRepostCount(rows *sql.Rows) ([]*StoredPost, error) {
	var posts []*StoredPost
	for rows.Next() {
		p := &StoredPost{}
		if err := rows.Scan(
			&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
			&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source, &p.MediaDone,
			&p.RepostCount,
		); err != nil {
			return nil, fmt.Errorf("scan post with repost count: %w", err)
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}

func scanPosts(rows *sql.Rows) ([]*StoredPost, error) {
	var posts []*StoredPost
	for rows.Next() {
		p := &StoredPost{}
		if err := rows.Scan(
			&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
			&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source, &p.MediaDone,
		); err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}
