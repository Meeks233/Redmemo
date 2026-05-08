package store

import (
	"database/sql"
	"fmt"
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
