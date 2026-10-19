package providers_test

import (
	"context"
	"testing"

	"github.com/earlgrey/piproxy/internal/providers"
)

type fakeProviderStore struct {
	providers []*providers.Provider
	aliases   []*providers.ModelAlias
}

func (s *fakeProviderStore) ListProviders(context.Context) ([]*providers.Provider, error) {
	return s.providers, nil
}

func (s *fakeProviderStore) ListAliases(context.Context) ([]*providers.ModelAlias, error) {
	return s.aliases, nil
}

func TestConfigLoaderMergesDefaultProvidersAndReplacesAliasSnapshots(t *testing.T) {
	ctx := context.Background()
	registry := providers.NewRegistry()
	loader := providers.NewConfigLoader(registry)
	store := &fakeProviderStore{
		providers: []*providers.Provider{{
			ID:              "prov_openai",
			Type:            providers.ProviderOpenAI,
			PublicBasePath:  "/openai",
			UpstreamBaseURL: "https://first.example",
			Enabled:         true,
		}},
		aliases: []*providers.ModelAlias{{
			ID:             "alias_first",
			PublicProvider: "openai",
			PublicModel:    "public-model",
			UpstreamModel:  "upstream-first",
			Enabled:        true,
		}},
	}
	loader.SetStore(store)

	if err := loader.LoadFromDB(ctx); err != nil {
		t.Fatalf("first LoadFromDB failed: %v", err)
	}
	if _, ok := registry.GetProvider(providers.ProviderOpenAI); !ok {
		t.Fatal("expected OpenAI provider after first load")
	}
	if _, ok := registry.GetProvider(providers.ProviderDeepSeek); !ok {
		t.Fatal("expected default DeepSeek provider to remain after DB provider load")
	}
	if _, ok := registry.GetAlias("", "openai", "public-model"); !ok {
		t.Fatal("expected alias after first load")
	}

	store.providers = []*providers.Provider{{
		ID:              "prov_anthropic",
		Type:            providers.ProviderAnthropic,
		PublicBasePath:  "/anthropic",
		UpstreamBaseURL: "https://second.example",
		Enabled:         true,
	}}
	store.aliases = nil

	if err := loader.LoadFromDB(ctx); err != nil {
		t.Fatalf("second LoadFromDB failed: %v", err)
	}
	if p, ok := registry.GetProvider(providers.ProviderOpenAI); !ok {
		t.Fatal("expected default OpenAI provider to be restored when DB OpenAI row disappears")
	} else if p.UpstreamBaseURL == "https://first.example" {
		t.Fatal("expected stale DB OpenAI provider to be removed")
	}
	if _, ok := registry.GetProvider(providers.ProviderAnthropic); !ok {
		t.Fatal("expected Anthropic provider after second load")
	}
	if _, ok := registry.GetProvider(providers.ProviderDeepSeek); !ok {
		t.Fatal("expected default DeepSeek provider to remain after second DB load")
	}
	if _, ok := registry.GetAlias("", "openai", "public-model"); ok {
		t.Fatal("expected stale alias to be removed")
	}
}

func TestConfigLoaderHydratesSparseKnownProviderModels(t *testing.T) {
	ctx := context.Background()
	registry := providers.NewRegistry()
	loader := providers.NewConfigLoader(registry)
	store := &fakeProviderStore{
		providers: []*providers.Provider{{
			ID:              "prov_xiaomi_sgp",
			Name:            "Xiaomi MiMo Token Plan (SGP)",
			Type:            providers.ProviderOpenAI,
			UpstreamBaseURL: "https://token-plan-sgp.xiaomimimo.com/v1",
			CredentialID:    "provider-credential:test",
			Enabled:         true,
			Billing: providers.BillingConfig{
				Mode:                 "subscription",
				Currency:             "USD",
				MonthlyCostCents:     600,
				RollingFiveHourQuota: 700,
				RollingSevenDayQuota: 2500,
			},
		}},
	}
	loader.SetStore(store)

	if err := loader.LoadFromDB(ctx); err != nil {
		t.Fatalf("LoadFromDB failed: %v", err)
	}
	route, ok := registry.ResolveModelRoute("", "openai", "/openai/v1/chat/completions", "corp-mimo-v2.5-pro")
	if !ok {
		t.Fatal("expected Xiaomi model route")
	}
	if route.Provider.ID != "prov_xiaomi_sgp" {
		t.Fatalf("provider id = %q, want prov_xiaomi_sgp", route.Provider.ID)
	}
	if route.Provider.PublicBasePath != "/openai" {
		t.Fatalf("public base path = %q, want /openai", route.Provider.PublicBasePath)
	}
	if route.Provider.PublicAPIBasePath != "/openai/v1" {
		t.Fatalf("public API base path = %q, want /openai/v1", route.Provider.PublicAPIBasePath)
	}
	if route.Provider.UpstreamBaseURL != "https://token-plan-sgp.xiaomimimo.com" {
		t.Fatalf("upstream base URL = %q, want normalized Xiaomi origin", route.Provider.UpstreamBaseURL)
	}
	if len(route.Provider.AllowedPaths) == 0 {
		t.Fatal("expected allowed paths from catalog defaults")
	}
	if !route.Provider.Streaming {
		t.Fatal("expected streaming from catalog defaults")
	}
	if route.UpstreamModel != "mimo-v2.5-pro" {
		t.Fatalf("upstream model = %q, want mimo-v2.5-pro", route.UpstreamModel)
	}
	if got := route.Provider.Billing; got.Mode != "subscription" ||
		got.MonthlyCostCents != 600 ||
		got.RollingFiveHourQuota != 700 ||
		got.RollingSevenDayQuota != 2500 {
		t.Fatalf("billing = %#v, want subscription account billing", got)
	}
}

func TestConfigLoaderAppliesAccountModelAllowlistAfterCatalogHydration(t *testing.T) {
	ctx := context.Background()
	registry := providers.NewRegistry()
	loader := providers.NewConfigLoader(registry)
	loader.SetStore(&fakeProviderStore{
		providers: []*providers.Provider{{
			ID:              "prov_qwen",
			Name:            "Qwen Token Plan",
			Type:            providers.ProviderOpenAI,
			UpstreamBaseURL: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode",
			CredentialID:    "provider-credential:test",
			Enabled:         true,
			ModelAllowlist:  []string{"glm-5.2", "deepseek-v4-pro"},
			Models: []providers.Model{
				{PublicID: "corp-glm-5.2", UpstreamID: "glm-5.2"},
				{PublicID: "corp-deepseek-v4-pro", UpstreamID: "deepseek-v4-pro"},
				{PublicID: "corp-qwen3.6-plus", UpstreamID: "qwen3.6-plus"},
			},
		}},
	})

	if err := loader.LoadFromDB(ctx); err != nil {
		t.Fatalf("LoadFromDB failed: %v", err)
	}
	var provider *providers.Provider
	for _, candidate := range registry.ListProviders() {
		if candidate.ID == "prov_qwen" {
			provider = candidate
			break
		}
	}
	if provider == nil {
		t.Fatal("expected Qwen provider account")
	}
	if len(provider.Models) != 2 {
		t.Fatalf("models = %#v, want exactly two allowlisted models", provider.Models)
	}
	if provider.Models[0].UpstreamID != "deepseek-v4-pro" && provider.Models[1].UpstreamID != "deepseek-v4-pro" {
		t.Fatalf("models = %#v, missing deepseek-v4-pro", provider.Models)
	}
	if provider.Models[0].UpstreamID != "glm-5.2" && provider.Models[1].UpstreamID != "glm-5.2" {
		t.Fatalf("models = %#v, missing glm-5.2", provider.Models)
	}
}
