package webauth

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// The fakes below implement just the store slices SessionManager depends on,
// so provisioning policy can be tested without a database.

type fakeIdentities struct {
	domains    map[string]*pgstore.WebIdentityDomain
	bySubject  map[string]*pgstore.WebIdentity
	upsertCall int
}

func (f *fakeIdentities) ResolveDomain(_ context.Context, email string) (*pgstore.WebIdentityDomain, error) {
	d, ok := f.domains[pgstore.DomainFromEmail(email)]
	if !ok {
		return nil, pgstore.ErrDomainNotMapped
	}
	return d, nil
}

func (f *fakeIdentities) GetBySubject(_ context.Context, issuer, subject string) (*pgstore.WebIdentity, error) {
	return f.bySubject[issuer+"|"+subject], nil
}

func (f *fakeIdentities) Upsert(_ context.Context, i *pgstore.WebIdentity) (*pgstore.WebIdentity, error) {
	f.upsertCall++
	if i.ID == "" {
		i.ID = "wid_test"
	}
	f.bySubject[i.Issuer+"|"+i.Subject] = i
	return i, nil
}

type fakeUsers struct {
	byID       map[string]*pgstore.UserRecord
	byEmail    map[string]*pgstore.UserRecord
	inserted   int
	duplicates int // number of Insert calls to answer with ErrDuplicateUserEmail
}

func (f *fakeUsers) GetByEmail(_ context.Context, tenantID, email string) (*pgstore.UserRecord, error) {
	return f.byEmail[tenantID+"|"+email], nil
}

func (f *fakeUsers) GetByID(_ context.Context, tenantID, userID string) (*pgstore.UserRecord, error) {
	u, ok := f.byID[userID]
	if !ok || u.TenantID != tenantID {
		return nil, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeUsers) Insert(_ context.Context, record *pgstore.UserRecord) error {
	if f.duplicates > 0 {
		f.duplicates--
		// Simulate the racing writer having landed the row first.
		f.byEmail[record.TenantID+"|"+record.Email] = &pgstore.UserRecord{
			ID: "usr_race", TenantID: record.TenantID, Email: record.Email, Status: "active",
		}
		f.byID["usr_race"] = f.byEmail[record.TenantID+"|"+record.Email]
		return pgstore.ErrDuplicateUserEmail
	}
	f.inserted++
	f.byID[record.ID] = record
	f.byEmail[record.TenantID+"|"+record.Email] = record
	return nil
}

type fakeSessions struct {
	rows map[string]*pgstore.WebSession
}

type fakeTenants struct {
	byID map[string]*pgstore.TenantRecord
}

func (f *fakeTenants) GetByID(_ context.Context, tenantID string) (*pgstore.TenantRecord, error) {
	tenant := f.byID[tenantID]
	if tenant == nil || tenant.Status == "deleted" {
		return nil, pgx.ErrNoRows
	}
	return tenant, nil
}

func (f *fakeSessions) Insert(_ context.Context, sess *pgstore.WebSession, tokenHash []byte) error {
	f.rows[string(tokenHash)] = sess
	return nil
}

func (f *fakeSessions) GetByTokenHash(_ context.Context, tokenHash []byte, _ time.Duration) (*pgstore.WebSession, error) {
	sess, ok := f.rows[string(tokenHash)]
	if !ok {
		return nil, pgstore.ErrSessionNotFound
	}
	return sess, nil
}

func (f *fakeSessions) Touch(context.Context, string) error { return nil }
func (f *fakeSessions) Revoke(_ context.Context, id string) error {
	for hash, sess := range f.rows {
		if sess.ID == id {
			delete(f.rows, hash)
		}
	}
	return nil
}

func newTestManager(identities *fakeIdentities, users *fakeUsers, sessions *fakeSessions) *SessionManager {
	tenants := &fakeTenants{byID: map[string]*pgstore.TenantRecord{}}
	for _, domain := range identities.domains {
		tenants.byID[domain.TenantID] = &pgstore.TenantRecord{ID: domain.TenantID, Status: "active"}
	}
	for _, identity := range identities.bySubject {
		tenants.byID[identity.TenantID] = &pgstore.TenantRecord{ID: identity.TenantID, Status: "active"}
	}
	return NewSessionManager(identities, tenants, users, sessions, time.Hour, time.Hour)
}

func testClaims() *Claims {
	return &Claims{
		Issuer:        "https://auth.example.com",
		Subject:       "subject-1",
		Email:         "dev@example.com",
		EmailVerified: true,
		Name:          "Dev Example",
	}
}

func TestLoginProvisionsUserOnFirstSight(t *testing.T) {
	identities := &fakeIdentities{
		domains: map[string]*pgstore.WebIdentityDomain{
			"example.com": {Domain: "example.com", TenantID: "tnt_1", DefaultScopes: []string{"llm:invoke"}},
		},
		bySubject: map[string]*pgstore.WebIdentity{},
	}
	users := &fakeUsers{byID: map[string]*pgstore.UserRecord{}, byEmail: map[string]*pgstore.UserRecord{}}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}

	sess, token, err := newTestManager(identities, users, sessions).
		Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if users.inserted != 1 {
		t.Errorf("inserted %d users, want 1", users.inserted)
	}
	if sess.TenantID != "tnt_1" {
		t.Errorf("tenant = %q, want tnt_1", sess.TenantID)
	}
	if len(sess.Scopes) != 1 || sess.Scopes[0] != "llm:invoke" {
		t.Errorf("scopes = %v, want the domain's default scopes", sess.Scopes)
	}
	if token == "" || sess.CSRFToken == "" {
		t.Error("expected a session token and a CSRF token")
	}
}

// An unmapped domain must be refused outright. Guessing a tenant would put a
// stranger inside another organisation's conversations and memory.
func TestLoginRefusesUnmappedDomain(t *testing.T) {
	identities := &fakeIdentities{domains: map[string]*pgstore.WebIdentityDomain{}, bySubject: map[string]*pgstore.WebIdentity{}}
	users := &fakeUsers{byID: map[string]*pgstore.UserRecord{}, byEmail: map[string]*pgstore.UserRecord{}}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}

	_, _, err := newTestManager(identities, users, sessions).
		Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
	if !errors.Is(err, pgstore.ErrDomainNotMapped) {
		t.Fatalf("err = %v, want ErrDomainNotMapped", err)
	}
	if users.inserted != 0 {
		t.Error("a refused login must not provision a user")
	}
	if len(sessions.rows) != 0 {
		t.Error("a refused login must not mint a session")
	}
}

// Archiving a user has to lock them out of the console, not just revoke their
// API keys.
func TestLoginRefusesArchivedUser(t *testing.T) {
	identities := &fakeIdentities{
		domains: map[string]*pgstore.WebIdentityDomain{
			"example.com": {Domain: "example.com", TenantID: "tnt_1", DefaultScopes: []string{"llm:invoke"}},
		},
		bySubject: map[string]*pgstore.WebIdentity{
			"https://auth.example.com|subject-1": {
				ID: "wid_1", Issuer: "https://auth.example.com", Subject: "subject-1",
				TenantID: "tnt_1", UserID: "usr_gone", Email: "dev@example.com",
			},
		},
	}
	// GetByID filters archived rows, so the user is simply absent.
	users := &fakeUsers{byID: map[string]*pgstore.UserRecord{}, byEmail: map[string]*pgstore.UserRecord{}}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}

	_, _, err := newTestManager(identities, users, sessions).
		Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
	if !errors.Is(err, ErrUserInactive) {
		t.Fatalf("err = %v, want ErrUserInactive", err)
	}
}

// An existing identity binding wins over the domain map, so remapping a domain
// does not move an established person into a different tenant.
func TestLoginKeepsExistingTenantBinding(t *testing.T) {
	identities := &fakeIdentities{
		domains: map[string]*pgstore.WebIdentityDomain{
			"example.com": {Domain: "example.com", TenantID: "tnt_new", DefaultScopes: []string{"llm:invoke"}},
		},
		bySubject: map[string]*pgstore.WebIdentity{
			"https://auth.example.com|subject-1": {
				ID: "wid_1", Issuer: "https://auth.example.com", Subject: "subject-1",
				TenantID: "tnt_original", UserID: "usr_1", Email: "dev@example.com",
			},
		},
	}
	users := &fakeUsers{
		byID: map[string]*pgstore.UserRecord{
			"usr_1": {ID: "usr_1", TenantID: "tnt_original", Email: "dev@example.com", Status: "active"},
		},
		byEmail: map[string]*pgstore.UserRecord{},
	}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}

	sess, _, err := newTestManager(identities, users, sessions).
		Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.TenantID != "tnt_original" {
		t.Errorf("tenant = %q, want tnt_original — a remapped domain must not move an existing identity", sess.TenantID)
	}
	if len(sess.Scopes) != 1 || sess.Scopes[0] != "llm:models:read" {
		t.Errorf("scopes = %v, want the read-only baseline instead of the other tenant's invoke grant", sess.Scopes)
	}
	if users.inserted != 0 {
		t.Error("an existing identity must not provision a second user")
	}
}

// Two simultaneous first logins race on the unique (tenant_id, email) index.
// The loser must read the winner's row back, not fail the login.
func TestLoginRecoversFromProvisioningRace(t *testing.T) {
	identities := &fakeIdentities{
		domains: map[string]*pgstore.WebIdentityDomain{
			"example.com": {Domain: "example.com", TenantID: "tnt_1", DefaultScopes: []string{"llm:invoke"}},
		},
		bySubject: map[string]*pgstore.WebIdentity{},
	}
	users := &fakeUsers{
		byID:       map[string]*pgstore.UserRecord{},
		byEmail:    map[string]*pgstore.UserRecord{},
		duplicates: 1,
	}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}

	sess, _, err := newTestManager(identities, users, sessions).
		Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.UserID != "usr_race" {
		t.Errorf("user = %q, want the row the racing writer created", sess.UserID)
	}
}

func TestResolveAndLogout(t *testing.T) {
	identities := &fakeIdentities{
		domains: map[string]*pgstore.WebIdentityDomain{
			"example.com": {Domain: "example.com", TenantID: "tnt_1", DefaultScopes: []string{"llm:invoke"}},
		},
		bySubject: map[string]*pgstore.WebIdentity{},
	}
	users := &fakeUsers{byID: map[string]*pgstore.UserRecord{}, byEmail: map[string]*pgstore.UserRecord{}}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}
	manager := newTestManager(identities, users, sessions)

	_, token, err := manager.Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := manager.Resolve(context.Background(), token); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := manager.Logout(context.Background(), token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := manager.Resolve(context.Background(), token); !errors.Is(err, pgstore.ErrSessionNotFound) {
		t.Fatalf("err = %v, want ErrSessionNotFound after logout", err)
	}
}

func TestDomainFromEmail(t *testing.T) {
	cases := map[string]string{
		"dev@Example.COM":  "example.com",
		"dev@example.com":  "example.com",
		"no-at-sign":       "",
		"two@at@signs.com": "",
		"trailing@":        "",
		"@leading.com":     "",
	}
	for input, want := range cases {
		if got := pgstore.DomainFromEmail(input); got != want {
			t.Errorf("DomainFromEmail(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoginRefusesInactiveTenantBeforeProvisioning(t *testing.T) {
	for _, returning := range []bool{false, true} {
		for _, status := range []string{"suspended", "deleted", "missing"} {
			t.Run(fmt.Sprintf("returning=%t/%s", returning, status), func(t *testing.T) {
				identities := &fakeIdentities{
					domains: map[string]*pgstore.WebIdentityDomain{
						"example.com": {Domain: "example.com", TenantID: "tnt_1", DefaultScopes: []string{"llm:invoke"}},
					},
					bySubject: map[string]*pgstore.WebIdentity{},
				}
				users := &fakeUsers{byID: map[string]*pgstore.UserRecord{}, byEmail: map[string]*pgstore.UserRecord{}}
				if returning {
					identities.bySubject["https://auth.example.com|subject-1"] = &pgstore.WebIdentity{ID: "wid_1", TenantID: "tnt_1", UserID: "usr_1"}
					users.byID["usr_1"] = &pgstore.UserRecord{ID: "usr_1", TenantID: "tnt_1", Status: "active"}
				}
				tenants := &fakeTenants{byID: map[string]*pgstore.TenantRecord{}}
				if status != "missing" {
					tenants.byID["tnt_1"] = &pgstore.TenantRecord{ID: "tnt_1", Status: status}
				}
				sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}
				manager := NewSessionManager(identities, tenants, users, sessions, time.Hour, time.Hour)
				_, _, err := manager.Login(context.Background(), testClaims(), httptest.NewRequest("GET", "/", nil))
				if !errors.Is(err, ErrTenantInactive) {
					t.Fatalf("login error = %v, want ErrTenantInactive", err)
				}
				if users.inserted != 0 || identities.upsertCall != 0 || len(sessions.rows) != 0 {
					t.Fatal("inactive tenant login provisioned a user, identity, or session")
				}
			})
		}
	}
}

func TestLoginEmailChangeCannotImportOtherTenantScopes(t *testing.T) {
	identities := &fakeIdentities{
		domains: map[string]*pgstore.WebIdentityDomain{
			"new-company.example": {Domain: "new-company.example", TenantID: "tnt_other", DefaultScopes: []string{"memory:observe", "llm:invoke"}},
		},
		bySubject: map[string]*pgstore.WebIdentity{
			"https://auth.example.com|subject-1": {ID: "wid_1", TenantID: "tnt_original", UserID: "usr_1"},
		},
	}
	users := &fakeUsers{
		byID:    map[string]*pgstore.UserRecord{"usr_1": {ID: "usr_1", TenantID: "tnt_original", Status: "active"}},
		byEmail: map[string]*pgstore.UserRecord{},
	}
	sessions := &fakeSessions{rows: map[string]*pgstore.WebSession{}}
	claims := testClaims()
	claims.Email = "dev@new-company.example"
	sess, _, err := newTestManager(identities, users, sessions).Login(context.Background(), claims, httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if sess.TenantID != "tnt_original" || sess.UserID != "usr_1" {
		t.Fatalf("existing binding moved: tenant=%q user=%q", sess.TenantID, sess.UserID)
	}
	if len(sess.Scopes) != 1 || sess.Scopes[0] != "llm:models:read" {
		t.Fatalf("foreign domain grants leaked: %v", sess.Scopes)
	}
}
