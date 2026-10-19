// Package secrets provides an abstraction for securely storing and retrieving
// sensitive credentials. Supports Vault, AWS Secrets Manager, GCP Secret Manager,
// and an environment-variable-based fallback for development.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Common errors.
var (
	ErrNotFound      = errors.New("secret not found")
	ErrAlreadyExists = errors.New("secret already exists")
	ErrAccessDenied  = errors.New("access denied to secret")
	ErrProvider      = errors.New("secret provider error")
)

// SecretStore is the interface for storing and retrieving secrets.
type SecretStore interface {
	// Get retrieves a secret by reference.
	Get(ctx context.Context, ref string) ([]byte, error)

	// Put stores a secret and returns its reference.
	Put(ctx context.Context, ref string, data []byte) error

	// Delete removes a secret.
	Delete(ctx context.Context, ref string) error

	// Health checks connectivity to the secret store.
	Health(ctx context.Context) error
}

// New creates a SecretStore based on the provider name.
func New(provider, address, token, namespace, envPrefix string) (SecretStore, error) {
	switch strings.ToLower(provider) {
	case "env":
		return NewEnvStore(envPrefix), nil
	case "vault":
		return NewVaultStore(address, token, namespace)
	case "aws":
		return NewAWSStore()
	case "gcp":
		return NewGCPStore()
	default:
		return nil, fmt.Errorf("unknown secret provider: %s", provider)
	}
}

// EnvStore reads secrets from environment variables. For development only.
type EnvStore struct {
	prefix string
}

// NewEnvStore creates a new EnvStore.
func NewEnvStore(prefix string) *EnvStore {
	return &EnvStore{prefix: prefix}
}

// Get reads a secret from an environment variable.
// The ref is converted: "secret/proxy/key" → "PROXY_SECRET_SECRET_PROXY_KEY"
func (s *EnvStore) Get(_ context.Context, ref string) ([]byte, error) {
	key := s.envVarName(ref)
	val, ok := os.LookupEnv(key)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return []byte(val), nil
}

// Put stores a secret as an environment variable. This is a no-op for
// setting env vars at runtime (they only affect child processes).
// In development, use .env files instead.
func (s *EnvStore) Put(_ context.Context, ref string, data []byte) error {
	key := s.envVarName(ref)
	return os.Setenv(key, string(data))
}

// Delete removes a secret from the environment.
func (s *EnvStore) Delete(_ context.Context, ref string) error {
	key := s.envVarName(ref)
	return os.Unsetenv(key)
}

// Health always returns nil for the env store.
func (s *EnvStore) Health(_ context.Context) error {
	return nil
}

func (s *EnvStore) envVarName(ref string) string {
	// Convert "secret/proxy/key" → "PROXY_SECRET_SECRET_PROXY_KEY"
	normalized := strings.ToUpper(strings.ReplaceAll(ref, "/", "_"))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	return s.prefix + normalized
}

// VaultStore is a no-op stub for Vault integration.
// Full implementation will use hashicorp/vault/api.
type VaultStore struct {
	address   string
	token     string
	namespace string
}

// NewVaultStore creates a new VaultStore.
func NewVaultStore(address, token, namespace string) (*VaultStore, error) {
	if address == "" {
		return nil, fmt.Errorf("%w: vault address is required", ErrProvider)
	}
	return &VaultStore{
		address:   address,
		token:     token,
		namespace: namespace,
	}, nil
}

func (s *VaultStore) Get(ctx context.Context, ref string) ([]byte, error) {
	// Stub: return a fake secret for development.
	if os.Getenv("PROXY_VAULT_STUB") == "1" {
		return []byte("stub-secret-for-" + ref), nil
	}
	return nil, fmt.Errorf("%w: vault not yet implemented: %s", ErrProvider, ref)
}

func (s *VaultStore) Put(ctx context.Context, ref string, data []byte) error {
	return fmt.Errorf("%w: vault not yet implemented", ErrProvider)
}

func (s *VaultStore) Delete(ctx context.Context, ref string) error {
	return fmt.Errorf("%w: vault not yet implemented", ErrProvider)
}

func (s *VaultStore) Health(ctx context.Context) error {
	return nil // Stub
}

// AWSStore is a no-op stub for AWS Secrets Manager integration.
type AWSStore struct{}

func NewAWSStore() (*AWSStore, error) {
	return &AWSStore{}, nil
}

func (s *AWSStore) Get(ctx context.Context, ref string) ([]byte, error) {
	return nil, fmt.Errorf("%w: AWS Secrets Manager not yet implemented", ErrProvider)
}

func (s *AWSStore) Put(ctx context.Context, ref string, data []byte) error {
	return fmt.Errorf("%w: AWS Secrets Manager not yet implemented", ErrProvider)
}

func (s *AWSStore) Delete(ctx context.Context, ref string) error {
	return fmt.Errorf("%w: AWS Secrets Manager not yet implemented", ErrProvider)
}

func (s *AWSStore) Health(ctx context.Context) error {
	return nil
}

// GCPStore is a no-op stub for GCP Secret Manager integration.
type GCPStore struct{}

func NewGCPStore() (*GCPStore, error) {
	return &GCPStore{}, nil
}

func (s *GCPStore) Get(ctx context.Context, ref string) ([]byte, error) {
	return nil, fmt.Errorf("%w: GCP Secret Manager not yet implemented", ErrProvider)
}

func (s *GCPStore) Put(ctx context.Context, ref string, data []byte) error {
	return fmt.Errorf("%w: GCP Secret Manager not yet implemented", ErrProvider)
}

func (s *GCPStore) Delete(ctx context.Context, ref string) error {
	return fmt.Errorf("%w: GCP Secret Manager not yet implemented", ErrProvider)
}

func (s *GCPStore) Health(ctx context.Context) error {
	return nil
}
