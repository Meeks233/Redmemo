package store

import (
	"database/sql"
	"fmt"
	"time"
)

type SubIconStore struct {
	db *sql.DB
}

func NewSubIconStore(db *sql.DB) *SubIconStore {
	return &SubIconStore{db: db}
}

func (s *SubIconStore) Get(name string) (*SubIcon, error) {
	icon := &SubIcon{}
	err := s.db.QueryRow(`
		SELECT name, icon_url, local_path, hash, fetched_at, expires_at,
		       about_json, about_fetched_at, about_expires_at, has_icon
		FROM sub_icons WHERE name = $1`, name,
	).Scan(&icon.Name, &icon.IconURL, &icon.LocalPath, &icon.Hash, &icon.FetchedAt, &icon.ExpiresAt,
		&icon.AboutJSON, &icon.AboutFetchedAt, &icon.AboutExpiresAt, &icon.HasIcon)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sub icon: %w", err)
	}
	return icon, nil
}

func (s *SubIconStore) Save(icon *SubIcon) error {
	_, err := s.db.Exec(`
		INSERT INTO sub_icons (name, icon_url, local_path, hash, fetched_at, expires_at, has_icon)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET
			icon_url   = EXCLUDED.icon_url,
			local_path = EXCLUDED.local_path,
			hash       = EXCLUDED.hash,
			fetched_at = EXCLUDED.fetched_at,
			expires_at = EXCLUDED.expires_at,
			has_icon   = EXCLUDED.has_icon`,
		icon.Name, icon.IconURL, icon.LocalPath, icon.Hash, icon.FetchedAt, icon.ExpiresAt, icon.HasIcon,
	)
	if err != nil {
		return fmt.Errorf("save sub icon: %w", err)
	}
	return nil
}

func (s *SubIconStore) ListExpired() ([]*SubIcon, error) {
	rows, err := s.db.Query(`
		SELECT name, icon_url, local_path, hash, fetched_at, expires_at,
		       about_json, about_fetched_at, about_expires_at, has_icon
		FROM sub_icons
		WHERE expires_at < NOW()
		ORDER BY expires_at`)
	if err != nil {
		return nil, fmt.Errorf("list expired icons: %w", err)
	}
	defer rows.Close()
	return scanIcons(rows)
}

func (s *SubIconStore) ListAll() ([]*SubIcon, error) {
	rows, err := s.db.Query(`
		SELECT name, icon_url, local_path, hash, fetched_at, expires_at,
		       about_json, about_fetched_at, about_expires_at, has_icon
		FROM sub_icons ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list all icons: %w", err)
	}
	defer rows.Close()
	return scanIcons(rows)
}

func (s *SubIconStore) GetIconMap(names []string) (map[string]*SubIcon, error) {
	if len(names) == 0 {
		return make(map[string]*SubIcon), nil
	}
	result := make(map[string]*SubIcon, len(names))
	for _, n := range names {
		icon, err := s.Get(n)
		if err != nil {
			return nil, err
		}
		if icon != nil {
			result[n] = icon
		}
	}
	return result, nil
}

func (s *SubIconStore) IconTTL() time.Duration {
	return 30 * 24 * time.Hour
}

// AboutTTL is the cache lifetime for /r/<sub>/about.json responses.
// Independent of icon TTL — about data refreshes far less often than icons.
func (s *SubIconStore) AboutTTL() time.Duration {
	return 60 * 24 * time.Hour
}

// SaveAbout upserts the about JSON for `name` with a fresh fetched_at and
// expires_at = now + AboutTTL. It does NOT touch icon_url / local_path /
// hash / fetched_at / expires_at. If the row does not exist yet, it is
// created with default (placeholder) icon fields — the icon scheduler
// will fill them on its own pass.
func (s *SubIconStore) SaveAbout(name string, aboutJSON []byte) error {
	now := time.Now()
	expires := now.Add(s.AboutTTL())
	_, err := s.db.Exec(`
		INSERT INTO sub_icons (name, icon_url, fetched_at, expires_at,
		                      about_json, about_fetched_at, about_expires_at)
		VALUES ($1, '', NOW(), NOW(), $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET
			about_json       = EXCLUDED.about_json,
			about_fetched_at = EXCLUDED.about_fetched_at,
			about_expires_at = EXCLUDED.about_expires_at`,
		name, aboutJSON, now, expires,
	)
	if err != nil {
		return fmt.Errorf("save sub about: %w", err)
	}
	return nil
}

func scanIcons(rows *sql.Rows) ([]*SubIcon, error) {
	var icons []*SubIcon
	for rows.Next() {
		icon := &SubIcon{}
		if err := rows.Scan(
			&icon.Name, &icon.IconURL, &icon.LocalPath, &icon.Hash,
			&icon.FetchedAt, &icon.ExpiresAt,
			&icon.AboutJSON, &icon.AboutFetchedAt, &icon.AboutExpiresAt,
			&icon.HasIcon,
		); err != nil {
			return nil, fmt.Errorf("scan sub icon: %w", err)
		}
		icons = append(icons, icon)
	}
	return icons, rows.Err()
}
