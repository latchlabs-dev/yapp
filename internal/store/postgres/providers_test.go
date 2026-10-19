package postgres

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/providers"
)

func TestProviderTypeFromDB(t *testing.T) {
	tests := []struct {
		dbType string
		want   providers.ProviderType
	}{
		{"openai", providers.ProviderOpenAI},
		{"openai-completions", providers.ProviderOpenAI},
		{"openai-responses", providers.ProviderOpenAIResponses},
		{"anthropic", providers.ProviderAnthropic},
		{"anthropic-messages", providers.ProviderAnthropic},
		{"deepseek", providers.ProviderDeepSeek},
		{"deepseek-completions", providers.ProviderDeepSeek},
		{"mistral", providers.ProviderMistralConversations},
		{"mistral-conversations", providers.ProviderMistralConversations},
		{"google", providers.ProviderGoogleGenerativeAI},
		{"google-generative-ai", providers.ProviderGoogleGenerativeAI},
		{"google-vertex", providers.ProviderGoogleVertex},
		{"azure-openai-responses", providers.ProviderAzureOpenAIResponses},
		{"openai-codex", providers.ProviderOpenAICodexResponses},
		{"openai-codex-responses", providers.ProviderOpenAICodexResponses},
		{"amazon-bedrock", providers.ProviderBedrockConverse},
		{"bedrock", providers.ProviderBedrockConverse},
		{"bedrock-converse-stream", providers.ProviderBedrockConverse},
		// Unknown types pass through as-is.
		{"custom-type", providers.ProviderType("custom-type")},
		{"", providers.ProviderType("")},
	}

	for _, tt := range tests {
		t.Run(tt.dbType, func(t *testing.T) {
			got := providerTypeFromDB(tt.dbType)
			if got != tt.want {
				t.Errorf("providerTypeFromDB(%q) = %q, want %q", tt.dbType, got, tt.want)
			}
		})
	}
}

func TestProviderTypeFromDBAllBuiltinsCovered(t *testing.T) {
	// Every provider type declared in the providers package should be
	// mappable via providerTypeFromDB (the admin UI can send any Pi
	// provider key as the "provider" field and it must map to a known
	// ProviderType).
	allTypes := []providers.ProviderType{
		providers.ProviderOpenAI,
		providers.ProviderOpenAIResponses,
		providers.ProviderAnthropic,
		providers.ProviderDeepSeek,
		providers.ProviderMistralConversations,
		providers.ProviderGoogleGenerativeAI,
		providers.ProviderGoogleVertex,
		providers.ProviderAzureOpenAIResponses,
		providers.ProviderOpenAICodexResponses,
		providers.ProviderBedrockConverse,
	}

	// For each type, providerTypeFromDB(string(type)) should return type.
	for _, pt := range allTypes {
		s := string(pt)
		got := providerTypeFromDB(s)
		if got != pt {
			t.Errorf("providerTypeFromDB(%q) = %q, want %q (round-trip failed)", s, got, pt)
		}
	}
}

func TestDecodeProviderBilling(t *testing.T) {
	got := decodeProviderBilling(map[string]any{
		"mode":                    "subscription",
		"currency":                "USD",
		"monthly_cost_cents":      600,
		"rolling_five_hour_quota": 700,
		"rolling_seven_day_quota": 2500,
	})
	if got.Mode != "subscription" || got.MonthlyCostCents != 600 ||
		got.RollingFiveHourQuota != 700 || got.RollingSevenDayQuota != 2500 {
		t.Fatalf("unexpected billing config: %#v", got)
	}
}

func TestDecodeStringList(t *testing.T) {
	got := decodeStringList([]interface{}{"glm-5.2", 123, "qwen3.7-plus"})
	if len(got) != 2 || got[0] != "glm-5.2" || got[1] != "qwen3.7-plus" {
		t.Fatalf("decodeStringList = %#v", got)
	}
}

func TestBalanceAccountFromConfig(t *testing.T) {
	credentialID := "cred_1"
	got := balanceAccountFromConfig("provider_1", "openai-codex-responses", map[string]interface{}{
		"upstream_base_url": "https://chatgpt.com/backend-api",
		"billing": map[string]interface{}{
			"mode":     "subscription",
			"currency": "USD",
		},
		"balance": map[string]interface{}{
			"enabled": true,
			"url":     "https://chatgpt.com/backend-api/wham/usage",
		},
	}, &credentialID)
	if got.ID != "provider_1" || got.BillingMode != "subscription" || !got.BalanceEnabled {
		t.Fatalf("account = %#v", got)
	}
	if got.CredentialRef != "provider-credential:cred_1" {
		t.Fatalf("credential ref = %q", got.CredentialRef)
	}
}

func TestBalanceAccountFromConfigEnablesSubscriptionPollingByDefault(t *testing.T) {
	credentialID := "cred_2"
	got := balanceAccountFromConfig("provider_2", "openai-completions", map[string]interface{}{
		"upstream_base_url": "https://token-plan.example/compatible-mode",
		"billing": map[string]interface{}{
			"mode": "subscription",
		},
	}, &credentialID)
	if !got.BalanceEnabled {
		t.Fatal("subscription balance polling should be enabled by default")
	}
}

func TestBalanceAccountFromConfigEnablesZAIPollingByCatalogKey(t *testing.T) {
	credentialID := "cred_zai"
	got := balanceAccountFromConfig("provider_zai", "openai-completions", map[string]interface{}{
		"catalog_key":       "zai",
		"upstream_base_url": "https://api.z.ai/api/coding/paas/v4",
	}, &credentialID)
	if !got.BalanceEnabled || got.CatalogKey != "zai" {
		t.Fatalf("account = %#v", got)
	}
}

func TestBalanceAccountFromConfigEnablesZAIPollingByOfficialHost(t *testing.T) {
	got := balanceAccountFromConfig("provider_zai", "openai-completions", map[string]interface{}{
		"upstream_base_url": "https://open.bigmodel.cn/api/anthropic",
	}, nil)
	if !got.BalanceEnabled {
		t.Fatalf("account = %#v", got)
	}

	evil := balanceAccountFromConfig("provider_evil", "openai-completions", map[string]interface{}{
		"upstream_base_url": "https://api.z.ai.evil.example/api/coding/paas/v4",
	}, nil)
	if evil.BalanceEnabled {
		t.Fatalf("lookalike ZAI host was enabled: %#v", evil)
	}

	disabled := balanceAccountFromConfig("provider_disabled", "openai-completions", map[string]interface{}{
		"catalog_key":       "zai",
		"upstream_base_url": "https://api.z.ai/api/coding/paas/v4",
		"balance":           map[string]interface{}{"enabled": false},
	}, nil)
	if disabled.BalanceEnabled {
		t.Fatalf("explicitly disabled ZAI polling was enabled: %#v", disabled)
	}
}
