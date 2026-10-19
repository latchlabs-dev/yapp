package webauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/auth"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

const keyCookiePrefix = "key1."
const keyCookiePurpose = "piproxy-web-key-v1"

type keyStore interface {
	GetByPrefix(context.Context, string) (*pgstore.KeyRecord, error)
}

type revocationChecker interface {
	IsRevoked(context.Context, string) bool
}

type cookieCipher interface {
	Encrypt([]byte) ([]byte, error)
	Decrypt([]byte) ([]byte, error)
}

// KeySessions keeps an existing personal API key in an authenticated, encrypted
// cookie, never in a new database row. The key and its principals are checked on
// EVERY request, so revocation, expiry, suspension and scope edits apply at once.
// Cookies have an absolute TTL; unlike OIDC sessions they have no idle tracking.
// Logout clears the browser cookie without revoking the user's underlying key.
// A copied cookie remains a bearer credential until expiry or key revocation.
type KeySessions struct {
	keys    keyStore
	tenants tenantStore
	users   userStore
	revoked revocationChecker
	cipher  cookieCipher
	ttl     time.Duration
}

func NewKeySessions(keys keyStore, tenants tenantStore, users userStore, revoked revocationChecker, cipher cookieCipher, ttl time.Duration) *KeySessions {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &KeySessions{keys: keys, tenants: tenants, users: users, revoked: revoked, cipher: cipher, ttl: ttl}
}

type keyCookie struct {
	Purpose string    `json:"purpose"`
	Key     string    `json:"key"`
	CSRF    string    `json:"csrf"`
	Expires time.Time `json:"expires"`
}

// Login validates a key before sealing it; neither the key nor cookie is ever
// returned in a JSON response. Only the Set-Cookie header carries the envelope.
func (m *KeySessions) Login(ctx context.Context, rawKey string) (*pgstore.WebSession, string, error) {
	key, err := m.authenticate(ctx, rawKey)
	if err != nil {
		return nil, "", err
	}
	csrf, err := NewCSRFToken()
	if err != nil {
		return nil, "", err
	}
	expires := time.Now().Add(m.ttl)
	if key.ExpiresAt != nil && key.ExpiresAt.Before(expires) {
		expires = *key.ExpiresAt
	}
	plain, err := json.Marshal(keyCookie{Purpose: keyCookiePurpose, Key: rawKey, CSRF: csrf, Expires: expires})
	if err != nil {
		return nil, "", err
	}
	sealed, err := m.cipher.Encrypt(plain)
	if err != nil {
		return nil, "", err
	}
	return keySession(key, csrf, expires), keyCookiePrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (m *KeySessions) Resolve(ctx context.Context, token string) (*pgstore.WebSession, error) {
	if !strings.HasPrefix(token, keyCookiePrefix) || len(token) > 4096 {
		return nil, pgstore.ErrSessionNotFound
	}
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, keyCookiePrefix))
	if err != nil {
		return nil, pgstore.ErrSessionNotFound
	}
	plain, err := m.cipher.Decrypt(sealed)
	if err != nil {
		return nil, pgstore.ErrSessionNotFound
	}
	var cookie keyCookie
	if json.Unmarshal(plain, &cookie) != nil || cookie.Purpose != keyCookiePurpose || cookie.CSRF == "" || !time.Now().Before(cookie.Expires) {
		return nil, pgstore.ErrSessionNotFound
	}
	key, err := m.authenticate(ctx, cookie.Key)
	if err != nil {
		return nil, err
	}
	return keySession(key, cookie.CSRF, cookie.Expires), nil
}

func (m *KeySessions) authenticate(ctx context.Context, rawKey string) (*pgstore.KeyRecord, error) {
	if len(rawKey) > 256 {
		return nil, pgstore.ErrSessionNotFound
	}
	prefix := auth.PrefixFromKey(rawKey)
	if prefix == "" || (m.revoked != nil && m.revoked.IsRevoked(ctx, prefix)) {
		return nil, pgstore.ErrSessionNotFound
	}
	key, err := m.keys.GetByPrefix(ctx, prefix)
	if err != nil || key == nil || !auth.VerifyKey(rawKey, key.KeyHash) || key.Status != "active" || key.RevokedAt != nil ||
		(key.ExpiresAt != nil && !time.Now().Before(*key.ExpiresAt)) || key.TenantID == "" || key.UserID == "" {
		return nil, pgstore.ErrSessionNotFound
	}
	// Personal history must belong to an existing active user in the key's
	// tenant. Service/tenant-only keys cannot be assigned a guessed owner.
	tenant, err := m.tenants.GetByID(ctx, key.TenantID)
	if err != nil || tenant == nil || tenant.ID != key.TenantID || tenant.Status != "active" {
		return nil, pgstore.ErrSessionNotFound
	}
	user, err := m.users.GetByID(ctx, key.TenantID, key.UserID)
	if err != nil || user == nil || user.ID != key.UserID || user.TenantID != key.TenantID || user.Status != "active" {
		return nil, pgstore.ErrSessionNotFound
	}
	return key, nil
}

func keySession(key *pgstore.KeyRecord, csrf string, expires time.Time) *pgstore.WebSession {
	if key.ExpiresAt != nil && key.ExpiresAt.Before(expires) {
		expires = *key.ExpiresAt
	}
	return &pgstore.WebSession{
		TenantID: key.TenantID, UserID: key.UserID, KeyID: key.ID, KeyPrefix: key.Prefix,
		Scopes: append([]string(nil), key.Scopes...), CSRFToken: csrf, ExpiresAt: expires,
	}
}
