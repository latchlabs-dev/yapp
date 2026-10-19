package secrets_test

import (
	"bytes"
	"testing"

	"github.com/earlgrey/piproxy/internal/secrets"
)

func TestEncryptorRoundTrip(t *testing.T) {
	encryptor, err := secrets.NewEncryptorFromKeyMaterial("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewEncryptorFromKeyMaterial: %v", err)
	}

	plaintext := []byte(`{"api_key":"sk-test"}`)
	blob, err := encryptor.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(blob, plaintext) {
		t.Fatal("encrypted blob matched plaintext")
	}

	got, err := encryptor.Decrypt(blob)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, plaintext)
	}
}

func TestEncryptorUsesRandomNonce(t *testing.T) {
	encryptor, err := secrets.NewEncryptorFromKeyMaterial("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewEncryptorFromKeyMaterial: %v", err)
	}

	first, err := encryptor.Encrypt([]byte("same"))
	if err != nil {
		t.Fatalf("Encrypt first: %v", err)
	}
	second, err := encryptor.Encrypt([]byte("same"))
	if err != nil {
		t.Fatalf("Encrypt second: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two encrypted blobs matched; nonce is not changing")
	}
}

func TestEncryptorRejectsWrongKey(t *testing.T) {
	encryptor, err := secrets.NewEncryptorFromKeyMaterial("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("NewEncryptorFromKeyMaterial: %v", err)
	}
	blob, err := encryptor.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	other, err := secrets.NewEncryptorFromKeyMaterial("abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("NewEncryptorFromKeyMaterial other: %v", err)
	}
	if _, err := other.Decrypt(blob); err == nil {
		t.Fatal("Decrypt succeeded with the wrong key")
	}
}

func TestEncryptorRequiresKeyMaterial(t *testing.T) {
	if _, err := secrets.NewEncryptorFromKeyMaterial(" "); err == nil {
		t.Fatal("expected empty key material to fail")
	}
}

// Weak passphrases must be rejected rather than silently stretched with a fast
// hash, so a leaked credential blob cannot be brute-forced offline.
func TestEncryptorRejectsWeakPassphrase(t *testing.T) {
	for _, material := range []string{"hunter2", "correct horse battery staple", "short"} {
		if _, err := secrets.NewEncryptorFromKeyMaterial(material); err == nil {
			t.Fatalf("expected weak passphrase %q to be rejected", material)
		}
	}
}
