package store

import "database/sql"

// TOTP enrollment persists in site_settings under reserved keys. The leading
// underscore keeps these rows out of SettingsStore.GetAll() (which already
// filters `name NOT LIKE '\_%'`), so a stray template that loops over the
// settings map can never leak the secret.
const (
	totpSecretKey = "_totp_secret"
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
// /settings save (source="user") cannot collide.
func (s *TOTPStore) SetSecret(secret string) error {
	_, err := s.db.Exec(`
		INSERT INTO site_settings (name, value, source)
		VALUES ($1, $2, 'totp')
		ON CONFLICT (name) DO UPDATE SET value = $2, source = 'totp', updated_at = NOW()
	`, totpSecretKey, secret)
	return err
}

// Reset clears the enrolled secret so the next visit triggers re-enrollment.
func (s *TOTPStore) Reset() error {
	_, err := s.db.Exec(`DELETE FROM site_settings WHERE name = $1`, totpSecretKey)
	return err
}
