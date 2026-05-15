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
	CREATE INDEX IF NOT EXISTS idx_posts_first_seen ON posts (first_seen DESC);
	CREATE INDEX IF NOT EXISTS idx_posts_score ON posts (score DESC);

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

	// v4: site_settings for persistent key-value settings (legacy sync, etc.)
	`CREATE TABLE IF NOT EXISTS site_settings (
		name        TEXT PRIMARY KEY,
		value       TEXT NOT NULL,
		source      TEXT NOT NULL DEFAULT 'legacy_sync',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`,

	// v5: subreddit liveness tracking
	`CREATE TABLE IF NOT EXISTS subreddit_status (
		name            TEXT PRIMARY KEY,
		status          TEXT NOT NULL DEFAULT 'live',
		reason          TEXT,
		last_live       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		fail_count      INTEGER NOT NULL DEFAULT 0,
		checked_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		CONSTRAINT valid_status CHECK (status IN ('live', 'dead', 'private', 'quarantined', 'unknown'))
	);`,

	// v6: L1/L2 prefetch media tracking + natural_prefetch source
	`ALTER TABLE posts ADD COLUMN IF NOT EXISTS media_done BOOLEAN NOT NULL DEFAULT false;
	 UPDATE posts SET media_done = true;
	 ALTER TABLE posts DROP CONSTRAINT IF EXISTS valid_source;
	 ALTER TABLE posts ADD CONSTRAINT valid_source
		CHECK (source IN ('redlib_proxy', 'oauth_fallback', 'prefetch', 'natural_prefetch', 'search', 'user_listing', 'background'));`,

	// v7: subreddit icon cache with expiry tracking
	`CREATE TABLE IF NOT EXISTS sub_icons (
		name            TEXT PRIMARY KEY,
		icon_url        TEXT NOT NULL DEFAULT '',
		local_path      TEXT,
		hash            TEXT,
		fetched_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at      TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 days'
	);`,

	// v8: re-fetch icons that were stored as local proxy paths instead of raw CDN URLs
	`UPDATE sub_icons SET expires_at = NOW() - INTERVAL '1 second'
	 WHERE icon_url NOT LIKE 'http%' AND icon_url != '';`,

	// v9: persist device identity (UA, headers) per token for restart survival
	`ALTER TABLE oauth_tokens ADD COLUMN IF NOT EXISTS headers_json JSONB;`,

	// v11: per-sub NSFW flag (nullable: NULL = never evaluated; once TRUE it is sticky)
	`ALTER TABLE subreddit_status ADD COLUMN IF NOT EXISTS nsfw BOOLEAN;`,

	// v12: cache subreddit /about/.json in the same row as the icon, with its
	// own independent 60-day expiry. NULL columns mean "never fetched".
	`ALTER TABLE sub_icons ADD COLUMN IF NOT EXISTS about_json JSONB;
	 ALTER TABLE sub_icons ADD COLUMN IF NOT EXISTS about_fetched_at TIMESTAMPTZ;
	 ALTER TABLE sub_icons ADD COLUMN IF NOT EXISTS about_expires_at TIMESTAMPTZ;`,
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
