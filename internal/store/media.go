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
		       first_seen, last_accessed, access_count, audio_state,
		       audio_fail_count, last_audio_attempt_at
		FROM media_index
		WHERE original_url = $1`, originalURL,
	).Scan(
		&m.OriginalURL, &m.Hash, &m.FilePath, &m.MIMEType, &m.FileSize,
		&m.FirstSeen, &m.LastAccessed, &m.AccessCount, &m.AudioState,
		&m.AudioFailCount, &m.LastAudioAttemptAt,
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

func (s *MediaIndexStore) Stats() (count int64, totalSize int64, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(file_size), 0) FROM media_index WHERE file_path IS NOT NULL`).Scan(&count, &totalSize)
	return
}

// Delete removes a media_index row and returns its file_path (if any) so the
// caller can unlink the cached file. Used to drop the legacy silent video-only
// row once a proper muxed copy supersedes it.
func (s *MediaIndexStore) Delete(originalURL string) (*string, error) {
	var filePath *string
	err := s.db.QueryRow(
		`DELETE FROM media_index WHERE original_url = $1 RETURNING file_path`,
		originalURL,
	).Scan(&filePath)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete media: %w", err)
	}
	return filePath, nil
}

// DeleteSupersededPlainRows removes every legacy non-muxed video row whose
// content is now superseded by a muxed: row that holds a conclusive cached
// file ('has_audio' or 'silent'). Returns the file paths of the deleted rows
// so the caller can unlink the orphaned files. Idempotent.
func (s *MediaIndexStore) DeleteSupersededPlainRows() ([]string, error) {
	rows, err := s.db.Query(`
		DELETE FROM media_index AS plain
		WHERE plain.original_url NOT LIKE 'muxed:%'
		  AND EXISTS (
		      SELECT 1 FROM media_index m
		      WHERE m.original_url = 'muxed:' || plain.original_url
		        AND m.audio_state IN ('has_audio', 'silent')
		        AND m.file_path IS NOT NULL
		  )
		RETURNING plain.file_path`)
	if err != nil {
		return nil, fmt.Errorf("delete superseded plain rows: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var fp *string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scan superseded plain row: %w", err)
		}
		if fp != nil {
			paths = append(paths, *fp)
		}
	}
	return paths, rows.Err()
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

// SetAudioState updates the audio verdict on an existing media_index row and
// resets the failure counter. Caller must Save the row first. Used for the
// conclusive verdicts "has_audio" and "silent".
func (s *MediaIndexStore) SetAudioState(originalURL, state string) error {
	_, err := s.db.Exec(`
		UPDATE media_index SET audio_state = $2, audio_fail_count = 0
		WHERE original_url = $1`,
		originalURL, state,
	)
	if err != nil {
		return fmt.Errorf("set audio state: %w", err)
	}
	return nil
}

// RecordAudioFailure increments the failure counter on an existing media_index
// row (caller must Save the emergency-silent file first) and stamps the attempt
// time. While the count stays below abandonThreshold the row is 'failed' (L5
// keeps retrying); once it reaches the threshold it flips to 'abandoned' and
// L5 stops. Returns the resulting audio_state.
func (s *MediaIndexStore) RecordAudioFailure(originalURL string, abandonThreshold int) (string, error) {
	var state string
	err := s.db.QueryRow(`
		UPDATE media_index
		SET audio_fail_count = audio_fail_count + 1,
		    last_audio_attempt_at = NOW(),
		    audio_state = CASE WHEN audio_fail_count + 1 >= $2 THEN 'abandoned' ELSE 'failed' END
		WHERE original_url = $1
		RETURNING audio_state`,
		originalURL, abandonThreshold,
	).Scan(&state)
	if err != nil {
		return "", fmt.Errorf("record audio failure: %w", err)
	}
	return state, nil
}

// ReviveAudio moves an 'abandoned' row back to 'failed' with a fresh retry
// budget, so the L5 layer picks it up again. Called when a user views a video
// whose audio mux was previously abandoned. No-op for rows not 'abandoned'.
func (s *MediaIndexStore) ReviveAudio(originalURL string) error {
	_, err := s.db.Exec(`
		UPDATE media_index
		SET audio_state = 'failed', audio_fail_count = 0
		WHERE original_url = $1 AND audio_state = 'abandoned'`,
		originalURL,
	)
	if err != nil {
		return fmt.Errorf("revive audio: %w", err)
	}
	return nil
}

// ListAudioFailed returns the original_url of muxed entries parked as 'failed',
// oldest first (first-come-first-served), capped at limit. 'abandoned' rows are
// intentionally excluded — L5 no longer retries those.
func (s *MediaIndexStore) ListAudioFailed(limit int) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT original_url FROM media_index
		WHERE audio_state = 'failed'
		ORDER BY first_seen ASC
		LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list audio failed: %w", err)
	}
	defer rows.Close()

	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("scan audio failed: %w", err)
		}
		urls = append(urls, u)
	}
	return urls, rows.Err()
}

func (s *MediaIndexStore) ListEvictionCandidates(limit int) ([]*MediaMeta, error) {
	rows, err := s.db.Query(`
		SELECT original_url, hash, file_path, mime_type, file_size,
		       first_seen, last_accessed, access_count, audio_state
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
			&m.FirstSeen, &m.LastAccessed, &m.AccessCount, &m.AudioState,
		); err != nil {
			return nil, fmt.Errorf("scan eviction candidate: %w", err)
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}
