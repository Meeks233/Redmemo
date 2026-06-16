package store

import (
	"database/sql"
	"fmt"
	"time"
)

// TrustedDevice is one persisted long-lived "trusted device" session — the
// 365-day cookie minted when the operator ticks "Trust this device" on the
// TOTP gate. Only the token's SHA-256 is stored (TokenHash is never read back
// out by the management UI); TokenPrefix is the cosmetic first-few-chars label.
type TrustedDevice struct {
	ID          int64
	TokenPrefix string
	IP          string
	CreatedAt   time.Time
	LastUsed    *time.Time
	ExpiresAt   time.Time
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
func (s *TrustedDeviceStore) Create(tokenHash, prefix, ip string, expiresAt time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO trusted_devices (token_hash, token_prefix, ip, expires_at)
		VALUES ($1, $2, $3, $4)`,
		tokenHash, prefix, ip, expiresAt,
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

// Check verifies a presented token hash and self-heals in one round-trip:
//   - a live row is touched (last_used = NOW) and reported TrustValid;
//   - an expired row is deleted on the spot and reported TrustExpired (so an
//     invalid-but-uncleaned token is reaped exactly when it is presented);
//   - a missing row is reported TrustAbsent.
//
// The two writes run inside one CTE so the valid-touch and expired-delete are a
// single atomic statement — a token is never both touched and deleted.
func (s *TrustedDeviceStore) Check(tokenHash string) (TrustVerdict, error) {
	var validN, expiredN int
	err := s.db.QueryRow(`
		WITH valid AS (
			UPDATE trusted_devices SET last_used = NOW()
			WHERE token_hash = $1 AND expires_at > NOW()
			RETURNING 1
		),
		expired AS (
			DELETE FROM trusted_devices
			WHERE token_hash = $1 AND expires_at <= NOW()
			RETURNING 1
		)
		SELECT (SELECT COUNT(*) FROM valid), (SELECT COUNT(*) FROM expired)`,
		tokenHash,
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
		SELECT id, token_prefix, COALESCE(ip, ''), created_at, last_used, expires_at
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
		if err := rows.Scan(&d.ID, &d.TokenPrefix, &d.IP, &d.CreatedAt, &d.LastUsed, &d.ExpiresAt); err != nil {
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
