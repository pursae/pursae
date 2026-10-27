package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetTransient is get_transient(): it decodes the stored JSON into v and reports whether an
// unexpired value exists.
func (s *Store) GetTransient(ctx context.Context, key string, v any) (bool, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT value FROM transients WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if v != nil {
		if err := json.Unmarshal(raw, v); err != nil {
			return false, err
		}
	}
	return true, nil
}

// SetTransient is set_transient(): ttl 0 stores the value without expiry.
func (s *Store) SetTransient(ctx context.Context, key string, v any, ttl time.Duration) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var expires *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expires = &t
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO transients(key, value, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at`, key, raw, expires)
	return err
}

// DeleteTransient is delete_transient().
func (s *Store) DeleteTransient(ctx context.Context, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM transients WHERE key = $1`, key)
	return err
}
