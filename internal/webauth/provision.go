package webauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// ErrUserInactive is returned when the resolved proxy user has been archived.
// An admin archiving a user must lock them out of the console too, not just
// revoke their API keys.
var ErrUserInactive = errors.New("user account is not active")

// ErrTenantInactive prevents both new and returning identities from signing
// into an organisation whose access has been suspended or archived.
var ErrTenantInactive = errors.New("tenant account is not active")

// identityStore is the narrow slice of the identity store the provisioner
// needs, kept as an interface so tests can drive it without Postgres.
type identityStore interface {
	ResolveDomain(ctx context.Context, email string) (*pgstore.WebIdentityDomain, error)
	GetBySubject(ctx context.Context, issuer, subject string) (*pgstore.WebIdentity, error)
	Upsert(ctx context.Context, i *pgstore.WebIdentity) (*pgstore.WebIdentity, error)
}

type userStore interface {
	GetByEmail(ctx context.Context, tenantID, email string) (*pgstore.UserRecord, error)
	GetByID(ctx context.Context, tenantID, userID string) (*pgstore.UserRecord, error)
	Insert(ctx context.Context, record *pgstore.UserRecord) error
}

type tenantStore interface {
	GetByID(ctx context.Context, tenantID string) (*pgstore.TenantRecord, error)
}

type sessionStore interface {
	Insert(ctx context.Context, sess *pgstore.WebSession, tokenHash []byte) error
	GetByTokenHash(ctx context.Context, tokenHash []byte, idleTTL time.Duration) (*pgstore.WebSession, error)
	Touch(ctx context.Context, sessionID string) error
	Revoke(ctx context.Context, sessionID string) error
}

// SessionManager mints OIDC sessions and resolves browser cookies. Optional
// KeySessions resolve existing-key cookies without provisioning an identity.
type SessionManager struct {
	identities identityStore
	tenants    tenantStore
	users      userStore
	sessions   sessionStore
	ttl        time.Duration
	idleTTL    time.Duration
	keys       *KeySessions
}

// NewSessionManager wires the stores and lifetimes for browser sessions.
func NewSessionManager(identities identityStore, tenants tenantStore, users userStore, sessions sessionStore, ttl, idleTTL time.Duration) *SessionManager {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionManager{
		identities: identities,
		tenants:    tenants,
		users:      users,
		sessions:   sessions,
		ttl:        ttl,
		idleTTL:    idleTTL,
	}
}

// EnableKeyLogin adds encrypted API-key cookies to the session resolver. Call
// only during startup, before serving requests; OIDC remains independent.
func (m *SessionManager) EnableKeyLogin(keys *KeySessions) { m.keys = keys }

// TTL is the absolute session lifetime, used to set the cookie expiry.
func (m *SessionManager) TTL() time.Duration { return m.ttl }

// Login resolves claims to a tenant and user, provisioning the user on first
// sight, and mints a session. It returns the raw session token, which the
// caller writes to the cookie and never persists.
//
// Order matters: an existing identity binding wins over the domain map, so a
// person whose email domain is later remapped stays with the tenant that owns
// their history rather than silently moving.
func (m *SessionManager) Login(ctx context.Context, claims *Claims, r *http.Request) (*pgstore.WebSession, string, error) {
	domain, err := m.identities.ResolveDomain(ctx, claims.Email)
	if err != nil && !errors.Is(err, pgstore.ErrDomainNotMapped) {
		return nil, "", err
	}

	existing, err := m.identities.GetBySubject(ctx, claims.Issuer, claims.Subject)
	if err != nil {
		return nil, "", err
	}

	var tenantID, userID string
	switch {
	case existing != nil:
		tenantID, userID = existing.TenantID, existing.UserID
	case domain != nil:
		tenantID = domain.TenantID
	default:
		// No prior binding and no domain mapping: refuse rather than guess a
		// tenant. Guessing would place this person in someone else's data.
		return nil, "", pgstore.ErrDomainNotMapped
	}

	// Check the resolved tenant before provisioning: a domain row can outlive a
	// soft-deleted tenant, and the identity binding can outlive a domain mapping.
	tenant, err := m.tenants.GetByID(ctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (tenant == nil || tenant.Status != "active")) {
		return nil, "", ErrTenantInactive
	}
	if err != nil {
		return nil, "", err
	}
	if existing == nil {
		userID, err = m.resolveOrCreateUser(ctx, tenantID, claims)
		if err != nil {
			return nil, "", err
		}
	}

	// GetByID filters archived users, so a missing row here means the account
	// was archived after the identity was bound — a lockout, not a lookup bug.
	user, err := m.users.GetByID(ctx, tenantID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrUserInactive
	}
	if err != nil {
		return nil, "", err
	}
	if user == nil || user.Status != "active" {
		return nil, "", ErrUserInactive
	}

	identity, err := m.identities.Upsert(ctx, &pgstore.WebIdentity{
		Issuer:      claims.Issuer,
		Subject:     claims.Subject,
		TenantID:    tenantID,
		UserID:      userID,
		Email:       claims.Email,
		DisplayName: claims.Name,
	})
	if err != nil {
		return nil, "", err
	}

	return m.mint(ctx, identity, scopesFor(domain, identity), r)
}

// resolveOrCreateUser finds the tenant's user for this email, creating one on
// first login. The unique (tenant_id, email) constraint makes the create
// idempotent under a race: a duplicate means someone else just made it, so we
// read it back rather than failing the login.
func (m *SessionManager) resolveOrCreateUser(ctx context.Context, tenantID string, claims *Claims) (string, error) {
	user, err := m.users.GetByEmail(ctx, tenantID, claims.Email)
	if err != nil {
		return "", err
	}
	if user != nil {
		return user.ID, nil
	}

	now := time.Now()
	record := &pgstore.UserRecord{
		ID:          id.New("usr"),
		TenantID:    tenantID,
		Email:       claims.Email,
		DisplayName: claims.Name,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	err = m.users.Insert(ctx, record)
	if errors.Is(err, pgstore.ErrDuplicateUserEmail) {
		if user, err = m.users.GetByEmail(ctx, tenantID, claims.Email); err != nil {
			return "", err
		}
		if user == nil {
			return "", fmt.Errorf("provision user %s: duplicate email with no readable row", claims.Email)
		}
		return user.ID, nil
	}
	if err != nil {
		return "", err
	}
	return record.ID, nil
}

func (m *SessionManager) mint(ctx context.Context, identity *pgstore.WebIdentity, scopes []string, r *http.Request) (*pgstore.WebSession, string, error) {
	token, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	csrf, err := NewCSRFToken()
	if err != nil {
		return nil, "", err
	}
	sess := &pgstore.WebSession{
		ID:         id.New("wss"),
		TenantID:   identity.TenantID,
		UserID:     identity.UserID,
		IdentityID: identity.ID,
		Scopes:     scopes,
		CSRFToken:  csrf,
		ClientIP:   clientIP(r),
		UserAgent:  truncate(r.UserAgent(), 512),
		ExpiresAt:  time.Now().Add(m.ttl),
	}
	if err := m.sessions.Insert(ctx, sess, HashToken(token)); err != nil {
		return nil, "", err
	}
	return sess, token, nil
}

// Resolve returns the live session for a raw cookie token and slides its
// last-seen timestamp forward. The touch is best-effort: losing it only
// shortens the session, so it must not fail the request. The session store
// checks the current tenant and user status in the same read as the token;
// OIDC scopes remain the login snapshot. Key cookies revalidate live scopes.
func (m *SessionManager) Resolve(ctx context.Context, token string) (*pgstore.WebSession, error) {
	if strings.HasPrefix(token, keyCookiePrefix) {
		if m.keys == nil {
			return nil, pgstore.ErrSessionNotFound
		}
		return m.keys.Resolve(ctx, token)
	}
	if token == "" {
		return nil, pgstore.ErrSessionNotFound
	}
	sess, err := m.sessions.GetByTokenHash(ctx, HashToken(token), m.idleTTL)
	if err != nil {
		return nil, err
	}
	if time.Since(sess.LastSeenAt) > time.Minute {
		_ = m.sessions.Touch(ctx, sess.ID)
	}
	return sess, nil
}

// Logout revokes a session by its raw cookie token.
func (m *SessionManager) Logout(ctx context.Context, token string) error {
	// A key cookie is stateless. Clearing it logs this browser out, not the
	// user's CLI or other devices; revoking the API key invalidates all copies.
	if strings.HasPrefix(token, keyCookiePrefix) {
		return nil
	}
	if token == "" {
		return nil
	}
	sess, err := m.sessions.GetByTokenHash(ctx, HashToken(token), 0)
	if errors.Is(err, pgstore.ErrSessionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.sessions.Revoke(ctx, sess.ID)
}

// scopesFor grants domain capabilities only inside that domain's tenant. A
// returning identity retains its original tenant after an email/domain change,
// but a missing or differently scoped mapping grants only the read-only baseline.
func scopesFor(domain *pgstore.WebIdentityDomain, identity *pgstore.WebIdentity) []string {
	if domain != nil && identity != nil && domain.TenantID == identity.TenantID && len(domain.DefaultScopes) > 0 {
		return append([]string(nil), domain.DefaultScopes...)
	}
	return []string{"llm:models:read"}
}

func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
