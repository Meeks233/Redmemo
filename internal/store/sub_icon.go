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
		SELECT name, icon_url, local_path, hash, fetched_at, expires_at
		FROM sub_icons WHERE name = $1`, name,
	).Scan(&icon.Name, &icon.IconURL, &icon.LocalPath, &icon.Hash, &icon.FetchedAt, &icon.ExpiresAt)
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
		INSERT INTO sub_icons (name, icon_url, local_path, hash, fetched_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET
			icon_url   = EXCLUDED.icon_url,
			local_path = EXCLUDED.local_path,
			hash       = EXCLUDED.hash,
			fetched_at = EXCLUDED.fetched_at,
			expires_at = EXCLUDED.expires_at`,
		icon.Name, icon.IconURL, icon.LocalPath, icon.Hash, icon.FetchedAt, icon.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("save sub icon: %w", err)
	}
	return nil
}

func (s *SubIconStore) ListExpired() ([]*SubIcon, error) {
	rows, err := s.db.Query(`
		SELECT name, icon_url, local_path, hash, fetched_at, expires_at
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
		SELECT name, icon_url, local_path, hash, fetched_at, expires_at
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

func scanIcons(rows *sql.Rows) ([]*SubIcon, error) {
	var icons []*SubIcon
	for rows.Next() {
		icon := &SubIcon{}
		if err := rows.Scan(
			&icon.Name, &icon.IconURL, &icon.LocalPath, &icon.Hash,
			&icon.FetchedAt, &icon.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("scan sub icon: %w", err)
		}
		icons = append(icons, icon)
	}
	return icons, rows.Err()
}
