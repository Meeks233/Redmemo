package store

import (
	"database/sql"
	"time"
)

type SettingsStore struct {
	db *sql.DB
}

func NewSettingsStore(db *sql.DB) *SettingsStore {
	return &SettingsStore{db: db}
}

func (s *SettingsStore) GetAll() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value FROM site_settings WHERE name NOT LIKE '\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		m[name] = value
	}
	return m, rows.Err()
}

func (s *SettingsStore) Get(name string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM site_settings WHERE name = $1`, name).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// GetMeta returns a row's value and its last-updated timestamp, found=false if
// absent. Used by the managed-settings reconcile to compare the user shadow and
// env shadow by recency.
func (s *SettingsStore) GetMeta(name string) (value string, updatedAt time.Time, found bool, err error) {
	err = s.db.QueryRow(`SELECT value, updated_at FROM site_settings WHERE name = $1`, name).Scan(&value, &updatedAt)
	if err == sql.ErrNoRows {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return value, updatedAt, true, nil
}

// SetShadow upserts a hidden bookkeeping row with an EXPLICIT updated_at — unlike
// SetBatch, which always stamps NOW(). The reconcile pass uses this to seed an
// env value's first observation at the epoch (oldest) and to stamp a genuine env
// change at the current time, so latest-writer-wins can order the two sides.
func (s *SettingsStore) SetShadow(name, value string, at time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO site_settings (name, value, source, updated_at)
		VALUES ($1, $2, 'shadow', $3)
		ON CONFLICT (name) DO UPDATE SET value = $2, source = 'shadow', updated_at = $3
	`, name, value, at.UTC())
	return err
}

// Delete removes a single settings row. Used to "forget" a remembered-query
// fallback key when the user clears the corresponding box, so a later restore
// pass won't resurrect an intentionally-disabled feature. Missing rows are a
// no-op, not an error.
func (s *SettingsStore) Delete(name string) error {
	_, err := s.db.Exec(`DELETE FROM site_settings WHERE name = $1`, name)
	return err
}

func (s *SettingsStore) SetBatch(settings map[string]string, source string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO site_settings (name, value, source)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET value = $2, source = $3, updated_at = NOW()
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for name, value := range settings {
		if _, err := stmt.Exec(name, value, source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetBatchIfLowerPriority writes settings only when the existing row has a
// lower-priority source (or doesn't exist). Priority: env_override > legacy_sync > default.
func (s *SettingsStore) SetBatchIfLowerPriority(settings map[string]string, source string) (updated int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO site_settings (name, value, source)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET value = $2, source = $3, updated_at = NOW()
		WHERE site_settings.source NOT IN ('env_override')
		   OR $3 = 'env_override'
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for name, value := range settings {
		res, err := stmt.Exec(name, value, source)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	return updated, tx.Commit()
}

// DemoteOrphans downgrades env_override rows whose env var has been removed.
// Called with the set of cookie names that currently have env vars.
// Orphaned rows get source changed to "legacy_sync" so future syncs can update them.
func (s *SettingsStore) DemoteOrphans(activeEnvKeys map[string]string) (int, error) {
	// Exclude hidden bookkeeping rows ("_user_*", "_env_*"): they are not
	// env_override values to begin with, and demoting them would corrupt the
	// managed-settings reconcile state.
	rows, err := s.db.Query(`SELECT name FROM site_settings WHERE source = 'env_override' AND name NOT LIKE '\_%'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var orphans []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		if _, stillActive := activeEnvKeys[name]; !stillActive {
			orphans = append(orphans, name)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(orphans) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`UPDATE site_settings SET source = 'demoted', updated_at = NOW() WHERE name = $1`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, name := range orphans {
		if _, err := stmt.Exec(name); err != nil {
			return 0, err // deferred tx.Rollback undoes the partial batch
		}
	}
	return len(orphans), tx.Commit()
}
