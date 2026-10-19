package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
)

// ErrDomainNotMapped is returned when an OIDC login presents an email whose
// domain has no tenant mapping. Login fails closed: guessing a tenant would
// silently place a person in the wrong organisation's data.
var ErrDomainNotMapped = errors.New("email domain is not mapped to a tenant")

// WebIdentityDomain maps an email domain to a tenant and the scope set granted
// to browser sessions opened by that domain's users.
type WebIdentityDomain struct {
	Domain        string    `json:"domain"`
	TenantID      string    `json:"tenant_id"`
	DefaultScopes []string  `json:"default_scopes"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// WebIdentity binds an OIDC (issuer, subject) pair to a proxy user.
type WebIdentity struct {
	ID          string     `json:"id"`
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"subject"`
	TenantID    string     `json:"tenant_id"`
	UserID      string     `json:"user_id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// WebIdentityStore reads the domain mapping and upserts OIDC identities.
type WebIdentityStore struct {
	pool *Pool
}

// NewWebIdentityStore creates a new WebIdentityStore.
func NewWebIdentityStore(pool *Pool) *WebIdentityStore {
	return &WebIdentityStore{pool: pool}
}

// ListDomains returns every configured domain mapping, newest last.
func (s *WebIdentityStore) ListDomains(ctx context.Context) ([]*WebIdentityDomain, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT domain, tenant_id, default_scopes, status, created_at, updated_at
		   FROM web_identity_domains
		  ORDER BY domain`,
	)
	if err != nil {
		return nil, fmt.Errorf("list web identity domains: %w", err)
	}
	defer rows.Close()

	var result []*WebIdentityDomain
	for rows.Next() {
		d := &WebIdentityDomain{}
		if err := rows.Scan(&d.Domain, &d.TenantID, &d.DefaultScopes, &d.Status, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan web identity domain: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// UpsertDomain creates or replaces a domain mapping.
func (s *WebIdentityStore) UpsertDomain(ctx context.Context, d *WebIdentityDomain) error {
	domain := NormalizeDomain(d.Domain)
	if domain == "" {
		return errors.New("domain is required")
	}
	status := d.Status
	if status == "" {
		status = "active"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO web_identity_domains (domain, tenant_id, default_scopes, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, now(), now())
		 ON CONFLICT (domain) DO UPDATE
		    SET tenant_id      = EXCLUDED.tenant_id,
		        default_scopes = EXCLUDED.default_scopes,
		        status         = EXCLUDED.status,
		        updated_at     = now()`,
		domain, d.TenantID, d.DefaultScopes, status,
	)
	if err != nil {
		return fmt.Errorf("upsert web identity domain %s: %w", domain, err)
	}
	return nil
}

// DeleteDomain removes a domain mapping. Existing sessions are unaffected;
// they carry their own snapshotted scopes and expire on their own schedule.
func (s *WebIdentityStore) DeleteDomain(ctx context.Context, domain string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM web_identity_domains WHERE domain = $1`, NormalizeDomain(domain))
	if err != nil {
		return fmt.Errorf("delete web identity domain %s: %w", domain, err)
	}
	return nil
}

// ResolveDomain returns the active mapping for an email address's domain.
// It returns ErrDomainNotMapped when there is no active mapping, which callers
// surface as a refusal rather than provisioning into a guessed tenant.
func (s *WebIdentityStore) ResolveDomain(ctx context.Context, email string) (*WebIdentityDomain, error) {
	domain := DomainFromEmail(email)
	if domain == "" {
		return nil, ErrDomainNotMapped
	}
	d := &WebIdentityDomain{}
	err := s.pool.QueryRow(ctx,
		`SELECT d.domain, d.tenant_id, d.default_scopes, d.status, d.created_at, d.updated_at
		   FROM web_identity_domains d
		   JOIN tenants t ON t.id = d.tenant_id AND t.status = 'active'
		  WHERE d.domain = $1 AND d.status = 'active'`,
		domain,
	).Scan(&d.Domain, &d.TenantID, &d.DefaultScopes, &d.Status, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDomainNotMapped
	}
	if err != nil {
		return nil, fmt.Errorf("resolve web identity domain %s: %w", domain, err)
	}
	return d, nil
}

// GetBySubject returns the identity bound to an (issuer, subject) pair, or nil
// when the person has not logged in before.
func (s *WebIdentityStore) GetBySubject(ctx context.Context, issuer, subject string) (*WebIdentity, error) {
	i := &WebIdentity{}
	var displayName *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, issuer, subject, tenant_id, user_id, email, display_name, last_login_at, created_at, updated_at
		   FROM web_identities
		  WHERE issuer = $1 AND subject = $2`,
		issuer, subject,
	).Scan(&i.ID, &i.Issuer, &i.Subject, &i.TenantID, &i.UserID, &i.Email, &displayName, &i.LastLoginAt, &i.CreatedAt, &i.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get web identity: %w", err)
	}
	if displayName != nil {
		i.DisplayName = *displayName
	}
	return i, nil
}

// Upsert creates or refreshes the identity for an (issuer, subject) pair and
// stamps last_login_at. The tenant/user binding is set on first insert and is
// deliberately not updated afterwards: a person's proxy user must not move
// tenants because their email address changed.
func (s *WebIdentityStore) Upsert(ctx context.Context, i *WebIdentity) (*WebIdentity, error) {
	if i.ID == "" {
		i.ID = id.New("wid")
	}
	out := &WebIdentity{}
	var displayName *string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO web_identities
		     (id, issuer, subject, tenant_id, user_id, email, display_name, last_login_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now(), now())
		 ON CONFLICT (issuer, subject) DO UPDATE
		    SET email         = EXCLUDED.email,
		        display_name  = EXCLUDED.display_name,
		        last_login_at = now(),
		        updated_at    = now()
		 RETURNING id, issuer, subject, tenant_id, user_id, email, display_name, last_login_at, created_at, updated_at`,
		i.ID, i.Issuer, i.Subject, i.TenantID, i.UserID, i.Email, nullableText(i.DisplayName),
	).Scan(&out.ID, &out.Issuer, &out.Subject, &out.TenantID, &out.UserID, &out.Email, &displayName,
		&out.LastLoginAt, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert web identity: %w", err)
	}
	if displayName != nil {
		out.DisplayName = *displayName
	}
	return out, nil
}

// ListIdentities returns who has signed into the console, newest first. It is
// how an admin confirms a new domain mapping actually works.
func (s *WebIdentityStore) ListIdentities(ctx context.Context, tenantID string, limit int) ([]*WebIdentity, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, issuer, subject, tenant_id, user_id, email, display_name,
		        last_login_at, created_at, updated_at
		   FROM web_identities
		  WHERE ($1::text IS NULL OR tenant_id = $1)
		  ORDER BY last_login_at DESC NULLS LAST
		  LIMIT $2`,
		nullableText(tenantID), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list web identities: %w", err)
	}
	defer rows.Close()

	var result []*WebIdentity
	for rows.Next() {
		i := &WebIdentity{}
		var displayName *string
		if err := rows.Scan(&i.ID, &i.Issuer, &i.Subject, &i.TenantID, &i.UserID, &i.Email,
			&displayName, &i.LastLoginAt, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan web identity: %w", err)
		}
		if displayName != nil {
			i.DisplayName = *displayName
		}
		result = append(result, i)
	}
	return result, rows.Err()
}

// NormalizeDomain lowercases and trims a domain, dropping a leading "@" so
// admins can paste either "example.com" or "@example.com".
func NormalizeDomain(domain string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "@"))
}

// DomainFromEmail extracts the normalized domain part of an email address.
// It returns "" for anything that is not a single-@ address.
func DomainFromEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 1 || at == len(email)-1 {
		return ""
	}
	if strings.Count(email, "@") != 1 {
		return ""
	}
	return NormalizeDomain(email[at+1:])
}
