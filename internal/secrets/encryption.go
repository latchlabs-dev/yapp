package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

var encryptedCredentialPrefix = []byte("piproxy:v1:")

// Encryptor encrypts provider credential blobs before storing them in Postgres.
type Encryptor struct {
	aead cipher.AEAD
}

// NewEncryptor creates an AES-256-GCM encryptor from a 32-byte key.
func NewEncryptor(key []byte) (*Encryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	return &Encryptor{aead: aead}, nil
}

// NewEncryptorFromKeyMaterial accepts a 32-byte key as base64, hex, or 32 raw bytes.
// Arbitrary passphrases are rejected; see normalizeEncryptionKey.
func NewEncryptorFromKeyMaterial(material string) (*Encryptor, error) {
	key, err := normalizeEncryptionKey(material)
	if err != nil {
		return nil, err
	}
	return NewEncryptor(key)
}

// Encrypt seals plaintext with AES-256-GCM and stores a version prefix with the blob.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("credential encryptor is not configured")
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := e.aead.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(encryptedCredentialPrefix)+len(nonce)+len(ciphertext))
	out = append(out, encryptedCredentialPrefix...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

// Decrypt opens a blob created by Encrypt.
func (e *Encryptor) Decrypt(blob []byte) ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("credential encryptor is not configured")
	}
	if !bytes.HasPrefix(blob, encryptedCredentialPrefix) {
		return nil, fmt.Errorf("unsupported credential blob format")
	}
	body := blob[len(encryptedCredentialPrefix):]
	nonceSize := e.aead.NonceSize()
	if len(body) < nonceSize {
		return nil, fmt.Errorf("credential blob is truncated")
	}
	nonce, ciphertext := body[:nonceSize], body[nonceSize:]
	plaintext, err := e.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential blob: %w", err)
	}
	return plaintext, nil
}

func normalizeEncryptionKey(material string) ([]byte, error) {
	material = strings.TrimSpace(material)
	if material == "" {
		return nil, fmt.Errorf("credential encryption key is required")
	}

	if decoded, err := base64.StdEncoding.DecodeString(material); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(material); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := hex.DecodeString(material); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len([]byte(material)) == 32 {
		return []byte(material), nil
	}

	// Do NOT derive a key from arbitrary material with a single fast hash: a
	// low-entropy passphrase would be brute-forceable offline if encrypted
	// credential blobs ever leak (e.g. a DB dump). Require a real 32-byte key
	// so weak input is rejected at startup rather than silently weakened.
	return nil, fmt.Errorf("PROXY_CREDENTIAL_ENCRYPTION_KEY must be a 32-byte key encoded as base64 or hex (or 32 raw bytes); provided material did not match any supported form")
}
