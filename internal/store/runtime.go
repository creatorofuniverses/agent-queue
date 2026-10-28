package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SaveRuntime stores value under key, replacing an earlier one.
func (s *Store) SaveRuntime(ctx context.Context, key string, value []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runtime_state (key, value, saved_at) VALUES (?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, saved_at=excluded.saved_at`, key, string(value), fmtTime(at))
	return err
}

// LoadRuntime returns the value saved under key; ok is false when there is none.
func (s *Store) LoadRuntime(ctx context.Context, key string) (value []byte, ok bool, err error) {
	var v string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM runtime_state WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return []byte(v), err == nil, err
}

// DeleteRuntime removes key; a missing key is not an error.
func (s *Store) DeleteRuntime(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_state WHERE key=?`, key)
	return err
}
