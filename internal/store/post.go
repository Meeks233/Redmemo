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
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts WHERE url_path = $1`, urlPath,
	).Scan(
		&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
		&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source, &p.MediaDone,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get post: %w", err)
	}
	return p, nil
}

func (s *PostStore) Save(post *StoredPost) error {
	_, err := s.db.Exec(`
		INSERT INTO posts (url_path, subreddit, post_id, title, json_data, rendered_html,
		                   author, score, created_utc, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (url_path) DO UPDATE SET
			json_data     = EXCLUDED.json_data,
			rendered_html = EXCLUDED.rendered_html,
			title         = EXCLUDED.title,
			author        = EXCLUDED.author,
			score         = EXCLUDED.score,
			created_utc   = EXCLUDED.created_utc,
			source        = EXCLUDED.source,
			last_updated  = NOW()`,
		post.URLPath, post.Subreddit, post.PostID, post.Title,
		post.JSONData, post.RenderedHTML,
		post.Author, post.Score, post.CreatedUTC, post.Source,
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
// When mediaOnly is true it additionally restricts to posts whose media has been
// fetched (media_done = true); the /random media path uses it to walk a small
// pool and redirect to the first entry whose bytes are genuinely on disk (see
// handleRandom), honouring the endpoint's "never contacts Reddit" contract.
func (s *PostStore) RandomWalk(opts ArchiveSearchOpts, mediaOnly bool, origin, cursor float64, n int) (posts []*StoredPost, newCursor float64, roundDone bool, err error) {
	if n < 1 {
		n = 1
	}
	newCursor = cursor

	// Phase A: cursor sits at/above origin → we are still on the upper segment,
	// climbing from cursor toward the top of the permutation.
	if cursor >= origin {
		upper, lastKey, err := s.randomWalkPage(opts, mediaOnly, cursor, false, 0, n)
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
		lower, lowKey, err := s.randomWalkPage(opts, mediaOnly, -1, true, origin, need)
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
	lower, lastKey, err := s.randomWalkPage(opts, mediaOnly, cursor, true, origin, n)
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
func (s *PostStore) randomWalkPage(opts ArchiveSearchOpts, mediaOnly bool, low float64, hasHigh bool, high float64, n int) ([]*StoredPost, float64, error) {
	extra, args, argN := archiveFilterClauses(opts, 1)
	where := "1=1" + extra
	if mediaOnly {
		where += " AND media_done = true"
	}
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

func (s *PostStore) Search(query string, limit int, excludeNSFW bool) ([]*StoredPost, error) {
	where := "title ILIKE '%' || $1 || '%'"
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
	Media      string     // "" (any) | "image" | "video" (is_gif=false) | "gif" (is_gif=true)
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
		fmt.Fprintf(&where, " AND title ILIKE '%%' || $%d || '%%'", argN)
		args = append(args, q)
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
	switch opts.Media {
	case "image":
		where.WriteString(" AND (json_data->>'PostType') IN ('image','gallery')")
	case "video":
		// Real videos: Reddit's is_gif=false, captured as PostType "video".
		where.WriteString(" AND (json_data->>'PostType') = 'video'")
	case "gif":
		// GIF uploads: Reddit's is_gif=true, captured as PostType "gif".
		where.WriteString(" AND (json_data->>'PostType') = 'gif'")
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
// posts (newest first) plus the total number of matches for pagination.
func (s *PostStore) ArchiveSearch(opts ArchiveSearchOpts) ([]*StoredPost, int64, error) {
	extra, args, argN := archiveFilterClauses(opts, 1)
	where := "1=1" + extra

	var total int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM posts WHERE "+where, args...).Scan(&total); err != nil {
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
	q := fmt.Sprintf(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source, media_done
		FROM posts
		WHERE %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d`, where, orderBy, limitN, offsetN)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("archive search: %w", err)
	}
	defer rows.Close()
	posts, err := scanPosts(rows)
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
	Name      string
	PostCount int64
}

func (s *PostStore) scanArchivedSubs(rows *sql.Rows) ([]ArchivedSub, error) {
	defer rows.Close()
	var subs []ArchivedSub
	for rows.Next() {
		var a ArchivedSub
		if err := rows.Scan(&a.Name, &a.PostCount); err != nil {
			return nil, fmt.Errorf("scan archived sub: %w", err)
		}
		subs = append(subs, a)
	}
	return subs, rows.Err()
}

// ArchivedSubsByNew returns subs with strictly more than minPosts archived posts,
// ordered by most-recent archive activity first.
func (s *PostStore) ArchivedSubsByNew(minPosts int) ([]ArchivedSub, error) {
	rows, err := s.db.Query(`
		SELECT subreddit, COUNT(*) AS cnt
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
		SELECT subreddit, COUNT(*) AS cnt
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
		SELECT subreddit, COUNT(*) AS cnt
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
