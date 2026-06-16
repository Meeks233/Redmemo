package store

// EstimatedDiskBytes is a cheap DB-side lower bound on media disk usage: the
// sum of file_size over resident content rows (file_path IS NOT NULL). The
// partial index idx_media_content_eviction ON (file_size, last_accessed)
// WHERE file_path IS NOT NULL lets Postgres satisfy this as an index-only
// aggregate without touching the heap.
//
// IMPORTANT — this is a LOWER BOUND, never an exact figure. A re-download
// orphans the old content row (file_path := NULL, see Save) before the stale
// file is unlinked from disk, so the bytes of a not-yet-removed orphan are
// excluded here while still occupying disk. Therefore real_usage >= estimate
// always holds. Callers must treat a low estimate as "definitely below" but a
// high estimate only as "maybe at/over" — the evictor uses it solely as a
// pre-check gate and reconciles against a real disk walk before evicting.
func (s *MediaIndexStore) EstimatedDiskBytes() (int64, error) {
	var total int64
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(file_size), 0)
		FROM media_content
		WHERE file_path IS NOT NULL`).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total, nil
}
