package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/blob"
)

// SessionObjectStore is the Postgres blob.Objects backend. It exists so the
// session archive works without a bucket (dev, tests, single-host installs);
// production points the archive at GCS instead.
type SessionObjectStore struct {
	pool *Pool
}

// NewSessionObjectStore returns a session_objects-backed object store.
func NewSessionObjectStore(pool *Pool) *SessionObjectStore {
	return &SessionObjectStore{pool: pool}
}

// Location identifies this backend in stored metadata.
func (s *SessionObjectStore) Location() string { return "postgres" }

// Put stores data under key. Keys are normally write-once; replacing is
// allowed so a retried write is idempotent.
func (s *SessionObjectStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_objects (key, content_type, size_bytes, data)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE SET
			content_type = EXCLUDED.content_type,
			size_bytes = EXCLUDED.size_bytes,
			data = EXCLUDED.data,
			created_at = now()
	`, key, contentType, int64(len(data)), data)
	if err != nil {
		return fmt.Errorf("put session object %s: %w", key, err)
	}
	return nil
}

// Read returns the object bytes or blob.ErrNotFound.
func (s *SessionObjectStore) Read(ctx context.Context, key string) ([]byte, error) {
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT data FROM session_objects WHERE key = $1`, key).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, blob.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read session object %s: %w", key, err)
	}
	return data, nil
}

// Delete removes an object; deleting an absent object is not an error.
func (s *SessionObjectStore) Delete(ctx context.Context, key string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM session_objects WHERE key = $1`, key); err != nil {
		return fmt.Errorf("delete session object %s: %w", key, err)
	}
	return nil
}
