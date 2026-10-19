package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSessionNotFound is returned when a cookie does not match a live session.
// It covers "no such token", "revoked", and "expired" alike so a caller cannot
// distinguish them and probe for valid tokens.
var ErrSessionNotFound = errors.New("web session not found")

// WebSession is the resolved browser identity. KeyID/KeyPrefix are populated
// only for key cookies and are not persisted in the OIDC web_sessions table.
type WebSession struct {
	ID         string
	TenantID   string
	UserID     string
	IdentityID string
	KeyID      string
	KeyPrefix  string
	Scopes     []string
	CSRFToken  string
	ClientIP   string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// WebSessionStore persists browser sessions. Only the SHA-256 of each session
// token is stored, so the table cannot be replayed if it leaks.
type WebSessionStore struct {
	pool *Pool
}

// NewWebSessionStore creates a new WebSessionStore.
func NewWebSessionStore(pool *Pool) *WebSessionStore {
	return &WebSessionStore{pool: pool}
}

// Insert stores a new session only while its tenant and user are active. Shared
// row locks serialize minting with lifecycle changes: suspension either waits
// for this insert and revokes it, or wins first and makes this insert fail.
func (s *WebSessionStore) Insert(ctx context.Context, sess *WebSession, tokenHash []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("insert web session begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Tenant first is the same lock order used by tenant archival.
	var principalID string
	err = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1 AND status = 'active' FOR SHARE`, sess.TenantID).Scan(&principalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("insert web session tenant: %w", err)
	}
	err = tx.QueryRow(ctx, `
		SELECT u.id FROM users u
		WHERE u.id = $1 AND u.tenant_id = $2 AND u.status = 'active'
		  AND EXISTS (SELECT 1 FROM web_identities i WHERE i.id = $3 AND i.user_id = u.id AND i.tenant_id = u.tenant_id)
		FOR SHARE OF u`, sess.UserID, sess.TenantID, sess.IdentityID).Scan(&principalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("insert web session user: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO web_sessions
		     (id, token_hash, tenant_id, user_id, identity_id, scopes, csrf_token,
		      client_ip, user_agent, created_at, last_seen_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), now(), $10)`,
		sess.ID, tokenHash, sess.TenantID, sess.UserID, sess.IdentityID, sess.Scopes,
		sess.CSRFToken, nullableInet(sess.ClientIP), nullableText(sess.UserAgent), sess.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("insert web session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("insert web session commit: %w", err)
	}
	return nil
}

// GetByTokenHash returns the live session for a token hash. A session is live
// only when its tenant and user are active, it is unrevoked, within its absolute
// expiry, and has been seen within idleTTL. Passing idleTTL <= 0 disables the
// idle check; it never disables the principal checks.
func (s *WebSessionStore) GetByTokenHash(ctx context.Context, tokenHash []byte, idleTTL time.Duration) (*WebSession, error) {
	sess := &WebSession{}
	var clientIP, userAgent *string
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.tenant_id, s.user_id, s.identity_id, s.scopes, s.csrf_token,
		        host(s.client_ip), s.user_agent, s.created_at, s.last_seen_at, s.expires_at
		   FROM web_sessions s
		   JOIN tenants t ON t.id = s.tenant_id AND t.status = 'active'
		   JOIN users u ON u.id = s.user_id AND u.tenant_id = s.tenant_id AND u.status = 'active'
		   JOIN web_identities i ON i.id = s.identity_id AND i.tenant_id = s.tenant_id AND i.user_id = s.user_id
		  WHERE s.token_hash = $1
		    AND s.revoked_at IS NULL
		    AND s.expires_at > now()
		    AND ($2::float8 IS NULL OR s.last_seen_at > now() - make_interval(secs => $2::float8))`,
		tokenHash, nullableSeconds(idleTTL),
	).Scan(&sess.ID, &sess.TenantID, &sess.UserID, &sess.IdentityID, &sess.Scopes, &sess.CSRFToken,
		&clientIP, &userAgent, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get web session: %w", err)
	}
	if clientIP != nil {
		sess.ClientIP = *clientIP
	}
	if userAgent != nil {
		sess.UserAgent = *userAgent
	}
	return sess, nil
}

// Touch advances last_seen_at so an active session keeps sliding forward.
// Callers run this off the request path; a lost touch only shortens a session.
func (s *WebSessionStore) Touch(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE web_sessions SET last_seen_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("touch web session: %w", err)
	}
	return nil
}

// Revoke marks a single session revoked (logout).
func (s *WebSessionStore) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("revoke web session: %w", err)
	}
	return nil
}

// RevokeAllForUser revokes every live session for a user. Used when an admin
// archives the user or rotates their access.
func (s *WebSessionStore) RevokeAllForUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE web_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("revoke web sessions for user %s: %w", userID, err)
	}
	return nil
}

// DeleteExpired removes sessions that expired or were revoked more than a day
// ago. Returns the number of rows removed.
func (s *WebSessionStore) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM web_sessions
		  WHERE expires_at < now() - INTERVAL '1 day'
		     OR (revoked_at IS NOT NULL AND revoked_at < now() - INTERVAL '1 day')`,
	)
	if err != nil {
		return 0, fmt.Errorf("delete expired web sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// nullableSeconds renders a duration as seconds for make_interval(), or NULL
// when it is not positive so the caller's SQL can skip the check entirely.
// Seconds rather than an interval literal: Go's Duration.String() ("2h0m0s")
// is not a format Postgres parses reliably.
func nullableSeconds(d time.Duration) interface{} {
	if d <= 0 {
		return nil
	}
	return d.Seconds()
}
