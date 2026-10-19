// Package auth provides authentication types and logic for proxy API keys.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Context holds resolved authentication information for a request.
type Context struct {
	TenantID   string    `json:"tenant_id"`
	UserID     string    `json:"user_id"`
	KeyID      string    `json:"key_id"`
	KeyPrefix  string    `json:"key_prefix"`
	Scopes     []string  `json:"scopes"`
	KeyExpires time.Time `json:"-"`
}

// HasScope checks if the auth context has a given scope.
func (a *Context) HasScope(scope string) bool {
	for _, s := range a.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

// HasAnyScope checks if the auth context has at least one of the given scopes.
func (a *Context) HasAnyScope(scopes ...string) bool {
	for _, required := range scopes {
		if a.HasScope(required) {
			return true
		}
	}
	return false
}

const (
	// KeyPrefix is the default prefix for proxy API keys.
	KeyPrefix = "cpk_live_"

	// SecretByteLen is the number of random bytes in the key secret.
	SecretByteLen = 32

	// PrefixVisibleLen is the number of characters of the prefix visible in logs.
	PrefixVisibleLen = 12

	// ScopeLLMInvoke allows calling LLM endpoints.
	ScopeLLMInvoke = "llm:invoke"

	// ScopeLLMInvokeOpenAI allows calling OpenAI endpoints.
	ScopeLLMInvokeOpenAI = "llm:invoke:openai"

	// ScopeLLMInvokeAnthropic allows calling Anthropic endpoints.
	ScopeLLMInvokeAnthropic = "llm:invoke:anthropic"
	// ScopeLLMInvokeDeepSeek allows calling DeepSeek endpoints.
	ScopeLLMInvokeDeepSeek = "llm:invoke:deepseek"

	// ScopeLLMModelsRead allows listing models.
	ScopeLLMModelsRead = "llm:models:read"

	// Lightweight session metadata is a baseline capability on every key.
	// Raw session logs and shared memory remain explicit capabilities.
	ScopeSessionSubmit     = "sessions:submit"
	ScopeSessionLogsWrite  = "sessions:logs:write"
	ScopeMemoryObserve     = "memory:observe"
	ScopeMemoryContextRead = "memory:context:read"

	// Admin scopes.
	ScopeAdminUsersWrite     = "admin:users:write"
	ScopeAdminKeysWrite      = "admin:keys:write"
	ScopeAdminQuotasWrite    = "admin:quotas:write"
	ScopeAdminProvidersWrite = "admin:providers:write"
	ScopeAdminUsageRead      = "admin:usage:read"
	ScopeAdminAuditRead      = "admin:audit:read"

	// ScopeAdminUsers is deprecated; use ScopeAdminUsersWrite.
	ScopeAdminUsers = "admin:users"
)

// BaselineKeyScopes applies capabilities that every proxy key receives. Raw
// transcript upload and shared-memory access deliberately remain opt-in.
func BaselineKeyScopes(scopes []string) []string {
	if len(scopes) == 0 {
		scopes = []string{ScopeLLMInvoke}
	}
	if !slices.Contains(scopes, ScopeSessionSubmit) {
		scopes = append(scopes, ScopeSessionSubmit)
	}
	return scopes
}

// GenerateKey creates a new API key and returns the full key,
// the prefix, and the hashed key for storage.
func GenerateKey(prefix string) (fullKey, keyPrefix, keyHash string, err error) {
	if prefix == "" {
		prefix = KeyPrefix
	}

	// Generate random bytes for the suffix.
	suffix := make([]byte, SecretByteLen)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", "", fmt.Errorf("generate key: %w", err)
	}

	// Encode as base64url (no padding).
	encoded := base64.RawURLEncoding.EncodeToString(suffix)

	fullKeyStr := prefix + encoded
	// The stored prefix MUST equal what PrefixFromKey extracts at lookup time,
	// otherwise GetByPrefix never matches and every key fails to authenticate.
	prefixPart := PrefixFromKey(fullKeyStr)

	// Hash the full key using HMAC-SHA256 with the configured pepper.
	hash := hashKey(fullKeyStr)

	return fullKeyStr, prefixPart, hash, nil
}

// pepper is a server-side secret mixed into key hashes to prevent
// rainbow-table attacks. Set via SetPepper before any VerifyKey calls.
var pepper string

// SetPepper sets the global pepper used for HMAC-SHA256 key hashing.
// Must be called before VerifyKey; safe to call once at startup.
func SetPepper(p string) {
	pepper = p
}

// hashKey creates an HMAC-SHA256 hash of the key using the configured pepper.
// If no pepper is configured, it falls back to raw SHA-256 (less secure).
func hashKey(key string) string {
	var h []byte
	if pepper != "" {
		mac := hmac.New(sha256.New, []byte(pepper))
		mac.Write([]byte(key))
		h = mac.Sum(nil)
	} else {
		sum := sha256.Sum256([]byte(key))
		h = sum[:]
	}
	return base64.RawURLEncoding.EncodeToString(h)
}

// VerifyKey checks if a key matches its stored hash using constant-time comparison.
func VerifyKey(key, storedHash string) bool {
	computed := hashKey(key)
	return hmac.Equal([]byte(computed), []byte(storedHash))
}

// ExtractKey attempts to extract an API key from the request headers.
// Returns the key or empty string if not found.
func ExtractKey(authHeader, proxyKeyHeader, apiKeyHeader string) string {
	// Check X-Corp-Proxy-Key first (preferred).
	if proxyKeyHeader != "" {
		return proxyKeyHeader
	}

	// Check Authorization: Bearer <key>.
	if strings.HasPrefix(authHeader, "Bearer ") {
		return authHeader[7:]
	}

	// Check x-api-key header.
	if apiKeyHeader != "" {
		return apiKeyHeader
	}

	return ""
}

// PrefixFromKey extracts the visible prefix from a full key.
// Format: "cpk_live_9vA2_m1nf6..." → "cpk_live_9vA2"
func PrefixFromKey(key string) string {
	if len(key) <= PrefixVisibleLen {
		return key
	}
	return key[:PrefixVisibleLen]
}
