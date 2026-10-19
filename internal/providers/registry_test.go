package providers

import (
	"testing"
)

func TestDefaultProvidersIncludesAllTypes(t *testing.T) {
	all := DefaultProviders()

	// Verify the core three are enabled by default.
	found := map[ProviderType]bool{}
	for _, p := range all {
		found[p.Type] = true
	}

	core := []ProviderType{
		ProviderOpenAI,
		ProviderAnthropic,
		ProviderDeepSeek,
	}
	for _, ct := range core {
		if !found[ct] {
			t.Errorf("core provider type %q is missing from DefaultProviders()", ct)
		}
	}

	// Verify all passthrough-gated protocols are present.
	passthrough := []ProviderType{
		ProviderMistralConversations,
		ProviderGoogleGenerativeAI,
		ProviderGoogleVertex,
		ProviderAzureOpenAIResponses,
		ProviderOpenAICodexResponses,
		ProviderBedrockConverse,
	}
	for _, pt := range passthrough {
		if !found[pt] {
			t.Errorf("passthrough provider type %q is missing from DefaultProviders()", pt)
		}
	}

	// Passthrough providers should start disabled.
	for _, p := range all {
		for _, pt := range passthrough {
			if p.Type == pt && p.Enabled {
				t.Errorf("passthrough provider %q should be disabled by default", pt)
			}
		}
	}
}

func TestDefaultProvidersEnabled(t *testing.T) {
	all := DefaultProviders()
	for _, p := range all {
		switch p.Type {
		case ProviderOpenAI, ProviderAnthropic, ProviderDeepSeek:
			if !p.Enabled {
				t.Errorf("core provider %q should be enabled by default", p.Type)
			}
		}
	}
}

func TestNewProviderTypeConstants(t *testing.T) {
	// Verify the new constants have the expected string values.
	tests := []struct {
		pt   ProviderType
		want string
	}{
		{ProviderOpenAI, "openai-completions"},
		{ProviderAnthropic, "anthropic-messages"},
		{ProviderDeepSeek, "deepseek-completions"},
		{ProviderOpenAIResponses, "openai-responses"},
		{ProviderMistralConversations, "mistral-conversations"},
		{ProviderGoogleGenerativeAI, "google-generative-ai"},
		{ProviderGoogleVertex, "google-vertex"},
		{ProviderAzureOpenAIResponses, "azure-openai-responses"},
		{ProviderOpenAICodexResponses, "openai-codex-responses"},
		{ProviderBedrockConverse, "bedrock-converse-stream"},
	}

	for _, tt := range tests {
		if string(tt.pt) != tt.want {
			t.Errorf("ProviderType %q = %q, want %q", tt.pt, string(tt.pt), tt.want)
		}
	}
}

func TestProviderTypeUniqueness(t *testing.T) {
	// All provider type constants must have unique string values.
	seen := map[string]ProviderType{}
	all := []ProviderType{
		ProviderOpenAI,
		ProviderOpenAIResponses,
		ProviderAnthropic,
		ProviderDeepSeek,
		ProviderMistralConversations,
		ProviderGoogleGenerativeAI,
		ProviderGoogleVertex,
		ProviderAzureOpenAIResponses,
		ProviderOpenAICodexResponses,
		ProviderBedrockConverse,
	}
	for _, pt := range all {
		s := string(pt)
		if prev, ok := seen[s]; ok {
			t.Errorf("duplicate provider type string %q for %q and %q", s, pt, prev)
		}
		seen[s] = pt
	}
}

func TestResolveProviderRoutesDeepSeekByProviderModel(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterProvider(&Provider{
		Type:           ProviderOpenAI,
		Name:           "OpenAI",
		PublicBasePath: "/openai",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("RegisterProvider OpenAI failed: %v", err)
	}
	if err := registry.RegisterProvider(&Provider{
		Type:           ProviderDeepSeek,
		Name:           "DeepSeek",
		PublicBasePath: "/deepseek",
		Models: []Model{{
			PublicID:   "corp-deepseek-v4-flash",
			UpstreamID: "deepseek-v4-flash",
		}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("RegisterProvider DeepSeek failed: %v", err)
	}

	route, ok := registry.ResolveModelRoute("", "deepseek", "/deepseek/v1/chat/completions", "corp-deepseek-v4-flash")
	if !ok {
		t.Fatal("expected DeepSeek model route")
	}
	if route.Provider.Type != ProviderDeepSeek {
		t.Fatalf("provider = %s, want %s", route.Provider.Type, ProviderDeepSeek)
	}
	if route.UpstreamModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash", route.UpstreamModel)
	}
}

func TestResolveModelRouteDoesNotCrossProviderBasePath(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterProvider(&Provider{
		Type:           ProviderDeepSeek,
		Name:           "Dedicated DeepSeek",
		PublicBasePath: "/deepseek",
		Models: []Model{{
			PublicID:   "corp-deepseek-v4-pro",
			UpstreamID: "deepseek-v4-pro",
		}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("RegisterProvider DeepSeek failed: %v", err)
	}
	if err := registry.RegisterProvider(&Provider{
		ID:             "qwen-plan",
		Type:           ProviderOpenAI,
		Name:           "Qwen Token Plan",
		PublicBasePath: "/openai",
		Models: []Model{{
			PublicID:   "corp-deepseek-v4-pro",
			UpstreamID: "deepseek-v4-pro",
		}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("RegisterProvider Qwen failed: %v", err)
	}

	route, ok := registry.ResolveModelRoute("", "openai", "/openai/v1/chat/completions", "corp-deepseek-v4-pro")
	if !ok {
		t.Fatal("expected model route")
	}
	if route.Provider.ID != "qwen-plan" {
		t.Fatalf("provider = %q, want qwen-plan", route.Provider.ID)
	}
}

func TestResolveModelRouteForProviderDisambiguatesDuplicateModels(t *testing.T) {
	registry := NewRegistry()
	for _, p := range []*Provider{
		{
			ID: "zai", Type: ProviderOpenAI, Name: "ZAI", PublicBasePath: "/openai",
			Models: []Model{{PublicID: "corp-glm-5.2", UpstreamID: "glm-5.2"}}, Enabled: true,
		},
		{
			ID: "qwen", Type: ProviderOpenAI, Name: "Qwen", PublicBasePath: "/openai",
			Models: []Model{{PublicID: "corp-glm-5.2", UpstreamID: "glm-5.2"}}, Enabled: true,
		},
	} {
		if err := registry.RegisterProvider(p); err != nil {
			t.Fatalf("RegisterProvider %s failed: %v", p.ID, err)
		}
	}

	route, ok := registry.ResolveModelRouteForProvider("qwen", "corp-glm-5.2")
	if !ok {
		t.Fatal("expected account-pinned route")
	}
	if route.Provider.ID != "qwen" || route.UpstreamModel != "glm-5.2" {
		t.Fatalf("route = provider %q model %q", route.Provider.ID, route.UpstreamModel)
	}
	if _, ok := registry.ResolveModelRouteForProvider("missing", "corp-glm-5.2"); ok {
		t.Fatal("unexpected route for unknown provider account")
	}
}

func TestApplyModelAllowlistUsesStableUpstreamIDs(t *testing.T) {
	p := &Provider{
		ModelAllowlist: []string{"glm-5.2", "qwen3.7-plus"},
		Models: []Model{
			{PublicID: "corp-glm-5.2", UpstreamID: "glm-5.2"},
			{PublicID: "corp-glm-5.1", UpstreamID: "glm-5.1"},
			{PublicID: "corp-qwen3.7-plus", UpstreamID: "qwen3.7-plus"},
		},
	}
	filtered := ApplyModelAllowlist(p)
	if len(filtered.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(filtered.Models))
	}
	if filtered.Models[0].UpstreamID != "glm-5.2" || filtered.Models[1].UpstreamID != "qwen3.7-plus" {
		t.Fatalf("unexpected filtered models: %+v", filtered.Models)
	}
	if len(p.Models) != 3 {
		t.Fatal("ApplyModelAllowlist mutated the input provider")
	}
}

func TestResolveModelRouteAliasScopedByTenantAndProvider(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterProvider(&Provider{
		ID:      "tenant-provider",
		Type:    ProviderOpenAI,
		Name:    "Tenant OpenAI",
		Enabled: true,
		Models:  []Model{{PublicID: "corp-tenant-upstream", UpstreamID: "tenant-upstream"}},
	}); err != nil {
		t.Fatalf("RegisterProvider tenant failed: %v", err)
	}
	if err := registry.RegisterProvider(&Provider{
		ID:      "global-provider",
		Type:    ProviderDeepSeek,
		Name:    "Global DeepSeek",
		Enabled: true,
		Models: []Model{
			{PublicID: "corp-global-upstream", UpstreamID: "global-upstream"},
			{PublicID: "corp-wrong-provider", UpstreamID: "wrong-provider"},
		},
	}); err != nil {
		t.Fatalf("RegisterProvider global failed: %v", err)
	}
	if err := registry.ReplaceAliases([]*ModelAlias{
		{
			ID:                "tenant-alias",
			TenantID:          "tenant-a",
			PublicProvider:    "openai",
			PublicModel:       "shared-model",
			UpstreamModel:     "tenant-upstream",
			ProviderAccountID: "tenant-provider",
			Enabled:           true,
		},
		{
			ID:                "global-alias",
			PublicProvider:    "openai",
			PublicModel:       "shared-model",
			UpstreamModel:     "global-upstream",
			ProviderAccountID: "global-provider",
			Enabled:           true,
		},
		{
			ID:                "other-provider-alias",
			TenantID:          "tenant-a",
			PublicProvider:    "anthropic",
			PublicModel:       "shared-model",
			UpstreamModel:     "wrong-provider",
			ProviderAccountID: "global-provider",
			Enabled:           true,
		},
	}); err != nil {
		t.Fatalf("ReplaceAliases failed: %v", err)
	}

	route, ok := registry.ResolveModelRoute("tenant-a", "openai", "/openai/v1/chat/completions", "shared-model")
	if !ok {
		t.Fatal("expected tenant alias route")
	}
	if route.Provider.ID != "tenant-provider" || route.UpstreamModel != "tenant-upstream" {
		t.Fatalf("tenant route = provider %q model %q", route.Provider.ID, route.UpstreamModel)
	}

	route, ok = registry.ResolveModelRoute("tenant-b", "openai", "/openai/v1/chat/completions", "shared-model")
	if !ok {
		t.Fatal("expected global alias fallback")
	}
	if route.Provider.ID != "global-provider" || route.UpstreamModel != "global-upstream" {
		t.Fatalf("global route = provider %q model %q", route.Provider.ID, route.UpstreamModel)
	}

	route, ok = registry.ResolveModelRoute("tenant-a", "anthropic", "/anthropic/v1/messages", "shared-model")
	if !ok {
		t.Fatal("expected provider-scoped anthropic alias")
	}
	if route.UpstreamModel != "wrong-provider" {
		t.Fatalf("provider-scoped route model = %q", route.UpstreamModel)
	}
}

func TestResolveModelRouteCatalogWinsOverSyntheticNameCollision(t *testing.T) {
	registry := NewRegistry()
	provider := &Provider{
		ID: "zai", Type: ProviderOpenAI, Name: "ZAI", Enabled: true,
		PublicBasePath: "/openai",
		Models:         []Model{{PublicID: "standard-flash", UpstreamID: "native-flash"}},
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAlias(&ModelAlias{
		ID: "synthetic", PublicProvider: "openai", PublicModel: "standard-flash",
		ProviderAccountID: "zai", UpstreamModel: "glm-5.3-flash", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	route, ok := registry.ResolveModelRoute("", "openai", "/openai/v1/chat/completions", "standard-flash")
	if !ok || route.UpstreamModel != "native-flash" {
		t.Fatalf("route = %#v, want native catalog model", route)
	}
}

func TestResolvePinnedSyntheticModelUsesTenantThenGlobal(t *testing.T) {
	registry := NewRegistry()
	provider := &Provider{
		ID: "zai", Type: ProviderOpenAI, Name: "ZAI", Enabled: true,
		Models: []Model{
			{PublicID: "corp-glm-5.3-flash", UpstreamID: "glm-5.3-flash"},
			{PublicID: "corp-glm-5.3", UpstreamID: "glm-5.3"},
		},
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceAliases([]*ModelAlias{
		{ID: "global", PublicProvider: "openai", PublicModel: "standard", ProviderAccountID: "zai", UpstreamModel: "glm-5.3-flash", Enabled: true},
		{ID: "tenant", TenantID: "tenant-a", PublicProvider: "openai", PublicModel: "standard", ProviderAccountID: "zai", UpstreamModel: "glm-5.3", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	tenantRoute, ok := registry.ResolveModelRouteForProviderAndTenant("zai", "tenant-a", "standard")
	if !ok || tenantRoute.UpstreamModel != "glm-5.3" {
		t.Fatalf("tenant route = %#v", tenantRoute)
	}
	globalRoute, ok := registry.ResolveModelRouteForProviderAndTenant("zai", "tenant-b", "standard")
	if !ok || globalRoute.UpstreamModel != "glm-5.3-flash" {
		t.Fatalf("global route = %#v", globalRoute)
	}
}

func TestListSyntheticModelsOmitsUnavailableTargets(t *testing.T) {
	registry := NewRegistry()
	provider := &Provider{
		ID: "zai", Type: ProviderOpenAI, Name: "ZAI", Enabled: true,
		CredentialID: "provider-credential:test",
		Models:       []Model{{PublicID: "corp-glm-5.3-flash", UpstreamID: "glm-5.3-flash", Name: "GLM-5.3-Flash"}},
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := registry.ReplaceAliases([]*ModelAlias{
		{ID: "valid", PublicProvider: "openai", PublicModel: "standard-flash", ProviderAccountID: "zai", UpstreamModel: "glm-5.3-flash", Enabled: true},
		{ID: "missing", PublicProvider: "openai", PublicModel: "missing", ProviderAccountID: "zai", UpstreamModel: "does-not-exist", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	models := registry.ListSyntheticModels("")
	if len(models) != 1 || models[0].Alias.PublicModel != "standard-flash" || models[0].TargetModel.UpstreamID != "glm-5.3-flash" {
		t.Fatalf("synthetic models = %#v", models)
	}
}
