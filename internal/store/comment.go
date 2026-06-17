package store

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"
)

type CommentStore struct {
	db *sql.DB
}

func NewCommentStore(db *sql.DB) *CommentStore {
	return &CommentStore{db: db}
}

func (s *CommentStore) GetLatest(postURLPath string) (*StoredComments, error) {
	c := &StoredComments{}
	err := s.db.QueryRow(`
		SELECT post_url_path, json_data, comment_count, fetched_at
		FROM comments
		WHERE post_url_path = $1
		ORDER BY fetched_at DESC
		LIMIT 1`, postURLPath,
	).Scan(&c.PostURLPath, &c.JSONData, &c.CommentCount, &c.FetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest comments: %w", err)
	}
	return c, nil
}

// HasCommentsForPaths returns the set of post url_paths (from the given list)
// that have at least one archived comment row. Used to decorate listing cards
// with the cloud-check "cached locally" hint without N round-trips: one indexed
// DISTINCT scan over the comments PK prefix covers the whole page. Absent paths
// simply don't appear in the returned map (callers treat missing as false).
func (s *CommentStore) HasCommentsForPaths(paths []string) (map[string]bool, error) {
	out := make(map[string]bool, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT post_url_path FROM comments WHERE post_url_path = ANY($1)`,
		pq.Array(paths),
	)
	if err != nil {
		return out, fmt.Errorf("has comments for paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return out, fmt.Errorf("has comments scan: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}

func (s *CommentStore) Save(postURLPath string, comments *StoredComments) error {
	_, err := s.db.Exec(`
		INSERT INTO comments (post_url_path, json_data, comment_count)
		VALUES ($1, $2, $3)`,
		postURLPath, comments.JSONData, comments.CommentCount,
	)
	if err != nil {
		return fmt.Errorf("save comments: %w", err)
	}
	return nil
}
