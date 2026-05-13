package store

import (
	"database/sql"
	"fmt"
	"time"
)

type SubStatusStore struct {
	db *sql.DB
}

func NewSubStatusStore(db *sql.DB) *SubStatusStore {
	return &SubStatusStore{db: db}
}

func (s *SubStatusStore) Get(name string) (*SubredditStatus, error) {
	st := &SubredditStatus{}
	var reason sql.NullString
	err := s.db.QueryRow(`
		SELECT name, status, reason, last_live, fail_count, checked_at
		FROM subreddit_status WHERE name = $1`, name,
	).Scan(&st.Name, &st.Status, &reason, &st.LastLive, &st.FailCount, &st.CheckedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sub status: %w", err)
	}
	st.Reason = reason.String
	return st, nil
}

func (s *SubStatusStore) MarkLive(name string) error {
	_, err := s.db.Exec(`
		INSERT INTO subreddit_status (name, status, last_live, fail_count, checked_at)
		VALUES ($1, 'live', NOW(), 0, NOW())
		ON CONFLICT (name) DO UPDATE SET
			status     = 'live',
			reason     = NULL,
			last_live  = NOW(),
			fail_count = 0,
			checked_at = NOW()`, name)
	return err
}

func (s *SubStatusStore) RecordFailure(name, reason string) error {
	_, err := s.db.Exec(`
		INSERT INTO subreddit_status (name, status, reason, fail_count, checked_at)
		VALUES ($1, 'unknown', $2, 1, NOW())
		ON CONFLICT (name) DO UPDATE SET
			reason     = $2,
			fail_count = subreddit_status.fail_count + 1,
			checked_at = NOW(),
			status     = CASE
				WHEN subreddit_status.fail_count + 1 >= 3 THEN
					CASE
						WHEN $2 LIKE '%banned%' THEN 'dead'
						WHEN $2 LIKE '%private%' THEN 'private'
						WHEN $2 LIKE '%quarantined%' THEN 'quarantined'
						ELSE 'dead'
					END
				ELSE 'unknown'
			END`, name, reason)
	return err
}

func (s *SubStatusStore) IsAlive(name string) (bool, error) {
	st, err := s.Get(name)
	if err != nil {
		return true, err
	}
	if st == nil {
		return true, nil
	}
	return st.Status == "live" || st.Status == "unknown", nil
}

func (s *SubStatusStore) ListDead() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM subreddit_status WHERE status IN ('dead', 'private', 'quarantined')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func (s *SubStatusStore) ListLive() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM subreddit_status WHERE status = 'live' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func (s *SubStatusStore) ShouldRecheck(name string, interval time.Duration) (bool, error) {
	st, err := s.Get(name)
	if err != nil {
		return false, err
	}
	if st == nil {
		return true, nil
	}
	return time.Since(st.CheckedAt) > interval, nil
}
