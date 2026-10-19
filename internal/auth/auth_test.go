package auth_test

import (
	"slices"
	"testing"

	"github.com/earlgrey/piproxy/internal/auth"
)

func TestGenerateKey(t *testing.T) {
	fullKey, prefix, hash, err := auth.GenerateKey("test_")
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}

	if fullKey == "" {
		t.Error("expected non-empty key")
	}
	if prefix == "" {
		t.Error("expected non-empty prefix")
	}
	if hash == "" {
		t.Error("expected non-empty hash")
	}

	if len(fullKey) < 30 {
		t.Errorf("expected key length >= 30, got %d", len(fullKey))
	}
}

func TestGenerateKeyDefaultPrefix(t *testing.T) {
	fullKey, _, _, err := auth.GenerateKey("")
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}

	if len(fullKey) < 30 {
		t.Errorf("expected key length >= 30, got %d", len(fullKey))
	}
}

func TestVerifyKey(t *testing.T) {
	fullKey, _, hash, err := auth.GenerateKey("test_")
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}

	if !auth.VerifyKey(fullKey, hash) {
		t.Error("VerifyKey should return true for valid key")
	}

	if auth.VerifyKey("wrong-key", hash) {
		t.Error("VerifyKey should return false for invalid key")
	}
}

func TestAuthContext(t *testing.T) {
	ctx := &auth.Context{
		TenantID:  "ten_123",
		UserID:    "usr_456",
		KeyID:     "key_789",
		KeyPrefix: "cpk_live_",
		Scopes:    []string{"llm:invoke", "admin:users"},
	}

	if !ctx.HasScope("llm:invoke") {
		t.Error("expected HasScope(llm:invoke) = true")
	}

	if ctx.HasScope("nonexistent") {
		t.Error("expected HasScope(nonexistent) = false")
	}

	if !ctx.HasAnyScope("llm:invoke", "admin:users") {
		t.Error("expected HasAnyScope to return true")
	}

	if ctx.HasAnyScope("nonexistent") {
		t.Error("expected HasAnyScope(nonexistent) to return false")
	}

	// Wildcard scope.
	ctx2 := &auth.Context{Scopes: []string{"*"}}
	if !ctx2.HasScope("anything") {
		t.Error("expected wildcard scope to match anything")
	}
}

func TestExtractKey(t *testing.T) {
	// X-Corp-Proxy-Key takes priority.
	result := auth.ExtractKey("Bearer token123", "cpk_live_key", "x-api-key-val")
	if result != "cpk_live_key" {
		t.Errorf("expected 'cpk_live_key', got %q", result)
	}

	// Authorization: Bearer
	result = auth.ExtractKey("Bearer mykey123", "", "")
	if result != "mykey123" {
		t.Errorf("expected 'mykey123', got %q", result)
	}

	// x-api-key fallback
	result = auth.ExtractKey("", "", "fallback-key")
	if result != "fallback-key" {
		t.Errorf("expected 'fallback-key', got %q", result)
	}

	// No key
	result = auth.ExtractKey("", "", "")
	if result != "" {
		t.Errorf("expected '', got %q", result)
	}
}

func TestPrefixFromKey(t *testing.T) {
	prefix := auth.PrefixFromKey("cpk_live_TEST_ONLY_SYNTHETIC_KEY_NOT_VALID")
	if len(prefix) != 12 {
		t.Errorf("expected prefix length 12, got %d: %q", len(prefix), prefix)
	}

	// Short key.
	prefix = auth.PrefixFromKey("short")
	if prefix != "short" {
		t.Errorf("expected 'short', got %q", prefix)
	}
}

func TestKeyUniqueness(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 10; i++ {
		fullKey, _, _, err := auth.GenerateKey("test_")
		if err != nil {
			t.Fatalf("GenerateKey failed: %v", err)
		}
		if keys[fullKey] {
			t.Error("duplicate key generated")
		}
		keys[fullKey] = true
	}
}

func TestScopeConstants(t *testing.T) {
	want := map[string]string{
		"llm invoke":          auth.ScopeLLMInvoke,
		"session submit":      auth.ScopeSessionSubmit,
		"session logs write":  auth.ScopeSessionLogsWrite,
		"memory observe":      auth.ScopeMemoryObserve,
		"memory context read": auth.ScopeMemoryContextRead,
		"admin keys write":    auth.ScopeAdminKeysWrite,
	}
	expected := map[string]string{
		"llm invoke":          "llm:invoke",
		"session submit":      "sessions:submit",
		"session logs write":  "sessions:logs:write",
		"memory observe":      "memory:observe",
		"memory context read": "memory:context:read",
		"admin keys write":    "admin:keys:write",
	}
	for name, got := range want {
		if got != expected[name] {
			t.Errorf("unexpected %s scope: %q", name, got)
		}
	}
}

func TestBaselineKeyScopesAlwaysIncludesSessionMetadata(t *testing.T) {
	for _, input := range [][]string{
		nil,
		{auth.ScopeLLMInvokeOpenAI},
		{auth.ScopeLLMInvoke, auth.ScopeSessionSubmit},
	} {
		got := auth.BaselineKeyScopes(input)
		if !slices.Contains(got, auth.ScopeSessionSubmit) {
			t.Fatalf("baseline scopes %v omitted %q", got, auth.ScopeSessionSubmit)
		}
		count := 0
		for _, scope := range got {
			if scope == auth.ScopeSessionSubmit {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("baseline scopes %v contain session scope %d times", got, count)
		}
	}
}
