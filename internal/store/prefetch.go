package store

import (
	"database/sql"
	"fmt"
)

type PrefetchConfigStore struct {
	db *sql.DB
}

func NewPrefetchConfigStore(db *sql.DB) *PrefetchConfigStore {
	return &PrefetchConfigStore{db: db}
}

func (s *PrefetchConfigStore) Get(subreddit string) (*StoredPrefetchConfig, error) {
	c := &StoredPrefetchConfig{}
	err := s.db.QueryRow(`
		SELECT subreddit, sort_by, max_pages, fetch_comments, fetch_media, priority, enabled
		FROM prefetch_config
		WHERE subreddit = $1`, subreddit,
	).Scan(
		&c.Subreddit, &c.SortBy, &c.MaxPages, &c.FetchComments,
		&c.FetchMedia, &c.Priority, &c.Enabled,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get prefetch config: %w", err)
	}
	return c, nil
}

func (s *PrefetchConfigStore) ListEnabled() ([]*StoredPrefetchConfig, error) {
	rows, err := s.db.Query(`
		SELECT subreddit, sort_by, max_pages, fetch_comments, fetch_media, priority, enabled
		FROM prefetch_config
		WHERE enabled = TRUE
		ORDER BY priority DESC`)
	if err != nil {
		return nil, fmt.Errorf("list prefetch configs: %w", err)
	}
	defer rows.Close()

	var configs []*StoredPrefetchConfig
	for rows.Next() {
		c := &StoredPrefetchConfig{}
		if err := rows.Scan(
			&c.Subreddit, &c.SortBy, &c.MaxPages, &c.FetchComments,
			&c.FetchMedia, &c.Priority, &c.Enabled,
		); err != nil {
			return nil, fmt.Errorf("scan prefetch config: %w", err)
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

func (s *PrefetchConfigStore) Upsert(cfg *StoredPrefetchConfig) error {
	_, err := s.db.Exec(`
		INSERT INTO prefetch_config (subreddit, sort_by, max_pages, fetch_comments, fetch_media, priority, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (subreddit) DO UPDATE SET
			sort_by        = EXCLUDED.sort_by,
			max_pages      = EXCLUDED.max_pages,
			fetch_comments = EXCLUDED.fetch_comments,
			fetch_media    = EXCLUDED.fetch_media,
			priority       = EXCLUDED.priority,
			enabled        = EXCLUDED.enabled`,
		cfg.Subreddit, cfg.SortBy, cfg.MaxPages, cfg.FetchComments,
		cfg.FetchMedia, cfg.Priority, cfg.Enabled,
	)
	if err != nil {
		return fmt.Errorf("upsert prefetch config: %w", err)
	}
	return nil
}

func (s *PrefetchConfigStore) Delete(subreddit string) error {
	_, err := s.db.Exec(`DELETE FROM prefetch_config WHERE subreddit = $1`, subreddit)
	if err != nil {
		return fmt.Errorf("delete prefetch config: %w", err)
	}
	return nil
}
