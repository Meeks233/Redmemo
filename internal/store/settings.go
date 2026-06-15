package store

import "database/sql"

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
	rows, err := s.db.Query(`SELECT name FROM site_settings WHERE source = 'env_override'`)
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
