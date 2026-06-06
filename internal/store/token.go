package store

import (
	"database/sql"
	"fmt"
)

type TokenStore struct {
	db *sql.DB
}

func NewTokenStore(db *sql.DB) *TokenStore {
	return &TokenStore{db: db}
}

func (s *TokenStore) Token() (*StoredToken, error) {
	t := &StoredToken{}
	err := s.db.QueryRow(`
		SELECT id, client_id, client_secret, access_token, expires_at,
		       rate_remaining, rate_reset_at, backend, enabled, last_used, created_at, headers_json
		FROM oauth_tokens
		WHERE enabled = TRUE
		ORDER BY rate_remaining DESC NULLS LAST
		LIMIT 1`,
	).Scan(
		&t.ID, &t.ClientID, &t.ClientSecret, &t.AccessToken, &t.ExpiresAt,
		&t.RateRemaining, &t.RateResetAt, &t.Backend, &t.Enabled, &t.LastUsed, &t.CreatedAt, &t.HeadersJSON,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	return t, nil
}

func (s *TokenStore) UpdateToken(token *StoredToken) error {
	_, err := s.db.Exec(`
		UPDATE oauth_tokens SET
			access_token   = $2,
			expires_at     = $3,
			rate_remaining = $4,
			rate_reset_at  = $5,
			last_used      = $6,
			headers_json   = $7
		WHERE id = $1`,
		token.ID, token.AccessToken, token.ExpiresAt,
		token.RateRemaining, token.RateResetAt, token.LastUsed, token.HeadersJSON,
	)
	if err != nil {
		return fmt.Errorf("update token: %w", err)
	}
	return nil
}

func (s *TokenStore) ListEnabled() ([]*StoredToken, error) {
	rows, err := s.db.Query(`
		SELECT id, client_id, client_secret, access_token, expires_at,
		       rate_remaining, rate_reset_at, backend, enabled, last_used, created_at, headers_json
		FROM oauth_tokens
		WHERE enabled = TRUE
		ORDER BY rate_remaining DESC NULLS LAST`)
	if err != nil {
		return nil, fmt.Errorf("list enabled tokens: %w", err)
	}
	defer rows.Close()
	return scanTokens(rows)
}

func (s *TokenStore) Upsert(token *StoredToken) error {
	_, err := s.db.Exec(`
		INSERT INTO oauth_tokens (client_id, client_secret, access_token, expires_at,
		                          rate_remaining, rate_reset_at, backend, enabled, headers_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			client_id      = EXCLUDED.client_id,
			client_secret  = EXCLUDED.client_secret,
			access_token   = EXCLUDED.access_token,
			expires_at     = EXCLUDED.expires_at,
			rate_remaining = EXCLUDED.rate_remaining,
			rate_reset_at  = EXCLUDED.rate_reset_at,
			backend        = EXCLUDED.backend,
			enabled        = EXCLUDED.enabled,
			headers_json   = EXCLUDED.headers_json`,
		token.ClientID, token.ClientSecret, token.AccessToken, token.ExpiresAt,
		token.RateRemaining, token.RateResetAt, token.Backend, token.Enabled, token.HeadersJSON,
	)
	if err != nil {
		return fmt.Errorf("upsert token: %w", err)
	}
	return nil
}

func (s *TokenStore) DeleteExpiredByBackend(backend string) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM oauth_tokens
		WHERE backend = $1 AND expires_at IS NOT NULL AND expires_at < NOW()`,
		backend,
	)
	if err != nil {
		return 0, fmt.Errorf("delete expired tokens: %w", err)
	}
	return res.RowsAffected()
}

func (s *TokenStore) CountByBackend(backend string) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM oauth_tokens
		WHERE backend = $1 AND enabled = TRUE`,
		backend,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count tokens by backend: %w", err)
	}
	return count, nil
}

func scanTokens(rows *sql.Rows) ([]*StoredToken, error) {
	var tokens []*StoredToken
	for rows.Next() {
		t := &StoredToken{}
		if err := rows.Scan(
			&t.ID, &t.ClientID, &t.ClientSecret, &t.AccessToken, &t.ExpiresAt,
			&t.RateRemaining, &t.RateResetAt, &t.Backend, &t.Enabled, &t.LastUsed, &t.CreatedAt, &t.HeadersJSON,
		); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
