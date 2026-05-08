package store

import (
	"database/sql"
	"fmt"
)

type SubredditStore struct {
	db *sql.DB
}

func NewSubredditStore(db *sql.DB) *SubredditStore {
	return &SubredditStore{db: db}
}

func (s *SubredditStore) Get(name string) (*StoredSubreddit, error) {
	sub := &StoredSubreddit{}
	err := s.db.QueryRow(`
		SELECT name, title, description, icon_url, members, json_data, last_updated
		FROM subreddits
		WHERE name = $1`, name,
	).Scan(
		&sub.Name, &sub.Title, &sub.Description, &sub.IconURL,
		&sub.Members, &sub.JSONData, &sub.LastUpdated,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subreddit: %w", err)
	}
	return sub, nil
}

func (s *SubredditStore) Save(sub *StoredSubreddit) error {
	_, err := s.db.Exec(`
		INSERT INTO subreddits (name, title, description, icon_url, members, json_data)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET
			title        = EXCLUDED.title,
			description  = EXCLUDED.description,
			icon_url     = EXCLUDED.icon_url,
			members      = EXCLUDED.members,
			json_data    = EXCLUDED.json_data,
			last_updated = NOW()`,
		sub.Name, sub.Title, sub.Description, sub.IconURL, sub.Members, sub.JSONData,
	)
	if err != nil {
		return fmt.Errorf("save subreddit: %w", err)
	}
	return nil
}

func (s *SubredditStore) List() ([]*StoredSubreddit, error) {
	rows, err := s.db.Query(`
		SELECT name, title, description, icon_url, members, json_data, last_updated
		FROM subreddits
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list subreddits: %w", err)
	}
	defer rows.Close()

	var subs []*StoredSubreddit
	for rows.Next() {
		sub := &StoredSubreddit{}
		if err := rows.Scan(
			&sub.Name, &sub.Title, &sub.Description, &sub.IconURL,
			&sub.Members, &sub.JSONData, &sub.LastUpdated,
		); err != nil {
			return nil, fmt.Errorf("scan subreddit: %w", err)
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}
