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

	// v13: audio-track verdict for muxed v.redd.it entries. Lives on the same
	// media_index row as the muxed/silent cached file. NULL = never checked;
	// 'has_audio' = mux succeeded; 'silent' = Reddit returned 4xx for every
	// audio candidate (skip mux permanently). Transient failures (ffmpeg
	// missing, network 5xx) never write this column — the next request retries.
	`ALTER TABLE media_index ADD COLUMN IF NOT EXISTS audio_state TEXT
		CHECK (audio_state IS NULL OR audio_state IN ('has_audio','silent'));`,

	// v14: 'failed' audio verdict. When audio probing transiently fails 3x in
	// a row the row is parked as 'failed' (no cached file) so user requests
	// serve the silent intermediate immediately while the L5 background layer
	// re-attempts the mux. A successful retry overwrites it with the real
	// 'has_audio'/'silent' verdict.
	`ALTER TABLE media_index DROP CONSTRAINT IF EXISTS media_index_audio_state_check;
	 ALTER TABLE media_index ADD CONSTRAINT media_index_audio_state_check
		CHECK (audio_state IS NULL OR audio_state IN ('has_audio','silent','failed'));`,

	// v15: bounded audio-retry tracking. audio_fail_count counts failed mux
	// attempts; once it crosses the abandon threshold the row moves to the
	// 'abandoned' state and L5 stops actively retrying it (a later user view
	// revives it with a fresh budget). last_audio_attempt_at gates retry
	// cooldown so a popular broken video does not storm ffmpeg.
	`ALTER TABLE media_index ADD COLUMN IF NOT EXISTS audio_fail_count INT NOT NULL DEFAULT 0;
	 ALTER TABLE media_index ADD COLUMN IF NOT EXISTS last_audio_attempt_at TIMESTAMPTZ;
	 ALTER TABLE media_index DROP CONSTRAINT IF EXISTS media_index_audio_state_check;
	 ALTER TABLE media_index ADD CONSTRAINT media_index_audio_state_check
		CHECK (audio_state IS NULL OR audio_state IN ('has_audio','silent','failed','abandoned'));`,

	// v16: pinned device profile — a single, frozen Android device identity
	// (device_id, UA, app version) reused for every mobile_spoof auth so the
	// spoofed device fingerprint never drifts across token refreshes. The
	// id = 1 CHECK enforces exactly one row.
	`CREATE TABLE IF NOT EXISTS device_profile (
		id              INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
		device_id       TEXT NOT NULL,
		user_agent      TEXT NOT NULL,
		android_version INTEGER NOT NULL,
		app_version     TEXT NOT NULL,
		build           TEXT NOT NULL,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`,

	// v17: long-term version rotation state on the (single) device_profile row.
	// os_next_check_at gates the monthly StatCounter poll; os_adopt_delay_days
	// is the persisted random 2-6 month delay applied to predicted Android
	// releases; apk_refresh_remaining counts down per token refresh and triggers
	// an app-version rotation when it hits zero. *_at columns are audit records.
	`ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS os_next_check_at    TIMESTAMPTZ NOT NULL DEFAULT NOW();
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS os_adopt_delay_days INTEGER     NOT NULL DEFAULT 120;
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS os_upgraded_at      TIMESTAMPTZ;
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS apk_refresh_remaining INTEGER   NOT NULL DEFAULT 12;
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS apk_checked_at      TIMESTAMPTZ;`,

	// v18: version rotation is now pure offline derivation bound to each token
	// mint (no StatCounter poll, no APKMirror, no refresh counter). Drop the
	// columns that backed the old gated design — see docs/version-tracking.md.
	`ALTER TABLE device_profile DROP COLUMN IF EXISTS os_next_check_at;
	 ALTER TABLE device_profile DROP COLUMN IF EXISTS apk_refresh_remaining;
	 ALTER TABLE device_profile DROP COLUMN IF EXISTS apk_checked_at;`,

	// v19: two-tier rotation. The Android version is no longer derived: it is
	// fixed for a device's life and only changes at a "major rotation" — every
	// ~3 years, modelling the user replacing their phone (new device_id + new
	// OS version). next_android_version is the version a monthly StatCounter
	// poll has scheduled for that next rotation. device_born_at /
	// device_lifespan_days track the current device's lifecycle.
	`ALTER TABLE device_profile DROP COLUMN IF EXISTS os_adopt_delay_days;
	 ALTER TABLE device_profile DROP COLUMN IF EXISTS os_upgraded_at;
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS device_born_at       TIMESTAMPTZ NOT NULL DEFAULT NOW();
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS device_lifespan_days INTEGER     NOT NULL DEFAULT 1095;
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS next_android_version INTEGER     NOT NULL DEFAULT 0;
	 ALTER TABLE device_profile ADD COLUMN IF NOT EXISTS os_next_check_at     TIMESTAMPTZ NOT NULL DEFAULT NOW();`,

	// v20: content-addressed media store. media_index is replaced by two tables
	// — media_content keyed by sha256(file_bytes), and media_url keyed by
	// CanonicalKey(rawURL) pointing at the content row. This dedups across
	// resolution variants, signature refreshes, and same-bytes-different-host
	// reposts. audio_state moves to media_content because it is a property of
	// the file, not the URL. THIS DROPS THE EXISTING MEDIA CACHE — only safe
	// because the project is pre-production; on-disk files at sha256(url) paths
	// will be orphaned and need manual cleanup of the media root.
	`DROP TABLE IF EXISTS media_index;
	 CREATE TABLE media_content (
	    content_hash         TEXT PRIMARY KEY,
	    file_path            TEXT,
	    mime_type            TEXT,
	    file_size            BIGINT NOT NULL DEFAULT 0,
	    audio_state          TEXT,
	    audio_fail_count     INT  NOT NULL DEFAULT 0,
	    last_audio_attempt_at TIMESTAMPTZ,
	    first_seen           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	    last_accessed        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	    access_count         BIGINT NOT NULL DEFAULT 0,
	    CONSTRAINT media_content_audio_state_check CHECK (
	        audio_state IS NULL OR audio_state IN ('has_audio','silent','failed','abandoned')
	    )
	 );
	 CREATE TABLE media_url (
	    canonical_key        TEXT PRIMARY KEY,
	    raw_url              TEXT NOT NULL,
	    content_hash         TEXT NOT NULL REFERENCES media_content(content_hash),
	    first_seen           TIMESTAMPTZ NOT NULL DEFAULT NOW()
	 );
	 CREATE INDEX idx_media_url_content ON media_url (content_hash);
	 CREATE INDEX idx_media_content_eviction ON media_content (file_size, last_accessed)
	    WHERE file_path IS NOT NULL;`,

	// Partial index covering the SFW slice of the posts table. The vast majority
	// of listing queries run with show_nsfw=off, which appends a
	// `AND COALESCE((json_data->>'over_18')::boolean, false) = false` predicate
	// to `WHERE LOWER(subreddit)=...` or the homepage `WHERE` clauses. A partial
	// index on the SFW rows lets the planner satisfy those scans directly.
	`CREATE INDEX IF NOT EXISTS posts_sfw_sub_created_idx
	    ON posts (LOWER(subreddit), created_utc DESC)
	    WHERE COALESCE((json_data->>'over_18')::boolean, false) = false;
	 CREATE INDEX IF NOT EXISTS posts_sfw_created_idx
	    ON posts (created_utc DESC)
	    WHERE COALESCE((json_data->>'over_18')::boolean, false) = false;`,
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
