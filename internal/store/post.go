package store

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

type PostStore struct {
	db *sql.DB
}

func NewPostStore(db *sql.DB) *PostStore {
	return &PostStore{db: db}
}

func (s *PostStore) Get(urlPath string) (*StoredPost, error) {
	p := &StoredPost{}
	err := s.db.QueryRow(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source
		FROM posts WHERE url_path = $1`, urlPath,
	).Scan(
		&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
		&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source,
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

func (s *PostStore) ListBySubreddit(sub string, limit, offset int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source
		FROM posts
		WHERE subreddit = $1
		ORDER BY created_utc DESC
		LIMIT $2 OFFSET $3`, sub, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list posts by subreddit: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) ListRecent(limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source
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
		       author, score, created_utc, first_seen, last_updated, source
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
		       author, score, created_utc, first_seen, last_updated, source
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
		       author, score, created_utc, first_seen, last_updated, source
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
		       author, score, created_utc, first_seen, last_updated, source
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
		       author, score, created_utc, first_seen, last_updated, source
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

func (s *PostStore) ListHomepage(sort string, limit, offset int, subs []string, mode string) ([]*StoredPost, error) {
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

	where := baseWhere
	var args []any
	argN := 1

	if len(subs) > 0 && mode == "whitelist" {
		where += fmt.Sprintf(" AND subreddit = ANY($%d)", argN)
		args = append(args, pq.Array(subs))
		argN++
	} else if len(subs) > 0 && mode == "blacklist" {
		where += fmt.Sprintf(" AND subreddit != ALL($%d)", argN)
		args = append(args, pq.Array(subs))
		argN++
	}

	args = append(args, limit)
	limitN := argN
	argN++
	args = append(args, offset)
	q := fmt.Sprintf(`SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source
		FROM posts WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`, where, orderBy, limitN, argN)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list homepage (%s): %w", sort, err)
	}
	defer rows.Close()
	return scanPosts(rows)
}

func (s *PostStore) Search(query string, limit int) ([]*StoredPost, error) {
	rows, err := s.db.Query(`
		SELECT url_path, subreddit, post_id, title, json_data, rendered_html,
		       author, score, created_utc, first_seen, last_updated, source
		FROM posts
		WHERE title ILIKE '%' || $1 || '%'
		ORDER BY created_utc DESC
		LIMIT $2`, query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search posts: %w", err)
	}
	defer rows.Close()
	return scanPosts(rows)
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

func (s *PostStore) SaveHTML(urlPath string, html []byte) error {
	htmlStr := string(html)
	_, err := s.db.Exec(`
		UPDATE posts SET rendered_html = $2, last_updated = NOW()
		WHERE url_path = $1`, urlPath, htmlStr,
	)
	return err
}

func scanPosts(rows *sql.Rows) ([]*StoredPost, error) {
	var posts []*StoredPost
	for rows.Next() {
		p := &StoredPost{}
		if err := rows.Scan(
			&p.URLPath, &p.Subreddit, &p.PostID, &p.Title, &p.JSONData, &p.RenderedHTML,
			&p.Author, &p.Score, &p.CreatedUTC, &p.FirstSeen, &p.LastUpdated, &p.Source,
		); err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		posts = append(posts, p)
	}
	return posts, rows.Err()
}
