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
	// audio candidate (skip mux permanently). Transient failures (network 5xx,
	// mux errors) never write this column — the next request retries.
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
	// cooldown so a popular broken video does not storm the muxer.
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

	// v21: priority-based eviction scoring on media_content. Each cached file
	// carries an eviction `score` in [0,100] (higher = evict sooner) plus a
	// sticky `score_floor` the score may never decay below. The initial score is
	// an ASYMMETRIC log-distance curve centred on 10MB: files near 10MB score
	// ~0 (cheapest to keep), and both very small and very large files score
	// toward 100 — large files punished harder (k=2.0) than small (k=0.8).
	// Access decays the score toward the floor (passive -1.00, active -5.00),
	// so frequently-touched assets sink down the eviction order over time.
	// content_hash (TEXT) is this table's identity — the goal's `asset_id`
	// maps onto it; there is no UUID in this schema.
	`
	-- Task 1: add columns (idempotent via duplicate_column guard).
	DO $$ BEGIN
	    ALTER TABLE media_content ADD COLUMN score NUMERIC(5,2) NOT NULL DEFAULT 0.00;
	EXCEPTION WHEN duplicate_column THEN NULL; END $$;
	DO $$ BEGIN
	    ALTER TABLE media_content ADD COLUMN score_floor NUMERIC(5,2) NOT NULL DEFAULT 0.00;
	EXCEPTION WHEN duplicate_column THEN NULL; END $$;
	DO $$ BEGIN
	    ALTER TABLE media_content ADD COLUMN last_accessed_at TIMESTAMPTZ;
	EXCEPTION WHEN duplicate_column THEN NULL; END $$;

	-- Scoring helpers (shared by the backfill, the INSERT trigger and any caller).
	-- media_initial_score: asymmetric log-distance decay from 10MB, clamped to
	-- [0,100]. file_size is bytes; size_mb = bytes / 1MiB. A non-positive size is
	-- degenerate/unknown and scores 100 (evict first).
	CREATE OR REPLACE FUNCTION media_initial_score(p_size BIGINT)
	RETURNS NUMERIC AS $fn$
	DECLARE
	    size_mb NUMERIC;
	    d       NUMERIC;
	    k       NUMERIC;
	    s       NUMERIC;
	BEGIN
	    IF p_size IS NULL OR p_size <= 0 THEN
	        RETURN 100.00;
	    END IF;
	    size_mb := p_size::NUMERIC / 1048576.0;
	    d := ln(size_mb / 10.0);                 -- natural-log distance from 10MB
	    IF size_mb < 10 THEN
	        k := 0.8;
	    ELSE
	        k := 2.0;
	    END IF;
	    s := 100.0 * (1 - exp(-k * (d * d)));
	    s := GREATEST(LEAST(s, 100.0), 0.0);     -- clamp [0,100]
	    RETURN ROUND(s, 2);
	END;
	$fn$ LANGUAGE plpgsql IMMUTABLE;

	-- media_score_floor: tier the initial score into a sticky floor.
	--   0.00–24.99 -> 0 | 25.00–49.99 -> 25 | 50.00–74.99 -> 50 | 75.00–100 -> 75
	CREATE OR REPLACE FUNCTION media_score_floor(p_score NUMERIC)
	RETURNS NUMERIC AS $fn$
	BEGIN
	    RETURN LEAST(FLOOR(p_score / 25.0) * 25.0, 75.0);
	END;
	$fn$ LANGUAGE plpgsql IMMUTABLE;

	-- Task 2: backfill rows that predate scoring (both columns still at default).
	UPDATE media_content
	SET score       = media_initial_score(file_size),
	    score_floor = media_score_floor(media_initial_score(file_size))
	WHERE score = 0 AND score_floor = 0;

	-- Initial score on INSERT. A BEFORE INSERT trigger fills score/score_floor
	-- from file_size whenever they arrive at their defaults, so the existing
	-- Save() upsert keeps inserting only (content_hash, file_path, mime, size).
	-- ON CONFLICT DO UPDATE fires no INSERT trigger, so a re-download never
	-- resets an already-decayed score.
	CREATE OR REPLACE FUNCTION media_content_set_initial_score()
	RETURNS TRIGGER AS $fn$
	BEGIN
	    IF COALESCE(NEW.score, 0) = 0 THEN
	        NEW.score := media_initial_score(NEW.file_size);
	    END IF;
	    IF COALESCE(NEW.score_floor, 0) = 0 THEN
	        NEW.score_floor := media_score_floor(NEW.score);
	    END IF;
	    RETURN NEW;
	END;
	$fn$ LANGUAGE plpgsql;

	DROP TRIGGER IF EXISTS trg_media_content_initial_score ON media_content;
	CREATE TRIGGER trg_media_content_initial_score
	    BEFORE INSERT ON media_content
	    FOR EACH ROW EXECUTE FUNCTION media_content_set_initial_score();

	-- Task 3: access-decay primitive. Decrements the score (passive -1.00,
	-- active -5.00), clamps at the floor, stamps last_accessed_at, returns the
	-- new score. asset_id is the content_hash (TEXT) — no UUID in this schema.
	CREATE OR REPLACE FUNCTION update_asset_access(asset_id TEXT, access_type TEXT)
	RETURNS NUMERIC AS $fn$
	DECLARE
	    delta     NUMERIC;
	    new_score NUMERIC;
	BEGIN
	    IF access_type = 'active' THEN
	        delta := 5.00;
	    ELSIF access_type = 'passive' THEN
	        delta := 1.00;
	    ELSE
	        RAISE EXCEPTION 'update_asset_access: invalid access_type %, expected passive or active', access_type;
	    END IF;

	    UPDATE media_content
	    SET score            = GREATEST(score - delta, score_floor),
	        last_accessed_at = NOW()
	    WHERE content_hash = asset_id
	    RETURNING score INTO new_score;

	    RETURN new_score;
	END;
	$fn$ LANGUAGE plpgsql;

	-- Task 4: eviction order — highest score first, then least-recently accessed
	-- (NULLS FIRST so never-decayed rows lead the tie), then random jitter.
	-- Restricted to resident files; an evicted (file_path IS NULL) row is not a
	-- candidate.
	CREATE OR REPLACE VIEW eviction_candidates AS
	SELECT content_hash, file_path, mime_type, file_size,
	       first_seen, last_accessed, last_accessed_at, access_count,
	       audio_state, score, score_floor
	FROM media_content
	WHERE file_path IS NOT NULL
	ORDER BY score DESC,
	         last_accessed_at ASC NULLS FIRST,
	         random();

	CREATE INDEX IF NOT EXISTS idx_media_content_score
	    ON media_content (score DESC, last_accessed_at ASC)
	    WHERE file_path IS NOT NULL;`,

	// v22: embed a physical-existence judgment in the eviction score. The score
	// now doubles as a presence flag: a resident file (file_path IS NOT NULL)
	// keeps its real eviction score in [0,100]; an absent one (evicted, deleted,
	// or orphaned) carries the sentinel -1. The invariant is
	//   file_path IS NULL  <=>  score = -1
	// maintained by Save (re-judges on re-download), the INSERT trigger, and
	// every file_path := NULL site (MarkEvicted / Delete / orphan sweeps). The
	// /random media path filters on score <> -1 so it only ever picks posts whose
	// bytes are genuinely on disk — without a per-candidate stat() — instead of
	// redirecting to a cold URL the proxy would have to live-fetch from Reddit.
	`
	-- Batch re-judge existing rows. Absent rows take the -1 sentinel (floor too,
	-- so a later passive decay can never lift them off it); resident rows that
	-- somehow carry a negative score are recomputed from their size.
	UPDATE media_content
	SET score = -1.00, score_floor = -1.00
	WHERE file_path IS NULL;

	UPDATE media_content
	SET score       = media_initial_score(file_size),
	    score_floor = media_score_floor(media_initial_score(file_size))
	WHERE file_path IS NOT NULL AND score < 0;

	-- INSERT trigger: a row inserted without a file_path is absent (-1); one with
	-- a file_path is scored from its size as before. The existing Save() upsert
	-- always inserts a file_path, so new fetches keep scoring normally.
	CREATE OR REPLACE FUNCTION media_content_set_initial_score()
	RETURNS TRIGGER AS $fn$
	BEGIN
	    IF NEW.file_path IS NULL THEN
	        NEW.score := -1.00;
	        NEW.score_floor := -1.00;
	    ELSE
	        IF COALESCE(NEW.score, 0) <= 0 THEN
	            NEW.score := media_initial_score(NEW.file_size);
	        END IF;
	        IF COALESCE(NEW.score_floor, 0) <= 0 THEN
	            NEW.score_floor := media_score_floor(NEW.score);
	        END IF;
	    END IF;
	    RETURN NEW;
	END;
	$fn$ LANGUAGE plpgsql;

	-- update_asset_access: a -1 (absent) row is never decayed — the sentinel is
	-- sticky until a re-download re-judges it. Resident rows decay as before.
	CREATE OR REPLACE FUNCTION update_asset_access(asset_id TEXT, access_type TEXT)
	RETURNS NUMERIC AS $fn$
	DECLARE
	    delta     NUMERIC;
	    new_score NUMERIC;
	BEGIN
	    IF access_type = 'active' THEN
	        delta := 5.00;
	    ELSIF access_type = 'passive' THEN
	        delta := 1.00;
	    ELSE
	        RAISE EXCEPTION 'update_asset_access: invalid access_type %, expected passive or active', access_type;
	    END IF;

	    UPDATE media_content
	    SET score = CASE WHEN score < 0 THEN score
	                     ELSE GREATEST(score - delta, score_floor) END,
	        last_accessed_at = NOW()
	    WHERE content_hash = asset_id
	    RETURNING score INTO new_score;

	    RETURN new_score;
	END;
	$fn$ LANGUAGE plpgsql;`,

	// v23: stable shuffle_key permutation backing the no-replacement /random walk.
	// Every post gets a random key in [0,1); /random traverses it with a monotonic
	// per-filter cursor (WHERE shuffle_key > :cursor ORDER BY shuffle_key LIMIT :n),
	// so one full round visits every matching row EXACTLY ONCE — sampling without
	// replacement — via an O(log N) btree range scan instead of the O(N log N) full
	// sort that ORDER BY RANDOM() costs. On wrap-around (a completed sweep) the walk
	// redraws the whole permutation (UPDATE posts SET shuffle_key = random(), see
	// PostStore.Reshuffle) so the next round is fresh, and rotates its entry point
	// by the golden-ratio step (PostStore.RandomWalk) — a Weyl/Kronecker low-
	// discrepancy sequence — so consecutive rounds are maximally decorrelated. That
	// reshuffle is the only O(N) write and fires solely at sweep end, never per
	// page. The volatile random() DEFAULT means existing rows are
	// backfilled with distinct keys on the rewrite this ADD COLUMN triggers, and
	// every later INSERT (Save's upsert) inherits a fresh key without touching it.
	`ALTER TABLE posts ADD COLUMN IF NOT EXISTS shuffle_key DOUBLE PRECISION NOT NULL DEFAULT random();
	 CREATE INDEX IF NOT EXISTS idx_posts_shuffle_key ON posts (shuffle_key);`,

	// v24: fold the only contents of the old deploy/init.sql into the app's
	// migration chain so a fresh deploy needs zero external SQL files. pg_trgm
	// is required for the future full-text search path on archived posts;
	// CREATE EXTENSION IF NOT EXISTS is idempotent and runs as the database
	// owner that POSTGRES_USER created (a superuser in the official image).
	// Cluster-level tuning (shared_buffers, work_mem, …) cannot move here —
	// shared_buffers requires a postgres restart — and now lives in the
	// docker-compose `command:` args, where it takes effect at startup with
	// no extra file to download.
	`CREATE EXTENSION IF NOT EXISTS pg_trgm;`,

	// v25: separate "upstream has no icon" from "fetch failed". Subs like r/golang
	// permanently report empty icon_url; L4 should record that verdict once and
	// never spend another /about.json call on them. has_icon defaults TRUE so
	// every pre-existing row keeps the old retry behavior until L4 visits it
	// again and learns the real verdict. NOT NULL is safe because the default
	// covers backfill in a single rewrite.
	`ALTER TABLE sub_icons ADD COLUMN IF NOT EXISTS has_icon BOOLEAN NOT NULL DEFAULT TRUE;`,

	// v26: sticky upstream-removed verdict on archived posts. Once Reddit reports
	// a permalink as removed/deleted (removed_by_category set, selftext
	// "[removed]"/"[deleted]", or author "[deleted]" with empty self-body) we
	// flip upstream_removed=TRUE and stop overwriting the archived JSON or
	// re-requesting that permalink. Default FALSE preserves prior behaviour for
	// every existing row. NOT NULL is safe because the default fills the backfill
	// in one rewrite.
	`ALTER TABLE posts ADD COLUMN IF NOT EXISTS upstream_removed BOOLEAN NOT NULL DEFAULT FALSE;`,

	// v27: unified ban/unavailable ledger for individual media URLs. A removed
	// post can leave its media URLs serving 403/404 forever even when the post
	// JSON itself is happily cached; without this table every poll on
	// /api/audio_status (and every imageReload.js retry) re-spawns a fetch that
	// re-hits the Reddit CDN, burning quota and inviting IP-level throttling.
	// Threshold is enforced in the store layer (default 3): below it the
	// fetcher keeps trying; at/above it marked_unavailable_at is stamped and
	// the proxy short-circuits future requests with a placeholder until the
	// user actively re-visits the owning post (handler.servePost calls Revive
	// on every URL it can scrape from the post JSON, which clears the mark and
	// re-allows one more attempt).
	`CREATE TABLE IF NOT EXISTS media_unavailable (
		canonical_key         TEXT PRIMARY KEY,
		raw_url               TEXT NOT NULL,
		host                  TEXT NOT NULL,
		kind                  TEXT NOT NULL,
		fail_count            INT  NOT NULL DEFAULT 0,
		last_status           INT  NOT NULL DEFAULT 0,
		last_error            TEXT,
		reason                TEXT,
		first_failed_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_attempt_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		marked_unavailable_at TIMESTAMPTZ,
		revived_at            TIMESTAMPTZ
	 );
	 CREATE INDEX IF NOT EXISTS idx_media_unavailable_host   ON media_unavailable (host);
	 CREATE INDEX IF NOT EXISTS idx_media_unavailable_marked ON media_unavailable (marked_unavailable_at)
	    WHERE marked_unavailable_at IS NOT NULL;`,

	// v28: two-tier ban state. marked_unavailable_at alone was ambiguous — the
	// proxy treated every marked URL as "transiently bad, retry on next user
	// visit" which is correct for a 503 burst but wrong for an asset we have
	// proven is gone. dead_at is the terminal escalation: set the first time a
	// previously-revived URL fails again (the user actively asked us to retry,
	// Reddit said no again, that's the assertion the asset is dead). A dead
	// row is permanently refused — Revive skips it and the proxy never spawns
	// another fetch. The "uncertain" tier (marked but not dead) keeps the old
	// question-mark placeholder + revive-on-visit; the "dead" tier flips to the
	// "Sorry, we missed it…" X-icon card and removes all retry affordance.
	`ALTER TABLE media_unavailable ADD COLUMN IF NOT EXISTS dead_at TIMESTAMPTZ;
	 CREATE INDEX IF NOT EXISTS idx_media_unavailable_dead ON media_unavailable (dead_at)
	    WHERE dead_at IS NOT NULL;`,

	// v29: spam-repost folding. Stamps every post with a (author, normalized
	// title) key so the search/archive layers can collapse the same Reddit
	// account dumping a near-identical title across many subs. The column is
	// a STORED generated column — every existing row backfills atomically
	// when the migration runs and every future INSERT auto-populates it, so
	// the app layer never writes to repost_key directly.
	//
	// Normalization mirrors reddit.RepostKey exactly: lowercase author,
	// lowercase title with internal whitespace collapsed to single spaces and
	// outer whitespace trimmed. Anonymous / deleted authors produce NULL so
	// the dedup layer can fall back to per-row uniqueness (each row's URL)
	// and avoid bucketing every "[deleted]" post into one cluster.
	`ALTER TABLE posts ADD COLUMN IF NOT EXISTS repost_key TEXT
	    GENERATED ALWAYS AS (
	        CASE
	            WHEN author IS NULL OR LOWER(author) IN ('', '[deleted]') THEN NULL
	            ELSE LOWER(author) || '|' || LOWER(regexp_replace(BTRIM(COALESCE(title, '')), '\s+', ' ', 'g'))
	        END
	    ) STORED;
	 CREATE INDEX IF NOT EXISTS idx_posts_repost_key ON posts (repost_key) WHERE repost_key IS NOT NULL;`,

	// v30: broaden repost folding from (author, title) to title-only with a
	// minimum title length. The v29 column keyed on author, which kept
	// distinct buckets for the same listing forwarded by different accounts
	// across many subs — exactly the syndicated-spam case the search UI was
	// supposed to fold. Dropping author lets one normalized title collapse
	// every copy regardless of who posted it; the length floor stops generic
	// titles ("lol", "rule") from co-mingling unrelated content.
	//
	// A generated column's expression cannot be altered in place — drop and
	// re-add. The matching index moves with it.
	`DROP INDEX IF EXISTS idx_posts_repost_key;
	 ALTER TABLE posts DROP COLUMN IF EXISTS repost_key;
	 ALTER TABLE posts ADD COLUMN repost_key TEXT
	    GENERATED ALWAYS AS (
	        CASE
	            WHEN LENGTH(LOWER(regexp_replace(BTRIM(COALESCE(title, '')), '\s+', ' ', 'g'))) < 12 THEN NULL
	            ELSE LOWER(regexp_replace(BTRIM(COALESCE(title, '')), '\s+', ' ', 'g'))
	        END
	    ) STORED;
	 CREATE INDEX idx_posts_repost_key ON posts (repost_key) WHERE repost_key IS NOT NULL;`,

	// v31: unified prefetch run ledger covering L1 listing fetches, L2 media
	// download waves and on-demand L3 comment fetches. Replaces the scattered
	// settings-table cycle blobs and the previously implicit L2 schedule
	// (fixed 25 per round, no persistence). Columns:
	//   layer        — 'L1' | 'L2' | 'L3'
	//   bucket       — L1 timeframe bucket (hour/day/.../all) when layer='L1'
	//                  or the parent L1 fetch's bucket when layer='L2'
	//   subreddit    — sub the run targets (NULL only for layer-wide rows we
	//                  do not currently emit)
	//   post_id      — set on L2 (one wave fans across many posts: NULL) and
	//                  on L3 (one row per comment fetch)
	//   cycle_id     — groups L2 sub-interval waves with their parent L1 fetch;
	//                  '<bucket>:<sub>:<unix_ts>' on the L1 row and copied to
	//                  every L2 wave it scheduled
	//   sub_interval — 1..5 for L2 sub-interval waves; NULL for L1/L3
	//   scheduled_at — wall-clock time the run is supposed to start
	//   started_at / finished_at — actual execution stamps
	//   status       — pending|running|ok|fail|skipped
	//   payload      — layer-specific JSON (post counts, urls, etc.)
	`CREATE TABLE IF NOT EXISTS prefetch_runs (
	    id            BIGSERIAL PRIMARY KEY,
	    layer         TEXT NOT NULL CHECK (layer IN ('L1','L2','L3')),
	    bucket        TEXT,
	    subreddit     TEXT,
	    post_id       TEXT,
	    cycle_id      TEXT,
	    sub_interval  INTEGER,
	    scheduled_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	    started_at    TIMESTAMPTZ,
	    finished_at   TIMESTAMPTZ,
	    status        TEXT NOT NULL DEFAULT 'pending'
	        CHECK (status IN ('pending','running','ok','fail','skipped')),
	    payload       JSONB,
	    error         TEXT
	 );
	 CREATE INDEX IF NOT EXISTS idx_prefetch_runs_layer_sched
	    ON prefetch_runs (layer, scheduled_at DESC);
	 CREATE INDEX IF NOT EXISTS idx_prefetch_runs_pending
	    ON prefetch_runs (layer, scheduled_at)
	    WHERE status = 'pending';
	 CREATE INDEX IF NOT EXISTS idx_prefetch_runs_cycle
	    ON prefetch_runs (cycle_id) WHERE cycle_id IS NOT NULL;
	 CREATE INDEX IF NOT EXISTS idx_prefetch_runs_sub_post
	    ON prefetch_runs (subreddit, post_id);`,

	// v32: backfill the TOTP enrollment-confirmed marker. Any instance that
	// already has a persisted _totp_secret was enrolled before this marker
	// existed, so treat it as confirmed — otherwise the new "interrupted
	// enrollment" guard would re-show the enrollment QR to an already-enrolled
	// operator on upgrade. Idempotent: inserts only when a secret exists and the
	// marker is absent.
	`INSERT INTO site_settings (name, value, source)
	 SELECT '_totp_confirmed', '1', 'totp'
	 WHERE EXISTS (SELECT 1 FROM site_settings WHERE name = '_totp_secret')
	 ON CONFLICT (name) DO NOTHING;`,

	// v33: trusted-device long tokens. A "Trust this device" tick on the TOTP
	// gate mints a sliding 30-day cookie whose SHA-256 is persisted here (never the
	// plaintext — a DB leak must not yield a usable session token). token_prefix
	// keeps the first few plaintext chars purely so the management table can show
	// the operator which cookie a row maps to. The instance caps live rows at 3
	// (enforced in the store layer); expires_at backs both the validity check
	// (WHERE expires_at > NOW()) and the daily expiry sweep.
	`CREATE TABLE IF NOT EXISTS trusted_devices (
		id           BIGSERIAL PRIMARY KEY,
		token_hash   TEXT NOT NULL UNIQUE,
		token_prefix TEXT NOT NULL,
		ip           TEXT,
		created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_used    TIMESTAMPTZ,
		expires_at   TIMESTAMPTZ NOT NULL
	 );
	 CREATE INDEX IF NOT EXISTS idx_trusted_devices_expires ON trusted_devices (expires_at);`,

	// v34: dual-stack OR binding for trusted devices. A trusted cookie minted on a
	// dual-stack client is then presented from EITHER its IPv4 or its IPv6 address
	// depending on Happy-Eyeballs / the OS's per-connection choice. Binding only to
	// the single mint-time address rejects the sibling family, drops that browser
	// back to the TOTP gate, and (if the operator re-ticks "Trust this device")
	// mints a SECOND trusted row — burning one of the 3 slots for a single physical
	// device. `ip` stays the primary (mint-time) binding; `ip_alt` holds the
	// complementary-family address, learned on its first valid presentation, so
	// Check matches EITHER (ip OR ip_alt). Nullable, no backfill: existing rows keep
	// their single `ip` and lazily gain an `ip_alt` the next time they appear from
	// the other family.
	`ALTER TABLE trusted_devices ADD COLUMN IF NOT EXISTS ip_alt TEXT;`,
}

// migrationAdvisoryLockKey is an arbitrary constant identifying the migration
// lock. Any value works as long as it is stable across RedMemo instances.
const migrationAdvisoryLockKey = 0x7265646d // "redm"

func RunMigrations(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version     INTEGER NOT NULL,
		applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	// Serialize migration runners across instances/replicas. Without this, two
	// processes starting against the same database read the same MAX(version)
	// and both try to apply the same migrations; the non-idempotent statements
	// (bare CREATE TABLE / ADD COLUMN / CREATE INDEX) make the loser's tx fail
	// and that instance crashes on startup. A session-level advisory lock makes
	// the second runner wait, then observe the up-to-date version. The lock auto-
	// releases when the session ends, so a crash mid-migration cannot wedge it.
	if _, err := db.Exec(`SELECT pg_advisory_lock($1)`, migrationAdvisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer db.Exec(`SELECT pg_advisory_unlock($1)`, migrationAdvisoryLockKey)

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
