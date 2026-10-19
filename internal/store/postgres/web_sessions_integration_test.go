package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/id"
)

type webSessionFixture struct {
	pool       *Pool
	tenant     *TenantRecord
	user       *UserRecord
	identityID string
	sessions   *WebSessionStore
}

func newWebSessionFixture(t *testing.T) *webSessionFixture {
	t.Helper()
	pool := openIntegrationPool(t)
	ctx := context.Background()
	now := time.Now()
	fixture := &webSessionFixture{
		pool:       pool,
		tenant:     &TenantRecord{ID: id.New("tn_web_auth_test"), Name: "Web auth test", Status: "active", CreatedAt: now, UpdatedAt: now},
		identityID: id.New("wid_web_auth_test"),
		sessions:   NewWebSessionStore(pool),
	}
	fixture.user = &UserRecord{ID: id.New("usr_web_auth_test"), TenantID: fixture.tenant.ID, Email: "dev@example.test", Status: "active", CreatedAt: now, UpdatedAt: now}
	if err := NewTenantStore(pool).Insert(ctx, fixture.tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM web_sessions WHERE tenant_id = $1`, fixture.tenant.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM web_identities WHERE tenant_id = $1`, fixture.tenant.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM web_identity_domains WHERE tenant_id = $1`, fixture.tenant.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE tenant_id = $1`, fixture.tenant.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, fixture.tenant.ID)
	})
	if err := NewUserStore(pool).Insert(ctx, fixture.user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO web_identities (id, issuer, subject, tenant_id, user_id, email)
		VALUES ($1, 'https://auth.example.test', $1, $2, $3, 'dev@example.test')`, fixture.identityID, fixture.tenant.ID, fixture.user.ID); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *webSessionFixture) newSession() (*WebSession, []byte) {
	session := &WebSession{
		ID: id.New("wss_web_auth_test"), TenantID: f.tenant.ID, UserID: f.user.ID, IdentityID: f.identityID,
		Scopes: []string{"llm:invoke", "memory:observe"}, CSRFToken: "test-csrf", ExpiresAt: time.Now().Add(time.Hour),
	}
	hash := sha256.Sum256([]byte(session.ID))
	return session, hash[:]
}

func TestWebSessionResolutionRequiresCurrentActivePrincipals(t *testing.T) {
	for _, principal := range []string{"user", "tenant"} {
		t.Run(principal, func(t *testing.T) {
			f := newWebSessionFixture(t)
			ctx := context.Background()
			session, hash := f.newSession()
			if err := f.sessions.Insert(ctx, session, hash); err != nil {
				t.Fatal(err)
			}
			for _, status := range []string{"suspended", "deleted"} {
				query, principalID := `UPDATE users SET status=$2 WHERE id=$1`, f.user.ID
				if principal == "tenant" {
					query, principalID = `UPDATE tenants SET status=$2 WHERE id=$1`, f.tenant.ID
				}
				if _, err := f.pool.Exec(ctx, query, principalID, status); err != nil {
					t.Fatal(err)
				}
				// Direct status updates deliberately leave revoked_at untouched,
				// proving resolution consults the current principals itself.
				if _, err := f.sessions.GetByTokenHash(ctx, hash, 0); !errors.Is(err, ErrSessionNotFound) {
					t.Fatalf("%s %s session error = %v, want ErrSessionNotFound", principal, status, err)
				}
				if _, err := f.pool.Exec(ctx, query, principalID, "active"); err != nil {
					t.Fatal(err)
				}
				resolved, err := f.sessions.GetByTokenHash(ctx, hash, 0)
				if err != nil || len(resolved.Scopes) != 2 || resolved.Scopes[1] != "memory:observe" {
					t.Fatalf("active principal did not retain its scope snapshot: session=%+v err=%v", resolved, err)
				}
			}
		})
	}
}

func changeWebPrincipal(ctx context.Context, f *webSessionFixture, operation string) error {
	switch operation {
	case "suspend_user":
		user := *f.user
		user.Status = "suspended"
		return NewUserStore(f.pool).Update(ctx, &user)
	case "archive_user":
		return NewUserStore(f.pool).Delete(ctx, f.tenant.ID, f.user.ID)
	case "suspend_tenant":
		tenant := *f.tenant
		tenant.Status = "suspended"
		return NewTenantStore(f.pool).Update(ctx, &tenant)
	default:
		return NewTenantStore(f.pool).Delete(ctx, f.tenant.ID)
	}
}

func TestWebSessionLifecycleRevocationSurvivesReactivation(t *testing.T) {
	for _, operation := range []string{"suspend_user", "archive_user", "suspend_tenant", "archive_tenant"} {
		t.Run(operation, func(t *testing.T) {
			f := newWebSessionFixture(t)
			ctx := context.Background()
			session, hash := f.newSession()
			if err := f.sessions.Insert(ctx, session, hash); err != nil {
				t.Fatal(err)
			}
			if err := changeWebPrincipal(ctx, f, operation); err != nil {
				t.Fatal(err)
			}
			var revoked bool
			if err := f.pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM web_sessions WHERE id=$1`, session.ID).Scan(&revoked); err != nil || !revoked {
				t.Fatalf("lifecycle did not persist revocation: revoked=%t err=%v", revoked, err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE tenants SET status='active' WHERE id=$1`, f.tenant.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE users SET status='active' WHERE id=$1`, f.user.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.sessions.GetByTokenHash(ctx, hash, 0); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("reactivation resurrected the revoked session: %v", err)
			}
		})
	}
}

func TestWebSessionMintCannotRaceLifecycleChanges(t *testing.T) {
	for _, operation := range []string{"suspend_user", "archive_user", "suspend_tenant", "archive_tenant"} {
		t.Run(operation, func(t *testing.T) {
			f := newWebSessionFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for attempt := 0; attempt < 12; attempt++ {
				if _, err := f.pool.Exec(ctx, `UPDATE tenants SET status='active' WHERE id=$1`, f.tenant.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.pool.Exec(ctx, `UPDATE users SET status='active' WHERE id=$1`, f.user.ID); err != nil {
					t.Fatal(err)
				}
				session, hash := f.newSession()
				start := make(chan struct{})
				minted, changed := make(chan error, 1), make(chan error, 1)
				go func() { <-start; minted <- f.sessions.Insert(ctx, session, hash) }()
				go func() { <-start; changed <- changeWebPrincipal(ctx, f, operation) }()
				close(start)
				mintErr, changeErr := <-minted, <-changed
				if changeErr != nil {
					t.Fatal(changeErr)
				}
				if mintErr != nil && !errors.Is(mintErr, ErrSessionNotFound) {
					t.Fatalf("concurrent mint failed unexpectedly: %v", mintErr)
				}
				if mintErr == nil {
					var revoked bool
					if err := f.pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM web_sessions WHERE id=$1`, session.ID).Scan(&revoked); err != nil || !revoked {
						t.Fatalf("mint won without subsequent revocation: revoked=%t err=%v", revoked, err)
					}
				}
				if _, err := f.sessions.GetByTokenHash(ctx, hash, 0); !errors.Is(err, ErrSessionNotFound) {
					t.Fatalf("concurrent lifecycle left a live session: %v", err)
				}
			}
		})
	}
}

func TestWebIdentityDomainRefusesInactiveTenant(t *testing.T) {
	f := newWebSessionFixture(t)
	ctx := context.Background()
	store := NewWebIdentityStore(f.pool)
	domain := id.New("auth-domain") + ".example.test"
	if err := store.UpsertDomain(ctx, &WebIdentityDomain{Domain: domain, TenantID: f.tenant.ID, DefaultScopes: []string{"llm:invoke"}, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"suspended", "deleted"} {
		if _, err := f.pool.Exec(ctx, `UPDATE tenants SET status=$2 WHERE id=$1`, f.tenant.ID, status); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ResolveDomain(ctx, "fresh@"+domain); !errors.Is(err, ErrDomainNotMapped) {
			t.Fatalf("%s tenant domain resolved: %v", status, err)
		}
	}
}
