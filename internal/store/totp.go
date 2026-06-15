package store

import "database/sql"

// TOTP enrollment persists in site_settings under reserved keys. The leading
// underscore keeps these rows out of SettingsStore.GetAll() (which already
// filters `name NOT LIKE '\_%'`), so a stray template that loops over the
// settings map can never leak the secret.
const (
	totpSecretKey = "_totp_secret"
	// totpConfirmedKey marks that the enrolled secret has been verified by the
	// operator entering a code for it. A persisted secret WITHOUT this marker
	// means enrollment was interrupted after the QR was shown — the gate then
	// re-shows the QR instead of stranding the owner at a bare code prompt.
	totpConfirmedKey = "_totp_confirmed"
)

type TOTPStore struct {
	db *sql.DB
}

func NewTOTPStore(db *sql.DB) *TOTPStore {
	return &TOTPStore{db: db}
}

// Secret returns the enrolled TOTP secret, or "" if enrollment has not
// happened yet (or was reset).
func (s *TOTPStore) Secret() (string, error) {
	var v string
	err := s.db.QueryRow(
		`SELECT value FROM site_settings WHERE name = $1`, totpSecretKey,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSecret stores the enrollment secret. Source is fixed to "totp" so a
// /settings save (source="user") cannot collide. A newly set or rotated secret
// is unconfirmed until a code is entered for it, so any prior confirmed marker
// is cleared first — that keeps the gate re-showing the QR if this fresh
// enrollment is interrupted before the first code.
func (s *TOTPStore) SetSecret(secret string) error {
	if _, err := s.db.Exec(`DELETE FROM site_settings WHERE name = $1`, totpConfirmedKey); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO site_settings (name, value, source)
		VALUES ($1, $2, 'totp')
		ON CONFLICT (name) DO UPDATE SET value = $2, source = 'totp', updated_at = NOW()
	`, totpSecretKey, secret)
	return err
}

// Confirmed reports whether the enrolled secret has been verified end-to-end
// (the operator entered a valid code for it during enrollment).
func (s *TOTPStore) Confirmed() (bool, error) {
	var v string
	err := s.db.QueryRow(
		`SELECT value FROM site_settings WHERE name = $1`, totpConfirmedKey,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// MarkConfirmed records that the enrolled secret has been verified end-to-end.
func (s *TOTPStore) MarkConfirmed() error {
	_, err := s.db.Exec(`
		INSERT INTO site_settings (name, value, source)
		VALUES ($1, '1', 'totp')
		ON CONFLICT (name) DO UPDATE SET value = '1', source = 'totp', updated_at = NOW()
	`, totpConfirmedKey)
	return err
}

// Reset clears the enrolled secret (and its confirmed marker) so the next visit
// triggers a fresh enrollment.
func (s *TOTPStore) Reset() error {
	_, err := s.db.Exec(`DELETE FROM site_settings WHERE name IN ($1, $2)`, totpSecretKey, totpConfirmedKey)
	return err
}
