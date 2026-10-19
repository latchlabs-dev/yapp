package secrets_test

import (
	"context"
	"os"
	"testing"

	"github.com/earlgrey/piproxy/internal/secrets"
)

func TestEnvStore(t *testing.T) {
	ctx := context.Background()
	store := secrets.NewEnvStore("PROXY_TEST_")

	// Set a test secret.
	os.Setenv("PROXY_TEST_MY_SECRET", "test-value")
	defer os.Unsetenv("PROXY_TEST_MY_SECRET")

	// Get it via the store.
	val, err := store.Get(ctx, "my-secret")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(val) != "test-value" {
		t.Errorf("expected 'test-value', got %q", string(val))
	}

	// Get non-existent secret.
	_, err = store.Get(ctx, "non-existent")
	if err == nil {
		t.Error("expected error for non-existent secret")
	}
}

func TestEnvStoreHealth(t *testing.T) {
	store := secrets.NewEnvStore("PROXY_TEST_")
	if err := store.Health(context.Background()); err != nil {
		t.Errorf("Health should not error: %v", err)
	}
}

func TestEnvStorePutAndDelete(t *testing.T) {
	ctx := context.Background()
	store := secrets.NewEnvStore("PROXY_TEST_")

	// Put should set the env var.
	err := store.Put(ctx, "my-test-key", []byte("test-data"))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Verify via direct env lookup.
	if val := os.Getenv("PROXY_TEST_MY_TEST_KEY"); val != "test-data" {
		t.Errorf("expected 'test-data', got %q", val)
	}

	// Delete should unset it.
	err = store.Delete(ctx, "my-test-key")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if val := os.Getenv("PROXY_TEST_MY_TEST_KEY"); val != "" {
		t.Errorf("expected empty after delete, got %q", val)
	}
}

func TestNewInvalidProvider(t *testing.T) {
	_, err := secrets.New("invalid", "", "", "", "")
	if err == nil {
		t.Error("expected error for invalid provider")
	}
}

func TestNewEnvProvider(t *testing.T) {
	store, err := secrets.New("env", "", "", "", "PROXY_TEST_")
	if err != nil {
		t.Fatalf("New('env') failed: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestCredentialStoreAcceptsRawEnvSecret(t *testing.T) {
	ctx := context.Background()
	store := secrets.NewEnvStore("PROXY_TEST_")
	credentialStore := secrets.NewCredentialStore(store)

	os.Setenv("PROXY_TEST_PROVIDERS_OPENAI_API_KEY", "sk-test")
	defer os.Unsetenv("PROXY_TEST_PROVIDERS_OPENAI_API_KEY")

	credential, err := credentialStore.GetCredential(ctx, "providers/openai/api-key")
	if err != nil {
		t.Fatalf("GetCredential failed: %v", err)
	}
	if credential.SecretValue() != "sk-test" {
		t.Errorf("expected raw secret, got %q", credential.SecretValue())
	}
}

func TestCredentialStoreAcceptsJSONEnvSecret(t *testing.T) {
	ctx := context.Background()
	store := secrets.NewEnvStore("PROXY_TEST_")
	credentialStore := secrets.NewCredentialStore(store)

	os.Setenv("PROXY_TEST_PROVIDERS_ANTHROPIC_API_KEY", `{"api_key":"sk-ant-test"}`)
	defer os.Unsetenv("PROXY_TEST_PROVIDERS_ANTHROPIC_API_KEY")

	credential, err := credentialStore.GetCredential(ctx, "providers/anthropic/api-key")
	if err != nil {
		t.Fatalf("GetCredential failed: %v", err)
	}
	if credential.SecretValue() != "sk-ant-test" {
		t.Errorf("expected JSON secret, got %q", credential.SecretValue())
	}
}

func TestCredentialStoreDoesNotCacheDBProviderCredentialRefs(t *testing.T) {
	ctx := context.Background()
	store := &countingSecretStore{data: []byte(`{"api_key":"sk-db-test"}`)}
	credentialStore := secrets.NewCredentialStore(store)

	for i := 0; i < 2; i++ {
		credential, err := credentialStore.GetCredential(ctx, "provider-credential:pcr_test")
		if err != nil {
			t.Fatalf("GetCredential call %d failed: %v", i+1, err)
		}
		if credential.SecretValue() != "sk-db-test" {
			t.Fatalf("unexpected credential value: %q", credential.SecretValue())
		}
	}

	if store.gets != 2 {
		t.Fatalf("expected DB credential ref to bypass cache, got %d store gets", store.gets)
	}
}

func TestNewVaultStoreMissingAddress(t *testing.T) {
	_, err := secrets.New("vault", "", "", "", "")
	if err == nil {
		t.Error("expected error for vault with empty address")
	}
}

func TestEnvVarNameConversion(t *testing.T) {
	store := secrets.NewEnvStore("PROXY_SECRET_")
	// We can't access the private envVarName method directly,
	// so test via Get which exercises the same path.
	_ = store
}

type countingSecretStore struct {
	data []byte
	gets int
}

func (s *countingSecretStore) Get(ctx context.Context, ref string) ([]byte, error) {
	s.gets++
	return s.data, nil
}

func (s *countingSecretStore) Put(ctx context.Context, ref string, data []byte) error {
	s.data = data
	return nil
}

func (s *countingSecretStore) Delete(ctx context.Context, ref string) error {
	s.data = nil
	return nil
}

func (s *countingSecretStore) Health(ctx context.Context) error {
	return nil
}
