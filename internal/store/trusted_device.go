package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// TrustedDevice is one persisted long-lived "trusted device" session — the
// sliding 30-day cookie minted when the operator ticks "Trust this device" on
// the TOTP gate (its expiry is pushed forward on every use). Only the token's
// SHA-256 is stored (TokenHash is never read back out by the management UI);
// TokenPrefix is the cosmetic first-few-chars label.
type TrustedDevice struct {
	ID          int64
	TokenPrefix string
	IP          string
	// UserAgent is the latest User-Agent the device presented (stamped on each
	// valid Check). Cosmetic only — shown in the management table so the operator
	// can recognise the browser; never an authentication input. Empty until the
	// row's cookie has been presented at least once since the column was added.
	UserAgent string
	CreatedAt time.Time
	LastUsed  *time.Time
	ExpiresAt time.Time
}

type TrustedDeviceStore struct {
	db *sql.DB
}

func NewTrustedDeviceStore(db *sql.DB) *TrustedDeviceStore {
	return &TrustedDeviceStore{db: db}
}

// CountActive returns the number of trusted devices that have not yet expired.
// Used to enforce the per-instance cap before minting a new one.
func (s *TrustedDeviceStore) CountActive() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM trusted_devices WHERE expires_at > NOW()`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count trusted devices: %w", err)
	}
	return n, nil
}

// Create persists a new trusted device. The caller is responsible for checking
// CountActive against the cap first; this just inserts.
func (s *TrustedDeviceStore) Create(tokenHash, prefix, ip, ua string, expiresAt time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO trusted_devices (token_hash, token_prefix, ip, ua, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		tokenHash, prefix, ip, ua, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("create trusted device: %w", err)
	}
	return nil
}

// TrustVerdict is the outcome of checking a presented trusted-device cookie
// against the table.
type TrustVerdict int

const (
	// TrustAbsent — no row carries this hash (revoked, never existed, or already
	// swept). Reject.
	TrustAbsent TrustVerdict = iota
	// TrustValid — a live, unexpired row matched; last_used was stamped. Allow.
	TrustValid
	// TrustExpired — a row matched but was past expiry; Check deleted it in the
	// same statement. Reject (and the lazy cleanup has happened).
	TrustExpired
)

// Check verifies a presented token hash AND that it is being presented from an
// IP the device is bound to, self-healing in one round-trip:
//   - a live row whose ip OR ip_alt matches clientIP is touched (last_used = NOW)
//     AND its expiry is slid forward to newExpiry, then reported TrustValid — this
//     is the sliding window: a token in regular use never lapses, while one left
//     idle past the window expires;
//   - an expired row is deleted on the spot and reported TrustExpired (so an
//     invalid-but-uncleaned token is reaped exactly when it is presented);
//   - a missing row — OR a live row replayed from an unbound IP — is reported
//     TrustAbsent (rejected).
//
// Dual-stack OR binding. A dual-stack browser reaches us over either its IPv4 or
// its IPv6 address depending on the OS's per-connection (Happy-Eyeballs) choice,
// so binding to only the mint-time address would reject the sibling family and
// burn a second trusted-device slot for one physical device. We instead carry TWO
// bindings: `ip` (set at mint) and `ip_alt`, the complementary-FAMILY address
// learned the first time the SAME live cookie is validly presented from the other
// family. Match is the OR of the two. The learn is deliberately narrow:
//   - it only fills ip_alt while it is still empty (a device is bound to at most
//     two addresses, ever — one per family);
//   - it only accepts an address of the OTHER family than `ip` (a different
//     SAME-family address — an IPv6 prefix rotation, or a same-network thief — is
//     NOT learned; it falls through to TrustAbsent, exactly as before).
//
// Security note: this relaxes pure IP binding by one address — a stolen cookie
// presented from a different family BEFORE the owner first uses that family can
// seat itself in ip_alt. That is the explicit trade for not wasting a slot on
// dual-stack: the 256-bit token stays the primary secret, same-family theft is
// still rejected, and the abuse tripwire still counts every genuine rejection.
//
// The writes run inside one CTE so the valid-touch (incl. the ip_alt learn) and
// the expired-delete are a single atomic statement — a token is never both
// touched and deleted, and two concurrent sibling-family presentations cannot
// both learn: whichever updates the row first wins under its row lock, and the
// second then matches via the freshly-written ip_alt (or is rejected if it is a
// third, distinct address). The renewal only fires on the valid branch, so an
// already-expired token can never be resurrected by a late presentation. The
// expired-delete is intentionally NOT IP-scoped: reaping a stale row is pure
// hygiene regardless of who presents it.
func (s *TrustedDeviceStore) Check(tokenHash, clientIP, ua string, newExpiry time.Time) (TrustVerdict, error) {
	// Textual family test: an IPv6 literal always contains ':', an IPv4 one never
	// does — cheaper and exception-free versus casting the stored TEXT to inet.
	clientIsV6 := strings.Contains(clientIP, ":")
	var validN, expiredN int
	err := s.db.QueryRow(`
		WITH valid AS (
			UPDATE trusted_devices
			SET last_used = NOW(),
			    expires_at = $3,
			    -- Cosmetic: stamp the latest UA on every valid presentation so the
			    -- management table reflects the browser currently using the cookie.
			    -- Not an auth input — the WHERE below never gates on it.
			    ua = $5,
			    -- Learn the sibling-family address only when we matched via the
			    -- learn branch (neither slot already held clientIP); an existing
			    -- match leaves ip_alt untouched.
			    ip_alt = CASE WHEN ip = $2 OR ip_alt = $2 THEN ip_alt ELSE $2 END
			WHERE token_hash = $1 AND expires_at > NOW()
			  AND (
			        ip = $2 OR ip_alt = $2
			        OR (
			             (ip_alt IS NULL OR ip_alt = '')
			             AND ip IS NOT NULL AND ip <> ''
			             AND (position(':' in ip) > 0) <> $4
			           )
			      )
			RETURNING 1
		),
		expired AS (
			DELETE FROM trusted_devices
			WHERE token_hash = $1 AND expires_at <= NOW()
			RETURNING 1
		)
		SELECT (SELECT COUNT(*) FROM valid), (SELECT COUNT(*) FROM expired)`,
		tokenHash, clientIP, newExpiry, clientIsV6, ua,
	).Scan(&validN, &expiredN)
	if err != nil {
		return TrustAbsent, fmt.Errorf("check trusted device: %w", err)
	}
	switch {
	case validN > 0:
		return TrustValid, nil
	case expiredN > 0:
		return TrustExpired, nil
	default:
		return TrustAbsent, nil
	}
}

// ListActive returns the live trusted devices, newest first, for the settings
// management table.
func (s *TrustedDeviceStore) ListActive() ([]TrustedDevice, error) {
	rows, err := s.db.Query(`
		SELECT id, token_prefix, COALESCE(ip, ''), COALESCE(ua, ''), created_at, last_used, expires_at
		FROM trusted_devices
		WHERE expires_at > NOW()
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list trusted devices: %w", err)
	}
	defer rows.Close()
	var out []TrustedDevice
	for rows.Next() {
		var d TrustedDevice
		if err := rows.Scan(&d.ID, &d.TokenPrefix, &d.IP, &d.UserAgent, &d.CreatedAt, &d.LastUsed, &d.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan trusted device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// HashByID returns the stored token hash for one device row, or "" if no row
// carries that id. Used so a self-revoke can recognise that the row being
// revoked is the caller's own browser (by matching its presented cookie hash)
// and drop that session immediately. Never exposes the hash to any UI.
func (s *TrustedDeviceStore) HashByID(id int64) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT token_hash FROM trusted_devices WHERE id = $1`, id).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("hash by id: %w", err)
	}
	return h, nil
}

// Revoke deletes a trusted device by id (an operator pressing "Revoke").
// Returns how many rows were removed so the caller can tell hit from miss.
func (s *TrustedDeviceStore) Revoke(id int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM trusted_devices WHERE id = $1`, id)
	if err != nil {
		return 0, fmt.Errorf("revoke trusted device: %w", err)
	}
	return res.RowsAffected()
}

// DeleteExpired drops every row past its expiry. Validity is already gated on
// expires_at, so this is pure hygiene — run from the daily sweep.
func (s *TrustedDeviceStore) DeleteExpired() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM trusted_devices WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired trusted devices: %w", err)
	}
	return res.RowsAffected()
}

// DeleteAll drops every trusted-device row — live or expired. Called whenever the
// TOTP second factor is reset or rotated: a trusted cookie outlives the
// secret it was minted under, so resetting the second factor (operator suspects
// compromise) MUST also de-authorise every trusted device, otherwise an attacker
// holding a trusted cookie keeps full /settings access across the reset.
func (s *TrustedDeviceStore) DeleteAll() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM trusted_devices`)
	if err != nil {
		return 0, fmt.Errorf("delete all trusted devices: %w", err)
	}
	return res.RowsAffected()
}
