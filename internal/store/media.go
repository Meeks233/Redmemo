package store

import (
	"database/sql"
	"fmt"
)

type MediaIndexStore struct {
	db *sql.DB
}

func NewMediaIndexStore(db *sql.DB) *MediaIndexStore {
	return &MediaIndexStore{db: db}
}

func (s *MediaIndexStore) Resolve(originalURL string) (*MediaMeta, error) {
	m := &MediaMeta{}
	err := s.db.QueryRow(`
		SELECT original_url, hash, file_path, mime_type, file_size,
		       first_seen, last_accessed, access_count
		FROM media_index
		WHERE original_url = $1`, originalURL,
	).Scan(
		&m.OriginalURL, &m.Hash, &m.FilePath, &m.MIMEType, &m.FileSize,
		&m.FirstSeen, &m.LastAccessed, &m.AccessCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve media: %w", err)
	}
	return m, nil
}

func (s *MediaIndexStore) Save(meta *MediaMeta) error {
	_, err := s.db.Exec(`
		INSERT INTO media_index (original_url, hash, file_path, mime_type, file_size)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (original_url) DO UPDATE SET
			file_path     = EXCLUDED.file_path,
			mime_type     = EXCLUDED.mime_type,
			file_size     = EXCLUDED.file_size,
			last_accessed = NOW()`,
		meta.OriginalURL, meta.Hash, meta.FilePath, meta.MIMEType, meta.FileSize,
	)
	if err != nil {
		return fmt.Errorf("save media: %w", err)
	}
	return nil
}

func (s *MediaIndexStore) RecordAccess(originalURL string) error {
	_, err := s.db.Exec(`
		UPDATE media_index
		SET last_accessed = NOW(), access_count = access_count + 1
		WHERE original_url = $1`, originalURL,
	)
	if err != nil {
		return fmt.Errorf("record media access: %w", err)
	}
	return nil
}

func (s *MediaIndexStore) BatchRecordAccess(urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin batch access: %w", err)
	}
	stmt, err := tx.Prepare(`
		UPDATE media_index
		SET last_accessed = NOW(), access_count = access_count + 1
		WHERE original_url = $1`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("prepare batch access: %w", err)
	}
	defer stmt.Close()

	for _, url := range urls {
		if _, err := stmt.Exec(url); err != nil {
			tx.Rollback()
			return fmt.Errorf("batch access %s: %w", url, err)
		}
	}
	return tx.Commit()
}

func (s *MediaIndexStore) MarkEvicted(hash string) error {
	_, err := s.db.Exec(`
		UPDATE media_index SET file_path = NULL WHERE hash = $1`, hash,
	)
	if err != nil {
		return fmt.Errorf("mark evicted: %w", err)
	}
	return nil
}

func (s *MediaIndexStore) ListEvictionCandidates(limit int) ([]*MediaMeta, error) {
	rows, err := s.db.Query(`
		SELECT original_url, hash, file_path, mime_type, file_size,
		       first_seen, last_accessed, access_count
		FROM media_index
		WHERE file_path IS NOT NULL
		ORDER BY (file_size / 1048576.0) * (EXTRACT(EPOCH FROM NOW() - last_accessed) / 3600.0) DESC
		LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list eviction candidates: %w", err)
	}
	defer rows.Close()

	var metas []*MediaMeta
	for rows.Next() {
		m := &MediaMeta{}
		if err := rows.Scan(
			&m.OriginalURL, &m.Hash, &m.FilePath, &m.MIMEType, &m.FileSize,
			&m.FirstSeen, &m.LastAccessed, &m.AccessCount,
		); err != nil {
			return nil, fmt.Errorf("scan eviction candidate: %w", err)
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}
