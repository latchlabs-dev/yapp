package webauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/secrets"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeKeys struct {
	record *pgstore.KeyRecord
	err    error
}

func (f *fakeKeys) GetByPrefix(_ context.Context, prefix string) (*pgstore.KeyRecord, error) {
	if f.err != nil || f.record == nil || f.record.Prefix != prefix {
		return nil, errors.New("not found")
	}
	return f.record, nil
}

type fakeRevocations struct{ revoked bool }

func (f *fakeRevocations) IsRevoked(context.Context, string) bool { return f.revoked }

func keyFixture(t *testing.T) (*KeySessions, string, *fakeKeys, *fakeTenants, *fakeUsers, *fakeRevocations) {
	t.Helper()
	raw, prefix, hash, err := auth.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	keys := &fakeKeys{record: &pgstore.KeyRecord{ID: "key_1", Prefix: prefix, KeyHash: hash, TenantID: "tenant_1", UserID: "user_1", Status: "active", Scopes: []string{auth.ScopeLLMInvoke}}}
	tenants := &fakeTenants{byID: map[string]*pgstore.TenantRecord{"tenant_1": {ID: "tenant_1", Status: "active"}}}
	users := &fakeUsers{byID: map[string]*pgstore.UserRecord{"user_1": {ID: "user_1", TenantID: "tenant_1", Status: "active"}}}
	revoked := &fakeRevocations{}
	cipher, err := secrets.NewEncryptor([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return NewKeySessions(keys, tenants, users, revoked, cipher, time.Hour), raw, keys, tenants, users, revoked
}

func TestKeyCookieEncryptedRoundtripAndCurrentPermissions(t *testing.T) {
	m, raw, keys, _, _, _ := keyFixture(t)
	ctx := context.Background()
	sess, cookie, err := m.Login(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cookie, raw) || sess.CSRFToken == "" || sess.KeyID != keys.record.ID {
		t.Fatal("key leaked or identity/CSRF missing")
	}
	_, second, err := m.Login(ctx, raw)
	if err != nil || cookie == second {
		t.Fatal("each login needs fresh encryption and CSRF")
	}
	keys.record.Scopes = []string{auth.ScopeLLMModelsRead}
	resolved, err := m.Resolve(ctx, cookie)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.KeyID != sess.KeyID || resolved.KeyPrefix != keys.record.Prefix || resolved.CSRFToken != sess.CSRFToken || len(resolved.Scopes) != 1 || resolved.Scopes[0] != auth.ScopeLLMModelsRead {
		t.Fatalf("cookie did not preserve identity and refresh scopes: %+v", resolved)
	}
}

func TestKeyCookieRejectsLiveAccessChanges(t *testing.T) {
	for _, change := range []string{"revoked", "disabled", "unknown status", "revoked timestamp", "cache", "expired", "hash changed", "user suspended", "user archived", "tenant suspended", "tenant archived", "key-only", "wrong tenant", "database failure"} {
		t.Run(change, func(t *testing.T) {
			m, raw, keys, tenants, users, revoked := keyFixture(t)
			_, cookie, err := m.Login(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "revoked":
				keys.record.Status = "revoked"
			case "disabled":
				keys.record.Status = "disabled"
			case "unknown status":
				keys.record.Status = "unknown"
			case "revoked timestamp":
				now := time.Now()
				keys.record.RevokedAt = &now
			case "cache":
				revoked.revoked = true
			case "expired":
				past := time.Now().Add(-time.Second)
				keys.record.ExpiresAt = &past
			case "hash changed":
				keys.record.KeyHash = "different"
			case "user suspended":
				users.byID["user_1"].Status = "suspended"
			case "user archived":
				delete(users.byID, "user_1")
			case "tenant suspended":
				tenants.byID["tenant_1"].Status = "suspended"
			case "tenant archived":
				delete(tenants.byID, "tenant_1")
			case "key-only":
				keys.record.UserID = ""
			case "wrong tenant":
				users.byID["user_1"].TenantID = "other"
			case "database failure":
				keys.err = errors.New("database unavailable")
			}
			if _, err := m.Resolve(context.Background(), cookie); !errors.Is(err, pgstore.ErrSessionNotFound) {
				t.Fatalf("access change ignored: %v", err)
			}
			if _, _, err := m.Login(context.Background(), raw); !errors.Is(err, pgstore.ErrSessionNotFound) {
				t.Fatalf("invalid key login allowed: %v", err)
			}
		})
	}
}

func TestKeyCookieRejectsTamperingExpiryAndWrongPurpose(t *testing.T) {
	m, raw, _, _, _, _ := keyFixture(t)
	ctx := context.Background()
	_, token, err := m.Login(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, keyCookiePrefix))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)/2] ^= 0x80
	for _, bad := range []string{"", raw, "key1.not-base64!", keyCookiePrefix + strings.Repeat("x", 5000), keyCookiePrefix + base64.RawURLEncoding.EncodeToString(sealed)} {
		if _, err := m.Resolve(ctx, bad); !errors.Is(err, pgstore.ErrSessionNotFound) {
			t.Fatal("unsealed or tampered cookie accepted")
		}
	}
	for _, payload := range []keyCookie{
		{Purpose: keyCookiePurpose, Key: raw, CSRF: "csrf", Expires: time.Now().Add(-time.Minute)},
		{Purpose: "provider-credential", Key: raw, CSRF: "csrf", Expires: time.Now().Add(time.Hour)},
		{Purpose: keyCookiePurpose, Key: raw, Expires: time.Now().Add(time.Hour)},
	} {
		plain, _ := json.Marshal(payload)
		sealed, err := m.cipher.Encrypt(plain)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Resolve(ctx, keyCookiePrefix+base64.RawURLEncoding.EncodeToString(sealed)); !errors.Is(err, pgstore.ErrSessionNotFound) {
			t.Fatal("invalid sealed payload accepted")
		}
	}
}

func TestKeySessionExpiryAndOIDCDispatch(t *testing.T) {
	m, raw, keys, _, _, _ := keyFixture(t)
	expires := time.Now().Add(5 * time.Minute)
	keys.record.ExpiresAt = &expires
	sess, cookie, err := m.Login(context.Background(), raw)
	if err != nil || !sess.ExpiresAt.Equal(expires) {
		t.Fatalf("key expiry not honored: %v", err)
	}
	manager := &SessionManager{}
	if _, err := manager.Resolve(context.Background(), cookie); !errors.Is(err, pgstore.ErrSessionNotFound) {
		t.Fatal("disabled key login accepted cookie")
	}
	manager.EnableKeyLogin(m)
	if _, err := manager.Resolve(context.Background(), cookie); err != nil {
		t.Fatal(err)
	}
	if err := manager.Logout(context.Background(), cookie); err != nil {
		t.Fatal(err)
	}
	if keys.record.Status != "active" {
		t.Fatal("browser logout revoked CLI key")
	}
}

func TestSessionCookieFlagsAndClear(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, CookieOptions{}, "sealed", time.Hour)
	cookies := rec.Result().Cookies()
	cookie := cookies[len(cookies)-1]
	if cookie.Name != SessionCookie || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" || cookie.Path != "/" || cookie.MaxAge != 3600 {
		t.Fatalf("unsafe cookie attributes: %+v", cookie)
	}
	rec = httptest.NewRecorder()
	ClearSessionCookie(rec, CookieOptions{})
	cleared := rec.Result().Cookies()[0]
	if cleared.Name != cookie.Name || cleared.Path != cookie.Path || cleared.MaxAge != -1 || cleared.Value != "" {
		t.Fatal("logout does not clear matching cookie")
	}
}
