package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/credentialref"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// CredentialFingerprint returns a short, stable, non-reversible fingerprint of
// a secret for display and dedup. It is the first 16 hex chars of sha256(secret).
func CredentialFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:16]
}

// dbCredentialPersister persists refreshed credentials by re-encrypting the
// credential JSON and updating the provider_credentials row in place, keeping
// the credential ID (and thus the provider's CredentialID ref) stable. This is
// required because Codex/ChatGPT refresh tokens ROTATE: the new refresh token
// must be written back or it is lost (and the old one is already invalidated).
type dbCredentialPersister struct {
	credentials *pgstore.ProviderCredentialStore
	encryptor   *Encryptor
	log         zerolog.Logger
}

// NewDBCredentialPersister wraps a ProviderCredentialStore + Encryptor into a
// CredentialPersister that writes refreshed credentials back to Postgres.
func NewDBCredentialPersister(credentials *pgstore.ProviderCredentialStore, encryptor *Encryptor, log zerolog.Logger) CredentialPersister {
	return &dbCredentialPersister{credentials: credentials, encryptor: encryptor, log: log}
}

// PersistRefreshed re-encrypts cred and updates its DB row in place.
func (p *dbCredentialPersister) PersistRefreshed(ctx context.Context, ref string, cred *Credential) error {
	if p == nil || p.credentials == nil || p.encryptor == nil {
		return fmt.Errorf("persist: db persister not configured")
	}
	id, ok := credentialref.ParseProviderCredential(ref)
	if !ok {
		return fmt.Errorf("persist: not a provider-credential ref: %s", ref)
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("persist: marshal credential: %w", err)
	}
	blob, err := p.encryptor.Encrypt(data)
	if err != nil {
		return fmt.Errorf("persist: encrypt credential: %w", err)
	}
	secret := cred.SecretValue()
	if secret == "" {
		return fmt.Errorf("persist: credential %s has no secret material", id)
	}
	if err := p.credentials.UpdateEncryptedBlob(ctx, id, blob, CredentialFingerprint(secret)); err != nil {
		return fmt.Errorf("persist: update credential %s: %w", id, err)
	}
	p.log.Info().Str("credential_id", id).Msg("refreshed credential persisted (rotated refresh token saved)")
	return nil
}
