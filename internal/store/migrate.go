package store

import (
	"database/sql"
	"fmt"
)

var migrations = []string{
	// v1: initial schema
	`CREATE TABLE IF NOT EXISTS posts (
		url_path        TEXT PRIMARY KEY,
		subreddit       TEXT NOT NULL,
		post_id         TEXT NOT NULL,
		title           TEXT,
		json_data       JSONB NOT NULL,
		rendered_html   TEXT,
		author          TEXT,
		score           INTEGER,
		created_utc     TIMESTAMPTZ,
		first_seen      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		source          TEXT NOT NULL,
		CONSTRAINT valid_source CHECK (source IN ('redlib_proxy', 'oauth_fallback', 'prefetch'))
	);
	CREATE INDEX IF NOT EXISTS idx_posts_subreddit ON posts (subreddit);
	CREATE INDEX IF NOT EXISTS idx_posts_post_id ON posts (post_id);
	CREATE INDEX IF NOT EXISTS idx_posts_created ON posts (created_utc DESC);
	CREATE INDEX IF NOT EXISTS idx_posts_last_updated ON posts (last_updated DESC);

	CREATE TABLE IF NOT EXISTS comments (
		post_url_path   TEXT NOT NULL REFERENCES posts(url_path),
		json_data       JSONB NOT NULL,
		comment_count   INTEGER,
		fetched_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (post_url_path, fetched_at)
	);

	CREATE TABLE IF NOT EXISTS subreddits (
		name            TEXT PRIMARY KEY,
		title           TEXT,
		description     TEXT,
		icon_url        TEXT,
		members         INTEGER,
		json_data       JSONB,
		last_updated    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS media_index (
		original_url    TEXT PRIMARY KEY,
		hash            TEXT NOT NULL,
		file_path       TEXT,
		mime_type       TEXT,
		file_size       BIGINT,
		first_seen      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_accessed   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		access_count    BIGINT NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_media_hash ON media_index (hash);
	CREATE INDEX IF NOT EXISTS idx_media_eviction ON media_index (file_size, last_accessed)
		WHERE file_path IS NOT NULL;

	CREATE TABLE IF NOT EXISTS oauth_tokens (
		id              SERIAL PRIMARY KEY,
		client_id       TEXT NOT NULL,
		client_secret   TEXT,
		access_token    TEXT,
		expires_at      TIMESTAMPTZ,
		rate_remaining  INTEGER,
		rate_reset_at   TIMESTAMPTZ,
		backend         TEXT NOT NULL,
		enabled         BOOLEAN NOT NULL DEFAULT TRUE,
		last_used       TIMESTAMPTZ,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS prefetch_config (
		subreddit       TEXT PRIMARY KEY,
		sort_by         TEXT NOT NULL DEFAULT 'hot',
		max_pages       INTEGER NOT NULL DEFAULT 1,
		fetch_comments  BOOLEAN NOT NULL DEFAULT TRUE,
		fetch_media     BOOLEAN NOT NULL DEFAULT TRUE,
		priority        INTEGER NOT NULL DEFAULT 0,
		enabled         BOOLEAN NOT NULL DEFAULT TRUE
	);`,

	// v2: expand source constraint to support search and user listing archive paths
	`ALTER TABLE posts DROP CONSTRAINT IF EXISTS valid_source;
	 ALTER TABLE posts ADD CONSTRAINT valid_source
		CHECK (source IN ('redlib_proxy', 'oauth_fallback', 'prefetch', 'search', 'user_listing'));`,

	// v3: add 'background' source for async archiving from redlib proxy path
	`ALTER TABLE posts DROP CONSTRAINT IF EXISTS valid_source;
	 ALTER TABLE posts ADD CONSTRAINT valid_source
		CHECK (source IN ('redlib_proxy', 'oauth_fallback', 'prefetch', 'search', 'user_listing', 'background'));`,
}

func RunMigrations(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version     INTEGER NOT NULL,
		applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	var current int
	row := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}

		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("execute migration %d: %w", i+1, err)
		}

		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES ($1)`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}

	return nil
}
