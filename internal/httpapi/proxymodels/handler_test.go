package proxymodels

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/providers"
)

func TestNewHandlerReturnsProviders(t *testing.T) {
	handler := NewHandler()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/v1/models", nil)
	handler.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected application/json, got %s", contentType)
	}

	var resp ProvidersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(resp.Providers) == 0 {
		t.Fatal("expected at least one provider")
	}

	// Verify the corp provider exists with models (named "corp", not
	// "deepseek", so Pi cannot override its credentials — see BuildResponse).
	found := false
	for _, p := range resp.Providers {
		if p.Name == "corp" {
			found = true
			if p.APIType != string(providers.ProviderDeepSeek) {
				t.Errorf("corp api_type = %q, want %q", p.APIType, providers.ProviderDeepSeek)
			}
			if p.BaseURL != "/deepseek/v1" {
				t.Errorf("corp base_url = %q, want /deepseek/v1", p.BaseURL)
			}
			if len(p.Models) == 0 {
				t.Error("expected corp provider to have models")
			}
			for _, m := range p.Models {
				if !strings.HasPrefix(m.ID, "corp-") {
					t.Errorf("model id %q must carry corp- prefix to avoid client builtin collision", m.ID)
				}
				if m.ID == "" {
					t.Error("model id is empty")
				}
				if m.Name == "" {
					t.Error("model name is empty")
				}
				if m.ContextWindow <= 0 {
					t.Errorf("model %s has invalid context_window", m.ID)
				}
				if m.MaxTokens <= 0 {
					t.Errorf("model %s has invalid max_tokens", m.ID)
				}
			}
			break
		}
	}
	if !found {
		t.Error("expected corp provider in response")
	}

	// Verify JSON structure has snake_case keys.
	raw := make(map[string]interface{})
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	providers, ok := raw["providers"].([]interface{})
	if !ok {
		t.Fatal("providers should be an array")
	}
	if len(providers) == 0 {
		t.Fatal("providers array should not be empty")
	}
	firstProvider := providers[0].(map[string]interface{})
	if _, ok := firstProvider["base_url"]; !ok {
		t.Error("expected snake_case base_url key")
	}
	if _, ok := firstProvider["api_type"]; !ok {
		t.Error("expected snake_case api_type key")
	}
}

func TestNewHandlerIncludesActiveProviderModelsFromRegistry(t *testing.T) {
	registry := providers.NewRegistry()
	xiaomi := providers.ApplyCatalogDefaults(&providers.Provider{
		ID:              "prov_xiaomi_sgp",
		Name:            "Xiaomi MiMo Token Plan (SGP)",
		Type:            providers.ProviderOpenAI,
		UpstreamBaseURL: "https://token-plan-sgp.xiaomimimo.com/v1",
		CredentialID:    "provider-credential:test",
		Enabled:         true,
	}, providers.CatalogXiaomiTokenSGP)
	if err := registry.RegisterProvider(xiaomi); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := NewHandler(registry)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/v1/models", nil)
	handler.ServeHTTP(w, r)

	var resp ProvidersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	for _, p := range resp.Providers {
		if p.Name != "Xiaomi MiMo Token Plan (SGP)" {
			continue
		}
		if p.BaseURL != "/openai/v1" {
			t.Fatalf("Xiaomi base_url = %q, want /openai/v1", p.BaseURL)
		}
		for _, m := range p.Models {
			if m.ID == "corp-mimo-v2.5-pro" {
				return
			}
		}
		t.Fatal("Xiaomi provider was present without corp-mimo-v2.5-pro")
	}
	t.Fatal("expected Xiaomi provider in model response")
}

func TestFilterResponseAppliesTenantWhitelist(t *testing.T) {
	resp := BuildResponseFromProviders([]*providers.Provider{{
		ID:           "prov_test",
		Name:         "Test",
		Type:         providers.ProviderOpenAI,
		CredentialID: "provider-credential:test",
		Enabled:      true,
		Models: []providers.Model{
			{PublicID: "corp-a", UpstreamID: "a", Name: "A", Input: []string{"text"}, ContextWindow: 1, MaxTokens: 1},
			{PublicID: "corp-b", UpstreamID: "b", Name: "B", Input: []string{"text"}, ContextWindow: 1, MaxTokens: 1},
		},
	}})

	filtered := FilterResponse(resp, &modelaccess.Config{Mode: modelaccess.ModeWhitelist, ModelIDs: []string{"corp-b"}})
	if len(filtered.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(filtered.Providers))
	}
	if len(filtered.Providers[0].Models) != 1 || filtered.Providers[0].Models[0].ID != "corp-b" {
		t.Fatalf("models = %+v, want only corp-b", filtered.Providers[0].Models)
	}
}

func TestFilterResponseDropsProviderWhenBlacklistRemovesAllModels(t *testing.T) {
	resp := BuildResponseFromProviders([]*providers.Provider{{
		ID:           "prov_test",
		Name:         "Test",
		Type:         providers.ProviderOpenAI,
		CredentialID: "provider-credential:test",
		Enabled:      true,
		Models: []providers.Model{{
			PublicID: "corp-a", UpstreamID: "a", Name: "A", Input: []string{"text"}, ContextWindow: 1, MaxTokens: 1,
		}},
	}})

	filtered := FilterResponse(resp, &modelaccess.Config{Mode: modelaccess.ModeBlacklist, ModelIDs: []string{"a"}})
	if len(filtered.Providers) != 0 {
		t.Fatalf("providers = %d, want 0", len(filtered.Providers))
	}
}

func TestFilterResponseSyntheticTargetBlacklistCannotBeBypassed(t *testing.T) {
	resp := &ProvidersResponse{Providers: []ProviderConfig{{
		Name:   "ZAI",
		Models: []PiModelConfig{{ID: "standard-flash", TargetID: "glm-5.3-flash"}},
	}}}
	filtered := FilterResponse(resp, &modelaccess.Config{
		Mode: modelaccess.ModeBlacklist, ModelIDs: []string{"corp-glm-5.3-flash"},
	})
	if len(filtered.Providers) != 0 {
		t.Fatalf("filtered = %#v, want synthetic target removed", filtered)
	}
}

func TestNewHandlerIncludesZAICodingModelsFromRegistry(t *testing.T) {
	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		ID:                "prov_zai",
		Name:              "ZAI",
		Type:              providers.ProviderOpenAI,
		PublicBasePath:    "/openai/v1",
		PublicAPIBasePath: "/openai/v1",
		UpstreamBaseURL:   "https://api.z.ai/api/coding/paas/v4",
		AllowedPaths:      []string{"/chat/completions", "/models"},
		CredentialID:      "provider-credential:test",
		Enabled:           true,
		Models: []providers.Model{{
			PublicID:      "corp-glm-5.2",
			UpstreamID:    "glm-5.2",
			Name:          "GLM-5.2",
			Reasoning:     true,
			Input:         []string{"text"},
			ContextWindow: 1000000,
			MaxTokens:     131072,
			Cost:          &providers.ModelCost{},
		}},
	}); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := NewHandler(registry)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/v1/models", nil)
	handler.ServeHTTP(w, r)

	var resp ProvidersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	for _, p := range resp.Providers {
		if p.Name != "ZAI" {
			continue
		}
		if p.RouteID != "prov_zai" {
			t.Fatalf("ZAI route_id = %q, want prov_zai", p.RouteID)
		}
		if p.BaseURL != "/openai/v1" {
			t.Fatalf("ZAI base_url = %q, want /openai/v1", p.BaseURL)
		}
		for _, m := range p.Models {
			if m.ID == "corp-glm-5.2" {
				return
			}
		}
		t.Fatal("ZAI provider was present without corp-glm-5.2")
	}
	t.Fatal("expected ZAI provider in model response")
}

func TestBuildResponseFromRegistryAdvertisesSyntheticModelWithTargetMetadata(t *testing.T) {
	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		ID: "zai", Name: "ZAI", Type: providers.ProviderOpenAI,
		PublicBasePath: "/openai", PublicAPIBasePath: "/openai/v1",
		CredentialID: "provider-credential:test", Enabled: true,
		Models: []providers.Model{{
			PublicID: "corp-glm-5.3-flash", UpstreamID: "glm-5.3-flash", Name: "GLM-5.3-Flash",
			Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 1000000, MaxTokens: 131072,
			Cost:   &providers.ModelCost{Input: 0.075, Output: 0.25},
			Compat: &providers.ModelCompat{ThinkingFormat: "zai"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAlias(&providers.ModelAlias{
		ID: "synthetic", PublicProvider: "openai", PublicModel: "standard-flash",
		ProviderAccountID: "zai", UpstreamModel: "glm-5.3-flash", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	resp := BuildResponseFromRegistry(registry, "tenant-a")
	if len(resp.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(resp.Providers))
	}
	if resp.Providers[0].RouteID != "zai" {
		t.Fatalf("route_id = %q", resp.Providers[0].RouteID)
	}
	for _, model := range resp.Providers[0].Models {
		if model.ID != "standard-flash" {
			continue
		}
		if model.Name != "standard-flash" || model.ContextWindow != 1000000 || model.MaxTokens != 131072 ||
			model.Compat == nil || model.Compat.ThinkingFormat != "zai" {
			t.Fatalf("synthetic metadata = %#v", model)
		}
		return
	}
	t.Fatal("standard-flash was not advertised")
}

func TestBuildResponseFromRegistryDoesNotReplaceCatalogCollision(t *testing.T) {
	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		ID: "zai", Name: "ZAI", Type: providers.ProviderOpenAI,
		PublicBasePath: "/openai", CredentialID: "provider-credential:test", Enabled: true,
		Models: []providers.Model{
			{PublicID: "standard-flash", UpstreamID: "native", Name: "Native", Input: []string{"text"}, ContextWindow: 10, MaxTokens: 5},
			{PublicID: "corp-target", UpstreamID: "target", Name: "Target", Input: []string{"text"}, ContextWindow: 20, MaxTokens: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAlias(&providers.ModelAlias{
		ID: "synthetic", PublicProvider: "openai", PublicModel: "standard-flash",
		ProviderAccountID: "zai", UpstreamModel: "target", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	resp := BuildResponseFromRegistry(registry, "")
	for _, model := range resp.Providers[0].Models {
		if model.ID == "standard-flash" && model.Name != "Native" {
			t.Fatalf("catalog collision was replaced: %#v", model)
		}
	}
}

func TestBuildResponseUsesProviderPublicAPIBasePathWithoutGuessing(t *testing.T) {
	resp := BuildResponseFromProviders([]*providers.Provider{{
		Name:              "Custom",
		Type:              providers.ProviderOpenAI,
		PublicBasePath:    "/custom",
		PublicAPIBasePath: "/custom/openai/v1",
		CredentialID:      "provider-credential:test",
		Enabled:           true,
		Models: []providers.Model{{
			PublicID:      "corp-custom",
			UpstreamID:    "custom",
			Name:          "Custom",
			Input:         []string{"text"},
			ContextWindow: 1,
			MaxTokens:     1,
		}},
	}})
	if len(resp.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(resp.Providers))
	}
	if resp.Providers[0].BaseURL != "/custom/openai/v1" {
		t.Fatalf("base_url = %q, want explicit public API base", resp.Providers[0].BaseURL)
	}
}

func TestNewHandlerModelHasCompat(t *testing.T) {
	handler := NewHandler()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/v1/models", nil)
	handler.ServeHTTP(w, r)

	var resp ProvidersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	for _, p := range resp.Providers {
		if p.Name == "corp" {
			if p.Compat == nil {
				t.Fatal("expected deepseek compat to be set")
			}
			if p.Compat.MaxTokensField != "max_tokens" {
				t.Errorf("expected max_tokens_field='max_tokens', got %s", p.Compat.MaxTokensField)
			}
			for _, m := range p.Models {
				if m.Compat == nil || m.Compat.ThinkingFormat == "" {
					t.Errorf("model %s expected compat.thinking_format", m.ID)
				}
			}
		}
	}
}

// The web console speaks only two of the protocols the CLI does, so it filters
// the catalog it serves. The CLI's own catalog must keep every provider.
func TestKeepAPITypesNarrowsToInvokableProviders(t *testing.T) {
	resp := &ProvidersResponse{Providers: []ProviderConfig{
		{Name: "corp", APIType: string(providers.ProviderDeepSeek), Models: []PiModelConfig{{ID: "corp-a"}}},
		{Name: "codex", APIType: string(providers.ProviderOpenAICodexResponses), Models: []PiModelConfig{{ID: "corp-b"}}},
		{Name: "claude", APIType: string(providers.ProviderAnthropic), Models: []PiModelConfig{{ID: "corp-c"}}},
	}}

	kept := KeepAPITypes(
		string(providers.ProviderDeepSeek),
		string(providers.ProviderAnthropic),
	)(resp)

	if len(kept.Providers) != 2 {
		t.Fatalf("providers = %d, want the two invokable ones", len(kept.Providers))
	}
	for _, provider := range kept.Providers {
		if provider.APIType == string(providers.ProviderOpenAICodexResponses) {
			t.Errorf("a protocol the client cannot invoke survived the filter: %+v", provider)
		}
	}
	// The unfiltered catalog is unchanged: the CLI still sees everything.
	if len(resp.Providers) != 3 {
		t.Errorf("the source catalog was mutated: %d providers left", len(resp.Providers))
	}
}

func TestKeepAPITypesToleratesNil(t *testing.T) {
	if got := KeepAPITypes("openai-completions")(nil); got != nil {
		t.Errorf("KeepAPITypes(nil) = %+v, want nil", got)
	}
}
