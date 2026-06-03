package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/redmemo/redmemo/internal/reddit"
)

// DefaultUnavailableThreshold is the number of consecutive failed fetches a
// media URL may accumulate before the proxy treats it as "probably banned" and
// stops re-hitting the CDN. Three matches the SubStatusStore RecordFailure
// threshold so the project keeps one mental model: "third strike, you're out".
const DefaultUnavailableThreshold = 3

// MediaUnavailableStore is the single ledger of media URLs that have been
// repeatedly refused by Reddit's CDN — typically because the owning post or
// the entire piece of media was banned/removed, after which v.redd.it /
// preview.redd.it / i.redd.it answer every byte request with 403/404 forever.
// The proxy queries IsUnavailable before any outbound fetch and refuses to
// hit upstream once a URL is marked; a user actively re-opening the owning
// post calls Revive to clear the mark and allow one more attempt.
type MediaUnavailableStore struct {
	db *sql.DB
}

func NewMediaUnavailableStore(db *sql.DB) *MediaUnavailableStore {
	return &MediaUnavailableStore{db: db}
}

// Ledger states surfaced to the proxy. The proxy never sees the raw row; it
// queries State() and branches on these constants so the schema (and the rule
// for escalating to "dead") is owned in one place.
const (
	// StateAlive: no record, or record with neither the soft nor the terminal
	// marker set. The proxy proceeds with a normal fetch.
	StateAlive = "alive"
	// StateUnavailable: marked_unavailable_at IS NOT NULL but dead_at IS NULL —
	// transiently bad, the user reopening the post triggers a revive that
	// allows one more attempt. The placeholder is the question-mark SVG.
	StateUnavailable = "unavailable"
	// StateDead: dead_at IS NOT NULL — terminal. Revive is a no-op and the
	// proxy refuses every request for the URL with the "Sorry, we missed it"
	// X-icon SVG. Set the first time a previously-revived URL fails again.
	StateDead = "dead"
)

// MediaUnavailableRecord is the row shape returned by Lookup.
type MediaUnavailableRecord struct {
	CanonicalKey        string
	RawURL              string
	Host                string
	Kind                string
	FailCount           int
	LastStatus          int
	LastError           string
	Reason              string
	FirstFailedAt       time.Time
	LastAttemptAt       time.Time
	MarkedUnavailableAt *time.Time
	RevivedAt           *time.Time
	DeadAt              *time.Time
}

// State returns the ledger verdict for rawURL — see the State* constants. dead
// dominates marked (a row that was marked, revived, then died still resolves
// to "dead"); a row that is neither marked nor dead resolves to "alive" so the
// proxy fetches normally.
func (s *MediaUnavailableStore) State(rawURL string) (string, error) {
	key := reddit.CanonicalKey(rawURL)
	var marked, dead sql.NullTime
	err := s.db.QueryRow(
		`SELECT marked_unavailable_at, dead_at FROM media_unavailable WHERE canonical_key = $1`,
		key,
	).Scan(&marked, &dead)
	if err == sql.ErrNoRows {
		return StateAlive, nil
	}
	if err != nil {
		return StateAlive, fmt.Errorf("media unavailable state: %w", err)
	}
	switch {
	case dead.Valid:
		return StateDead, nil
	case marked.Valid:
		return StateUnavailable, nil
	}
	return StateAlive, nil
}

// IsUnavailable is the fast path the media proxy calls before every outbound
// fetch. Returns true for both the soft "unavailable" and terminal "dead"
// states — both refuse the fetch. Callers needing to distinguish (e.g. to
// pick the right placeholder SVG) use State() instead.
func (s *MediaUnavailableStore) IsUnavailable(rawURL string) (bool, error) {
	st, err := s.State(rawURL)
	if err != nil {
		return false, err
	}
	return st != StateAlive, nil
}

// Lookup returns the full ledger row for rawURL, or (nil, nil) when there is
// no record. Callers use it to surface the reason on the placeholder.
func (s *MediaUnavailableStore) Lookup(rawURL string) (*MediaUnavailableRecord, error) {
	key := reddit.CanonicalKey(rawURL)
	r := &MediaUnavailableRecord{}
	var lastErr, reason sql.NullString
	var marked, revived sql.NullTime
	var dead sql.NullTime
	err := s.db.QueryRow(`
		SELECT canonical_key, raw_url, host, kind, fail_count, last_status,
		       last_error, reason, first_failed_at, last_attempt_at,
		       marked_unavailable_at, revived_at, dead_at
		FROM media_unavailable WHERE canonical_key = $1`, key,
	).Scan(
		&r.CanonicalKey, &r.RawURL, &r.Host, &r.Kind, &r.FailCount, &r.LastStatus,
		&lastErr, &reason, &r.FirstFailedAt, &r.LastAttemptAt, &marked, &revived, &dead,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup unavailable: %w", err)
	}
	r.LastError = lastErr.String
	r.Reason = reason.String
	if marked.Valid {
		t := marked.Time
		r.MarkedUnavailableAt = &t
	}
	if revived.Valid {
		t := revived.Time
		r.RevivedAt = &t
	}
	if dead.Valid {
		t := dead.Time
		r.DeadAt = &t
	}
	return r, nil
}

// RecordFailure increments the failure counter for rawURL and, once the count
// reaches threshold (use DefaultUnavailableThreshold when unsure), stamps
// marked_unavailable_at so IsUnavailable starts returning true. status is the
// observed HTTP code (0 for network/transport errors); errMsg is truncated to
// 256 chars on write so a chatty wrapped error chain never blows the row size.
// Returns the post-update fail_count and whether the URL is now marked.
//
// kind hints at the media type ("video", "audio", "image", "preview", ...) and
// is best-effort: ClassifyURL is a one-liner if the caller doesn't already know.
func (s *MediaUnavailableStore) RecordFailure(
	rawURL, kind string,
	status int,
	errMsg string,
	threshold int,
) (failCount int, marked bool, err error) {
	if threshold < 1 {
		threshold = DefaultUnavailableThreshold
	}
	key := reddit.CanonicalKey(rawURL)
	host := extractHost(rawURL)
	if kind == "" {
		kind = ClassifyMediaURL(rawURL)
	}
	reason := classifyReason(status)
	if len(errMsg) > 256 {
		errMsg = errMsg[:256]
	}
	// Two-tier escalation:
	//   - First time fail_count crosses threshold: stamp marked_unavailable_at
	//     (still revivable on user post visit).
	//   - Any failure on a row that has already been revived once
	//     (revived_at IS NOT NULL): stamp dead_at immediately. The user gave
	//     us a fresh chance and Reddit still refused — assert dead, no more
	//     attempts ever. A dead row's dead_at is sticky (later failures
	//     never overwrite it; later revives are skipped at the Revive site).
	var marker, deadMark sql.NullTime
	err = s.db.QueryRow(`
		INSERT INTO media_unavailable (
			canonical_key, raw_url, host, kind,
			fail_count, last_status, last_error, reason,
			first_failed_at, last_attempt_at, marked_unavailable_at
		)
		VALUES (
			$1, $2, $3, $4,
			1, $5, $6, $7,
			NOW(), NOW(),
			CASE WHEN 1 >= $8 THEN NOW() ELSE NULL END
		)
		ON CONFLICT (canonical_key) DO UPDATE SET
			raw_url         = EXCLUDED.raw_url,
			host            = EXCLUDED.host,
			kind            = EXCLUDED.kind,
			fail_count      = media_unavailable.fail_count + 1,
			last_status     = EXCLUDED.last_status,
			last_error      = EXCLUDED.last_error,
			reason          = EXCLUDED.reason,
			last_attempt_at = NOW(),
			marked_unavailable_at = CASE
				WHEN media_unavailable.fail_count + 1 >= $8 AND media_unavailable.marked_unavailable_at IS NULL
				THEN NOW()
				ELSE media_unavailable.marked_unavailable_at
			END,
			dead_at = CASE
				WHEN media_unavailable.dead_at IS NOT NULL THEN media_unavailable.dead_at
				WHEN media_unavailable.revived_at IS NOT NULL THEN NOW()
				ELSE NULL
			END
		RETURNING fail_count, marked_unavailable_at, dead_at`,
		key, rawURL, host, kind, status, nullString(errMsg), reason, threshold,
	).Scan(&failCount, &marker, &deadMark)
	if err != nil {
		return 0, false, fmt.Errorf("record media unavailable: %w", err)
	}
	return failCount, marker.Valid || deadMark.Valid, nil
}

// Revive clears marked_unavailable_at and resets fail_count on every supplied
// canonical key so the proxy will attempt those URLs once more. Called when a
// user actively opens the owning post — the assumption is that the user has
// fresh information ("maybe it's back now?") and we should re-probe at most
// one more time before re-marking on the next failure. raw URLs are accepted
// for caller convenience; canonicalization is internal.
func (s *MediaUnavailableStore) Revive(rawURLs []string) error {
	if len(rawURLs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(rawURLs))
	seen := make(map[string]struct{}, len(rawURLs))
	for _, u := range rawURLs {
		if u == "" {
			continue
		}
		k := reddit.CanonicalKey(u)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	// Dead rows are NEVER revived — that's the whole point of the terminal
	// tier. A row that has been escalated to dead has already burned through
	// one user-triggered retry; Reddit said no twice now, no further attempts.
	_, err := s.db.Exec(`
		UPDATE media_unavailable
		SET marked_unavailable_at = NULL,
		    fail_count            = 0,
		    revived_at            = NOW()
		WHERE canonical_key = ANY($1)
		  AND marked_unavailable_at IS NOT NULL
		  AND dead_at IS NULL`,
		pq.Array(keys),
	)
	if err != nil {
		return fmt.Errorf("revive media: %w", err)
	}
	return nil
}

// ClassifyMediaURL maps a Reddit CDN URL onto a coarse media kind for the
// ledger. Falls back to "other" so an unfamiliar host still records a row.
func ClassifyMediaURL(rawURL string) string {
	host := extractHost(rawURL)
	switch host {
	case "v.redd.it":
		// DASH_AUDIO segments are the audio companion; everything else is video.
		if strings.Contains(rawURL, "DASH_AUDIO") || strings.Contains(rawURL, "CMAF_AUDIO") {
			return "audio"
		}
		return "video"
	case "i.redd.it":
		return "image"
	case "preview.redd.it", "external-preview.redd.it":
		return "preview"
	case "a.thumbs.redditmedia.com", "b.thumbs.redditmedia.com":
		return "thumb"
	case "emoji.redditmedia.com":
		return "emoji"
	case "styles.redditmedia.com":
		return "style"
	}
	return "other"
}

func extractHost(rawURL string) string {
	// Tolerate the muxed: key prefix (mux uses it internally) so the recorded
	// host is the underlying v.redd.it identity, not "muxed".
	raw := strings.TrimPrefix(rawURL, "muxed:")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return strings.ToLower(u.Host)
}

func classifyReason(status int) string {
	switch status {
	case 0:
		return "network"
	case 401, 403:
		return "banned"
	case 404, 410:
		return "gone"
	case 429:
		return "rate_limited"
	}
	if status >= 500 {
		return "upstream_5xx"
	}
	if status >= 400 {
		return "client_4xx"
	}
	return "unknown"
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
