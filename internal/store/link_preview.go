package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// LinkPreviewStore persists the unfurled OpenGraph metadata for external links
// so a page only ever pays the cross-site fetch once. Rows are keyed by the
// canonical link URL (reddit.CanonicalKey) and carry a status so a link that
// could not be unfurled is remembered as a negative result and not re-fetched
// on every render.
type LinkPreviewStore struct {
	db *sql.DB
}

func NewLinkPreviewStore(db *sql.DB) *LinkPreviewStore {
	return &LinkPreviewStore{db: db}
}

// LinkPreview is one cached unfurl result. Status is "ok" (Title/Image usable)
// or "failed" (the link could not be unfurled — remembered so we back off).
type LinkPreview struct {
	URLKey      string
	URL         string
	Title       string
	Description string
	ImageURL    string
	SiteName    string
	ImageWide   bool   // render image as full-width banner vs small thumbnail
	VideoURL    string // playable embed (X/Twitter via fixupx), or ""
	Status      string
	FetchedAt   time.Time
}

const (
	LinkPreviewOK     = "ok"
	LinkPreviewFailed = "failed"
)

// GetMany returns the cached rows for the given canonical keys, keyed by
// url_key. Keys without a row are simply absent from the map. Rows older than
// maxAge are treated as expired (absent) so a previously-failed link gets a
// fresh attempt, and a stale title/image eventually refreshes.
func (s *LinkPreviewStore) GetMany(keys []string, maxAge time.Duration) (map[string]*LinkPreview, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT url_key, url, title, description, image_url, site_name, image_wide, video_url, status, fetched_at
		FROM link_preview
		WHERE url_key = ANY($1) AND fetched_at > $2`,
		pq.Array(keys), time.Now().Add(-maxAge))
	if err != nil {
		return nil, fmt.Errorf("get link previews: %w", err)
	}
	defer rows.Close()

	out := make(map[string]*LinkPreview, len(keys))
	for rows.Next() {
		lp := &LinkPreview{}
		if err := rows.Scan(&lp.URLKey, &lp.URL, &lp.Title, &lp.Description,
			&lp.ImageURL, &lp.SiteName, &lp.ImageWide, &lp.VideoURL, &lp.Status, &lp.FetchedAt); err != nil {
			return nil, fmt.Errorf("scan link preview: %w", err)
		}
		out[lp.URLKey] = lp
	}
	return out, rows.Err()
}

// Get returns the single cached row for one canonical key, fresh within maxAge,
// or nil when absent/expired. The lazy /api/unfurl endpoint resolves one link at
// a time, so this is the per-link counterpart to GetMany.
func (s *LinkPreviewStore) Get(key string, maxAge time.Duration) (*LinkPreview, error) {
	lp := &LinkPreview{}
	err := s.db.QueryRow(`
		SELECT url_key, url, title, description, image_url, site_name, image_wide, video_url, status, fetched_at
		FROM link_preview
		WHERE url_key = $1 AND fetched_at > $2`,
		key, time.Now().Add(-maxAge),
	).Scan(&lp.URLKey, &lp.URL, &lp.Title, &lp.Description,
		&lp.ImageURL, &lp.SiteName, &lp.ImageWide, &lp.VideoURL, &lp.Status, &lp.FetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link preview: %w", err)
	}
	return lp, nil
}

// Upsert writes (or refreshes) one cached preview row. fetched_at is stamped to
// NOW() so the freshness window in GetMany restarts on every write.
func (s *LinkPreviewStore) Upsert(lp *LinkPreview) error {
	_, err := s.db.Exec(`
		INSERT INTO link_preview (url_key, url, title, description, image_url, site_name, image_wide, video_url, status, fetched_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW())
		ON CONFLICT (url_key) DO UPDATE SET
			url         = EXCLUDED.url,
			title       = EXCLUDED.title,
			description = EXCLUDED.description,
			image_url   = EXCLUDED.image_url,
			site_name   = EXCLUDED.site_name,
			image_wide  = EXCLUDED.image_wide,
			video_url   = EXCLUDED.video_url,
			status      = EXCLUDED.status,
			fetched_at  = NOW()`,
		lp.URLKey, lp.URL, lp.Title, lp.Description, lp.ImageURL, lp.SiteName, lp.ImageWide, lp.VideoURL, lp.Status)
	if err != nil {
		return fmt.Errorf("upsert link preview: %w", err)
	}
	return nil
}
