package store

import (
	"database/sql"
	"fmt"

	"github.com/redmemo/redmemo/internal/reddit"
)

// MediaIndexStore is a content-addressed media cache backed by two tables:
//
//   media_content — keyed by sha256(file_bytes). One row per unique file.
//                   Holds file_path, audio_state, eviction stats.
//   media_url     — keyed by CanonicalKey(rawURL). Maps a stable URL identity
//                   (query stripped) onto a content row. Many URLs can alias
//                   the same content.
//
// Callers still pass raw URLs (or muxed: prefixed keys). Canonicalization is
// internal so the call sites in media/proxy.go and media/mux.go are unchanged.
type MediaIndexStore struct {
	db *sql.DB
}

func NewMediaIndexStore(db *sql.DB) *MediaIndexStore {
	return &MediaIndexStore{db: db}
}

// Resolve returns the cached media for rawURL — joining the URL row to its
// content row by canonical key. Returns (nil, nil) when the canonical key is
// unknown (no media_url entry yet).
func (s *MediaIndexStore) Resolve(rawURL string) (*MediaMeta, error) {
	key := reddit.CanonicalKey(rawURL)
	m := &MediaMeta{}
	err := s.db.QueryRow(`
		SELECT u.raw_url, c.content_hash, c.file_path, c.mime_type, c.file_size,
		       c.first_seen, c.last_accessed, c.access_count, c.audio_state,
		       c.audio_fail_count, c.last_audio_attempt_at
		FROM media_url u
		JOIN media_content c ON c.content_hash = u.content_hash
		WHERE u.canonical_key = $1`, key,
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

// Save upserts a (content, url) pair after a successful fetch. meta.Hash is
// the hex sha256 of the file bytes — the new authoritative identifier. The
// variant-upgrade rule applies: if the canonical_key already points at a
// different (smaller) content row, repoint to the new larger file and NULL the
// old content row's file_path so eviction sweeps the orphan. Same-size or
// smaller fetches are no-ops on the URL row (we keep the better copy).
func (s *MediaIndexStore) Save(meta *MediaMeta) error {
	if meta.Hash == "" {
		return fmt.Errorf("save media: empty content hash")
	}
	if meta.FilePath == nil {
		return fmt.Errorf("save media: nil file path")
	}
	key := reddit.CanonicalKey(meta.OriginalURL)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin save: %w", err)
	}
	defer tx.Rollback()

	// Upsert the content row. Existing row keeps its audio_state and access
	// stats; only file_path / mime / size are refreshed (in case the file was
	// previously evicted and we just re-downloaded it).
	if _, err := tx.Exec(`
		INSERT INTO media_content (content_hash, file_path, mime_type, file_size)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (content_hash) DO UPDATE SET
			file_path = EXCLUDED.file_path,
			mime_type = EXCLUDED.mime_type,
			file_size = EXCLUDED.file_size`,
		meta.Hash, *meta.FilePath, meta.MIMEType, meta.FileSize,
	); err != nil {
		return fmt.Errorf("upsert content: %w", err)
	}

	// Variant-upgrade: look up what the canonical currently points at.
	var (
		existingHash *string
		existingSize *int64
	)
	err = tx.QueryRow(`
		SELECT u.content_hash, c.file_size
		FROM media_url u
		JOIN media_content c ON c.content_hash = u.content_hash
		WHERE u.canonical_key = $1`, key,
	).Scan(&existingHash, &existingSize)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lookup existing url: %w", err)
	}

	repoint := true
	if existingHash != nil {
		if *existingHash == meta.Hash {
			// Same content; nothing to do beyond refreshing raw_url (the
			// signature on the latest fetch is the freshest one we have).
			if _, err := tx.Exec(
				`UPDATE media_url SET raw_url = $1 WHERE canonical_key = $2`,
				meta.OriginalURL, key,
			); err != nil {
				return fmt.Errorf("refresh raw_url: %w", err)
			}
			return tx.Commit()
		}
		// Different content under the same canonical. Bigger wins — a fresh
		// thumbnail fetch must never overwrite a larger source already cached.
		if existingSize != nil && *existingSize >= meta.FileSize {
			repoint = false
		}
	}

	if !repoint {
		// Keep the existing (larger) mapping. The new content row stays in the
		// content table — another canonical may alias it later, or eviction
		// will clean it up.
		return tx.Commit()
	}

	// Repoint or insert the URL mapping.
	if _, err := tx.Exec(`
		INSERT INTO media_url (canonical_key, raw_url, content_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (canonical_key) DO UPDATE SET
			raw_url      = EXCLUDED.raw_url,
			content_hash = EXCLUDED.content_hash`,
		key, meta.OriginalURL, meta.Hash,
	); err != nil {
		return fmt.Errorf("upsert url: %w", err)
	}

	// NULL the orphaned old content's file_path so eviction reclaims the
	// disk byte. Skip if any other URL still aliases that content.
	if existingHash != nil {
		if _, err := tx.Exec(`
			UPDATE media_content
			SET file_path = NULL
			WHERE content_hash = $1
			  AND NOT EXISTS (SELECT 1 FROM media_url WHERE content_hash = $1)`,
			*existingHash,
		); err != nil {
			return fmt.Errorf("orphan old content: %w", err)
		}
	}

	return tx.Commit()
}

// RecordAccess bumps last_accessed and access_count on the content row that
// rawURL's canonical key resolves to. A canonical that does not map yet is a
// silent no-op (the next Save will install the row).
func (s *MediaIndexStore) RecordAccess(rawURL string) error {
	key := reddit.CanonicalKey(rawURL)
	_, err := s.db.Exec(`
		UPDATE media_content
		SET last_accessed = NOW(), access_count = access_count + 1
		WHERE content_hash = (SELECT content_hash FROM media_url WHERE canonical_key = $1)`,
		key,
	)
	if err != nil {
		return fmt.Errorf("record media access: %w", err)
	}
	return nil
}

// BatchRecordAccess applies RecordAccess to many URLs inside one transaction.
func (s *MediaIndexStore) BatchRecordAccess(urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin batch access: %w", err)
	}
	stmt, err := tx.Prepare(`
		UPDATE media_content
		SET last_accessed = NOW(), access_count = access_count + 1
		WHERE content_hash = (SELECT content_hash FROM media_url WHERE canonical_key = $1)`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("prepare batch access: %w", err)
	}
	defer stmt.Close()

	for _, url := range urls {
		if _, err := stmt.Exec(reddit.CanonicalKey(url)); err != nil {
			tx.Rollback()
			return fmt.Errorf("batch access %s: %w", url, err)
		}
	}
	return tx.Commit()
}

// Stats reports the count and total file size of resident (non-evicted)
// content rows. Used by the settings page disk-usage display.
func (s *MediaIndexStore) Stats() (count int64, totalSize int64, err error) {
	err = s.db.QueryRow(`
		SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(file_size), 0)
		FROM media_content
		WHERE file_path IS NOT NULL`).Scan(&count, &totalSize)
	return
}

// Delete removes a media_url row by canonical key. If the underlying content
// row is now orphaned (no other URL aliases it), its file_path is returned
// so the caller can unlink the file; otherwise nil. The content row itself
// is left in place — its row is cheap and a future fetch may re-attach.
func (s *MediaIndexStore) Delete(rawURL string) (*string, error) {
	key := reddit.CanonicalKey(rawURL)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin delete: %w", err)
	}
	defer tx.Rollback()

	var contentHash string
	err = tx.QueryRow(
		`DELETE FROM media_url WHERE canonical_key = $1 RETURNING content_hash`,
		key,
	).Scan(&contentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete media url: %w", err)
	}

	var stillReferenced bool
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM media_url WHERE content_hash = $1)`,
		contentHash,
	).Scan(&stillReferenced); err != nil {
		return nil, fmt.Errorf("check orphan content: %w", err)
	}

	if stillReferenced {
		return nil, tx.Commit()
	}

	// Orphaned: read the file_path, then clear it. Two statements in the same
	// tx — the read sees the pre-update value, the caller os.Removes it.
	var filePath *string
	err = tx.QueryRow(
		`SELECT file_path FROM media_content WHERE content_hash = $1`,
		contentHash,
	).Scan(&filePath)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("read orphan file_path: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE media_content SET file_path = NULL WHERE content_hash = $1`,
		contentHash,
	); err != nil {
		return nil, fmt.Errorf("clear orphan file_path: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return filePath, nil
}

// DeleteSupersededPlainRows drops every legacy non-muxed video URL row whose
// muxed:<inner> counterpart already holds a conclusive cached file. Returns
// file paths of any orphaned content (the unmuxed silent file) so the caller
// can unlink them. Idempotent.
func (s *MediaIndexStore) DeleteSupersededPlainRows() ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin sweep: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		DELETE FROM media_url AS plain
		WHERE plain.canonical_key NOT LIKE 'muxed:%'
		  AND EXISTS (
		      SELECT 1 FROM media_url mu
		      JOIN media_content mc ON mc.content_hash = mu.content_hash
		      WHERE mu.canonical_key = 'muxed:' || plain.canonical_key
		        AND mc.audio_state IN ('has_audio', 'silent')
		        AND mc.file_path IS NOT NULL
		  )
		RETURNING plain.content_hash`)
	if err != nil {
		return nil, fmt.Errorf("delete superseded plain rows: %w", err)
	}
	var orphans []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan superseded plain row: %w", err)
		}
		orphans = append(orphans, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var paths []string
	for _, h := range orphans {
		var fp *string
		err := tx.QueryRow(`
			UPDATE media_content
			SET file_path = NULL
			WHERE content_hash = $1
			  AND NOT EXISTS (SELECT 1 FROM media_url WHERE content_hash = $1)
			RETURNING file_path`,
			h,
		).Scan(&fp)
		if err == sql.ErrNoRows {
			continue // still referenced by another URL
		}
		if err != nil {
			return nil, fmt.Errorf("orphan content: %w", err)
		}
		if fp != nil {
			paths = append(paths, *fp)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}

// MarkEvicted clears the file_path on a content row whose disk file the
// evictor has just removed. The URL aliases stay so the next request can
// re-trigger a fetch and re-attach.
func (s *MediaIndexStore) MarkEvicted(contentHash string) error {
	_, err := s.db.Exec(
		`UPDATE media_content SET file_path = NULL WHERE content_hash = $1`,
		contentHash,
	)
	if err != nil {
		return fmt.Errorf("mark evicted: %w", err)
	}
	return nil
}

// SetAudioState writes a conclusive audio verdict ("has_audio" or "silent")
// on the content row aliased by rawURL's canonical key and resets the failure
// counter. Used by the muxing pipeline once it knows the truth about a video.
func (s *MediaIndexStore) SetAudioState(rawURL, state string) error {
	key := reddit.CanonicalKey(rawURL)
	_, err := s.db.Exec(`
		UPDATE media_content
		SET audio_state = $2, audio_fail_count = 0
		WHERE content_hash = (SELECT content_hash FROM media_url WHERE canonical_key = $1)`,
		key, state,
	)
	if err != nil {
		return fmt.Errorf("set audio state: %w", err)
	}
	return nil
}

// RecordAudioFailure increments the failure counter on the content row and
// stamps the attempt time. Below the threshold the row is 'failed' (L5 keeps
// retrying); once it reaches the threshold it flips to 'abandoned' and L5
// stops. Returns the resulting audio_state.
func (s *MediaIndexStore) RecordAudioFailure(rawURL string, abandonThreshold int) (string, error) {
	key := reddit.CanonicalKey(rawURL)
	var state string
	err := s.db.QueryRow(`
		UPDATE media_content
		SET audio_fail_count = audio_fail_count + 1,
		    last_audio_attempt_at = NOW(),
		    audio_state = CASE WHEN audio_fail_count + 1 >= $2 THEN 'abandoned' ELSE 'failed' END
		WHERE content_hash = (SELECT content_hash FROM media_url WHERE canonical_key = $1)
		RETURNING audio_state`,
		key, abandonThreshold,
	).Scan(&state)
	if err != nil {
		return "", fmt.Errorf("record audio failure: %w", err)
	}
	return state, nil
}

// ReviveAudio moves an 'abandoned' content row back to 'failed' so L5 picks
// it up again. No-op for rows not 'abandoned'.
func (s *MediaIndexStore) ReviveAudio(rawURL string) error {
	key := reddit.CanonicalKey(rawURL)
	_, err := s.db.Exec(`
		UPDATE media_content
		SET audio_state = 'failed', audio_fail_count = 0
		WHERE content_hash = (SELECT content_hash FROM media_url WHERE canonical_key = $1)
		  AND audio_state = 'abandoned'`,
		key,
	)
	if err != nil {
		return fmt.Errorf("revive audio: %w", err)
	}
	return nil
}

// ListAudioFailed returns the raw URL of muxed entries parked as 'failed',
// oldest first, capped at limit. 'abandoned' rows are excluded — L5 stops
// retrying those.
func (s *MediaIndexStore) ListAudioFailed(limit int) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT u.raw_url
		FROM media_url u
		JOIN media_content c ON c.content_hash = u.content_hash
		WHERE c.audio_state = 'failed'
		  AND u.canonical_key LIKE 'muxed:%'
		ORDER BY c.first_seen ASC
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

// ListEvictionCandidates returns the worst (size × idle_hours) content rows,
// largest pressure first. The evictor passes these to MarkEvicted after it
// removes the file on disk.
func (s *MediaIndexStore) ListEvictionCandidates(limit int) ([]*MediaMeta, error) {
	rows, err := s.db.Query(`
		SELECT content_hash, file_path, mime_type, file_size,
		       first_seen, last_accessed, access_count, audio_state
		FROM media_content
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
			&m.Hash, &m.FilePath, &m.MIMEType, &m.FileSize,
			&m.FirstSeen, &m.LastAccessed, &m.AccessCount, &m.AudioState,
		); err != nil {
			return nil, fmt.Errorf("scan eviction candidate: %w", err)
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}
