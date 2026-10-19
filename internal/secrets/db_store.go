package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/earlgrey/piproxy/internal/credentialref"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// EncryptedDBStore reads encrypted provider credentials from Postgres.
type EncryptedDBStore struct {
	credentials *pgstore.ProviderCredentialStore
	encryptor   *Encryptor
}

// NewEncryptedDBStore creates a secret store backed by provider_credentials.encrypted_blob.
func NewEncryptedDBStore(credentials *pgstore.ProviderCredentialStore, encryptor *Encryptor) *EncryptedDBStore {
	return &EncryptedDBStore{credentials: credentials, encryptor: encryptor}
}

// Get returns the decrypted credential payload for provider-credential:<id> refs.
func (s *EncryptedDBStore) Get(ctx context.Context, ref string) ([]byte, error) {
	id, ok := credentialref.ParseProviderCredential(ref)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	if s == nil || s.credentials == nil {
		return nil, fmt.Errorf("%w: encrypted credential store is not configured", ErrProvider)
	}
	if s.encryptor == nil {
		return nil, fmt.Errorf("%w: credential encryption key is not configured", ErrProvider)
	}

	record, err := s.credentials.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get DB credential %s: %w", id, err)
	}
	if record.Status != "active" {
		return nil, fmt.Errorf("%w: DB credential %s is %s", ErrNotFound, id, record.Status)
	}
	if len(record.EncryptedBlob) == 0 {
		return nil, fmt.Errorf("%w: DB credential %s has no encrypted blob", ErrNotFound, id)
	}

	plaintext, err := s.encryptor.Decrypt(record.EncryptedBlob)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// Put is intentionally unsupported; admin writes DB credentials through ProviderCredentialStore.
func (s *EncryptedDBStore) Put(ctx context.Context, ref string, data []byte) error {
	return fmt.Errorf("%w: encrypted DB credential writes use the admin API", ErrProvider)
}

// Delete is intentionally unsupported; admin revokes DB credentials instead.
func (s *EncryptedDBStore) Delete(ctx context.Context, ref string) error {
	return fmt.Errorf("%w: encrypted DB credential deletes use revocation", ErrProvider)
}

// Health returns nil because DB health is checked through the Postgres pool.
func (s *EncryptedDBStore) Health(ctx context.Context) error {
	return nil
}

// FallbackStore tries the primary store, then falls back on ErrNotFound.
type FallbackStore struct {
	Primary   SecretStore
	Secondary SecretStore
}

// NewFallbackStore creates a store that falls back to secondary only for missing refs.
func NewFallbackStore(primary, secondary SecretStore) *FallbackStore {
	return &FallbackStore{Primary: primary, Secondary: secondary}
}

func (s *FallbackStore) Get(ctx context.Context, ref string) ([]byte, error) {
	if s.Primary != nil {
		data, err := s.Primary.Get(ctx, ref)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, ErrNotFound) || s.Secondary == nil {
			return nil, err
		}
	}
	if s.Secondary != nil {
		return s.Secondary.Get(ctx, ref)
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

func (s *FallbackStore) Put(ctx context.Context, ref string, data []byte) error {
	if _, ok := credentialref.ParseProviderCredential(ref); ok && s.Primary != nil {
		return s.Primary.Put(ctx, ref, data)
	}
	if s.Secondary != nil {
		return s.Secondary.Put(ctx, ref, data)
	}
	return fmt.Errorf("%w: no writable secret store configured", ErrProvider)
}

func (s *FallbackStore) Delete(ctx context.Context, ref string) error {
	if _, ok := credentialref.ParseProviderCredential(ref); ok && s.Primary != nil {
		return s.Primary.Delete(ctx, ref)
	}
	if s.Secondary != nil {
		return s.Secondary.Delete(ctx, ref)
	}
	return fmt.Errorf("%w: no writable secret store configured", ErrProvider)
}

func (s *FallbackStore) Health(ctx context.Context) error {
	if s.Primary != nil {
		if err := s.Primary.Health(ctx); err != nil {
			return err
		}
	}
	if s.Secondary != nil {
		return s.Secondary.Health(ctx)
	}
	return nil
}
