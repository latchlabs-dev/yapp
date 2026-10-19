package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/earlgrey/piproxy/internal/credentialref"
)

const (
	credentialCacheTTL = 5 * time.Minute
	// refreshSkew refreshes an OAuth2 access token this far before its real
	// expiry, to avoid racing the upstream clock.
	refreshSkew = 5 * time.Minute
	// refreshSuccessCacheTTL bounds how long a refreshed DB credential is held
	// in memory after a successful refresh. The DB remains source of truth;
	// this only prevents re-fetching and re-refreshing under bursty traffic.
	refreshSuccessCacheTTL = 5 * time.Minute
	// refreshFailureCacheTTL backs off the token endpoint when a refresh fails,
	// so a persistent outage does not produce one refresh attempt per request.
	refreshFailureCacheTTL = 60 * time.Second
)

// Credential holds a decrypted upstream provider credential.
type Credential struct {
	ProviderType string `json:"provider_type"`
	APIKey       string `json:"api_key,omitempty"`
	BearerToken  string `json:"bearer_token,omitempty"`
	EndpointURL  string `json:"endpoint_url,omitempty"`
	// OAuth2 fields
	AccessToken string `json:"access_token,omitempty"`
	// RefreshToken is the OAuth2 refresh token. For ChatGPT/Codex credentials
	// the refresh token ROTATES on every refresh, so a refreshed Credential
	// must be persisted or the rotated token is lost.
	RefreshToken string `json:"refresh_token,omitempty"`
	// AccountID is the chatgpt-account-id required on the upstream request
	// for Codex (ChatGPT) credentials.
	AccountID     string    `json:"account_id,omitempty"`
	OAuthTokenURL string    `json:"oauth_token_url,omitempty"`
	OAuthClientID string    `json:"oauth_client_id,omitempty"`
	OAuthScope    string    `json:"oauth_scope,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

// SecretValue returns the credential material used for upstream authentication.
func (c *Credential) SecretValue() string {
	for _, candidate := range []string{c.APIKey, c.BearerToken, c.AccessToken} {
		if secret := strings.TrimSpace(candidate); secret != "" {
			return secret
		}
	}
	return ""
}

// NeedsRefresh reports whether the access token is missing or near expiry and
// a refresh token is available to renew it.
func (c *Credential) NeedsRefresh() bool {
	if c.RefreshToken == "" {
		return false
	}
	if c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return false // unknown expiry; assume valid, let upstream reject if not
	}
	return time.Until(c.ExpiresAt) <= refreshSkew
}

// OAuthRefresher refreshes an expired OAuth2 access token stored in a
// Credential (e.g. Codex/ChatGPT refresh-token rotation). Implementations must
// return an updated Credential copy and leave the input untouched.
type OAuthRefresher interface {
	RefreshCredential(ctx context.Context, cred *Credential) (*Credential, error)
}

// CredentialPersister writes a refreshed credential back to its backing store
// (e.g. re-encrypting and updating the DB row in place) so rotated refresh
// tokens survive process restarts. Best-effort: a persistence failure must not
// fail the in-flight request, but it means the rotated token will be lost on
// restart.
type CredentialPersister interface {
	PersistRefreshed(ctx context.Context, ref string, cred *Credential) error
}

// CredentialStore manages upstream provider credentials.
type CredentialStore struct {
	store     SecretStore
	cache     sync.Map // map[string]*cachedCredential
	refresher OAuthRefresher
	persister CredentialPersister
	sf        singleflight.Group
	log       zerolog.Logger
}

// SetOAuthRefresher attaches an OAuth2 refresher (e.g. codexauth.Refresher).
// When set, credentials that carry a refresh token are auto-renewed before
// expiry.
func (cs *CredentialStore) SetOAuthRefresher(r OAuthRefresher) { cs.refresher = r }

// SetPersister attaches a persister used to write back rotated refresh tokens.
func (cs *CredentialStore) SetPersister(p CredentialPersister) { cs.persister = p }

// SetLogger attaches a logger for refresh/persist diagnostics.
func (cs *CredentialStore) SetLogger(log zerolog.Logger) { cs.log = log }

type cachedCredential struct {
	cred      *Credential
	expiresAt time.Time
}

// NewCredentialStore creates a new CredentialStore.
func NewCredentialStore(store SecretStore) *CredentialStore {
	return &CredentialStore{
		store: store,
		log:   zerolog.Nop(),
	}
}

// GetCredential retrieves and decrypts a credential by its secret reference.
func (cs *CredentialStore) GetCredential(ctx context.Context, ref string) (*Credential, error) {
	_, isDBCredential := credentialref.ParseProviderCredential(ref)

	// Check cache first. Non-DB credentials are cached for the short TTL below;
	// DB credentials are cached only by the refresh path (to coalesce refresh
	// bursts and back off the token endpoint), so the DB remains source of
	// truth for revocation and rotation under normal traffic.
	if cached, ok := cs.cache.Load(ref); ok {
		cc := cached.(*cachedCredential)
		if time.Now().Before(cc.expiresAt) {
			return cc.cred, nil
		}
		cs.cache.Delete(ref)
	}

	// Fetch from secret store.
	data, err := cs.store.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("get credential %s: %w", ref, err)
	}

	cred, err := parseCredential(ref, data)
	if err != nil {
		return nil, err
	}

	// Auto-refresh expired OAuth2 credentials (e.g. Codex/ChatGPT tokens).
	// Refresh tokens rotate, so the refreshed credential is persisted and
	// cached; refresh is single-flighted per ref to avoid concurrent rotation.
	if cs.refresher != nil && cred.NeedsRefresh() {
		cred = cs.refreshAndPersist(ctx, ref, cred)
	}

	// Cache non-DB credentials with a short TTL. DB credentials are intentionally
	// not cached here (see note above).
	if !isDBCredential {
		cs.cache.Store(ref, &cachedCredential{
			cred:      cred,
			expiresAt: time.Now().Add(credentialCacheTTL),
		})
	}

	return cred, nil
}

// refreshAndPersist renews an OAuth2 credential, persists the rotated tokens,
// and caches the result. It is single-flighted per ref so concurrent requests
// share one refresh. On refresh failure it returns the (expired) credential and
// caches it briefly to back off the token endpoint; persistence failure is
// non-fatal (the in-memory token is still used for this request) but is logged.
func (cs *CredentialStore) refreshAndPersist(ctx context.Context, ref string, cred *Credential) *Credential {
	v, err, _ := cs.sf.Do(ref, func() (interface{}, error) {
		refreshed, rerr := cs.refresher.RefreshCredential(ctx, cred)
		if rerr != nil {
			return nil, rerr
		}
		if cs.persister != nil {
			if perr := cs.persister.PersistRefreshed(ctx, ref, refreshed); perr != nil {
				// Refresh succeeded but we could not save the rotated refresh
				// token. Use the new access token for this request; on restart
				// the stale (now-invalidated) refresh token will fail.
				cs.log.Warn().Err(perr).Str("ref", ref).
					Msg("credential refreshed but rotated refresh token was NOT persisted; token will break on restart")
			}
		}
		return refreshed, nil
	})

	if v != nil {
		refreshed := v.(*Credential)
		cs.cache.Store(ref, &cachedCredential{
			cred:      refreshed,
			expiresAt: time.Now().Add(refreshSuccessCacheTTL),
		})
		return refreshed
	}

	// Refresh failed: cache the stale credential briefly to avoid hammering
	// the token endpoint on every request during an outage.
	cs.log.Warn().Err(err).Str("ref", ref).Msg("oauth credential refresh failed; using cached credential")
	cs.cache.Store(ref, &cachedCredential{
		cred:      cred,
		expiresAt: time.Now().Add(refreshFailureCacheTTL),
	})
	return cred
}

func parseCredential(ref string, data []byte) (*Credential, error) {
	var cred Credential
	if err := json.Unmarshal(data, &cred); err == nil {
		return &cred, nil
	} else if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return nil, fmt.Errorf("unmarshal credential %s: %w", ref, err)
	}

	var raw string
	if err := json.Unmarshal(data, &raw); err == nil {
		return &Credential{APIKey: raw}, nil
	}

	raw = strings.TrimSpace(string(data))
	if raw == "" {
		return nil, fmt.Errorf("credential %s is empty", ref)
	}
	return &Credential{APIKey: raw}, nil
}

// StoreCredential encrypts and stores a credential.
func (cs *CredentialStore) StoreCredential(ctx context.Context, ref string, cred *Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("marshal credential: %w", err)
	}

	if err := cs.store.Put(ctx, ref, data); err != nil {
		return fmt.Errorf("store credential %s: %w", ref, err)
	}

	// Invalidate cache.
	cs.cache.Delete(ref)
	return nil
}

// DeleteCredential removes a credential.
func (cs *CredentialStore) DeleteCredential(ctx context.Context, ref string) error {
	if err := cs.store.Delete(ctx, ref); err != nil {
		return fmt.Errorf("delete credential %s: %w", ref, err)
	}
	cs.cache.Delete(ref)
	return nil
}

// InvalidateCache removes a credential from the cache.
func (cs *CredentialStore) InvalidateCache(ref string) {
	cs.cache.Delete(ref)
}

// ClearCache removes all cached credentials.
func (cs *CredentialStore) ClearCache() {
	cs.cache.Range(func(key, _ interface{}) bool {
		cs.cache.Delete(key)
		return true
	})
}
