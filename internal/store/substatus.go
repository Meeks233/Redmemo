package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
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
	var nsfw sql.NullBool
	err := s.db.QueryRow(`
		SELECT name, status, reason, last_live, fail_count, checked_at, nsfw
		FROM subreddit_status WHERE name = $1`, name,
	).Scan(&st.Name, &st.Status, &reason, &st.LastLive, &st.FailCount, &st.CheckedAt, &nsfw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get sub status: %w", err)
	}
	st.Reason = reason.String
	if nsfw.Valid {
		v := nsfw.Bool
		st.NSFW = &v
	}
	return st, nil
}

// SetNSFW marks the sub as NSFW (true) or not (false). Once a sub has been
// marked TRUE we keep it sticky: callers should avoid downgrading to false.
// Creates a status row if missing (with default status='unknown').
func (s *SubStatusStore) SetNSFW(name string, nsfw bool) error {
	_, err := s.db.Exec(`
		INSERT INTO subreddit_status (name, status, nsfw)
		VALUES ($1, 'unknown', $2)
		ON CONFLICT (name) DO UPDATE SET
			nsfw = CASE
				WHEN subreddit_status.nsfw IS TRUE THEN TRUE
				ELSE EXCLUDED.nsfw
			END`, name, nsfw)
	if err != nil {
		return fmt.Errorf("set nsfw: %w", err)
	}
	return nil
}

// GetNSFWMap returns a map of name → nsfw flag. Names without a status row, or
// with nsfw IS NULL, are absent from the result (caller decides what to do).
func (s *SubStatusStore) GetNSFWMap(names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT name, nsfw FROM subreddit_status
		WHERE LOWER(name) = ANY(SELECT LOWER(unnest) FROM unnest($1::text[]))
		  AND nsfw IS NOT NULL`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("get nsfw map: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var n string
		var v bool
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}

// GetStatusMap returns a map of name → status string (e.g. "dead", "private",
// "live", "unknown") for the given names. Names without a row are absent.
func (s *SubStatusStore) GetStatusMap(names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT name, status FROM subreddit_status
		WHERE LOWER(name) = ANY(SELECT LOWER(unnest) FROM unnest($1::text[]))`, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("get status map: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var n, st string
		if err := rows.Scan(&n, &st); err != nil {
			return nil, err
		}
		out[n] = st
	}
	return out, rows.Err()
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

// ListAllAlive returns all locally known subs that are not confirmed dead/private.
// A sub is "known" if it appears in subreddit_status as live/quarantined, in posts,
// or in prefetch_config (enabled). status='unknown' is NOT an exclusion: that value
// doubles as the SetNSFW placeholder, so excluding it silently dropped NSFW-flagged
// subs from L4 icon refresh. Only 'dead' and 'private' — gravestones written after
// fail_count >= 3 in RecordFailure — actually mark a sub as gone.
func (s *SubStatusStore) ListAllAlive() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT s.name FROM (
			SELECT name FROM subreddit_status WHERE status IN ('live', 'quarantined')
			UNION
			SELECT DISTINCT subreddit FROM posts
			UNION
			SELECT subreddit FROM prefetch_config WHERE enabled = true
		) AS s
		LEFT JOIN subreddit_status ss ON ss.name = s.name
		WHERE ss.status IS NULL OR ss.status NOT IN ('dead', 'private')
		ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("list all alive subs: %w", err)
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
