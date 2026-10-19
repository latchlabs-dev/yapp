package providers_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/providers"
)

func TestDeepSeekFlashRoutesUseOfficialVisionModel(t *testing.T) {
	registry := providers.NewRegistry()
	loader := providers.NewConfigLoader(registry)
	qwen := &providers.Provider{
		ID: "qwen", Name: "Qwen Token Plan", Type: providers.ProviderOpenAI,
		PublicBasePath: "/openai", UpstreamBaseURL: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode",
		Enabled: true, CredentialID: "provider-credential:test",
		ModelAllowlist: []string{"deepseek-v4.1-flash"},
		Models: providers.NormalizeCatalogModels([]providers.Model{{
			PublicID: "corp-deepseek-v4.1-flash", UpstreamID: "deepseek-v4.1-flash",
			Input: []string{"text", "image"},
		}}, "qwen-token-plan"),
	}
	loader.SetStore(&fakeProviderStore{providers: []*providers.Provider{qwen}})
	if err := loader.LoadFromDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"corp-deepseek-flash", "corp-deepseek-v4.1-flash", "corp-deepseek-v4-flash", "corp-deepseek-v4-flash-vision-exp"} {
		t.Run(id, func(t *testing.T) {
			route, ok := registry.ResolveModelRoute("", "", "", id)
			if !ok || route.Provider.UpstreamBaseURL != "https://api.deepseek.com" || route.UpstreamModel != "deepseek-flash" {
				t.Fatalf("route = %#v, want official DeepSeek Flash", route)
			}
			var found bool
			for _, model := range route.Provider.Models {
				if model.PublicID != id {
					continue
				}
				found = true
				if !reflect.DeepEqual(model.Input, []string{"text", "image"}) || model.ContextWindow != 1048576 || model.MaxTokens != 393216 {
					t.Fatalf("incorrect vision metadata: %#v", model)
				}
				if model.Cost == nil || model.Cost.Input != 0.30 || model.Cost.Output != 1.20 || model.Cost.CacheRead != 0.006 {
					t.Fatalf("incorrect Flash pricing: %#v", model.Cost)
				}
				if strings.Contains(model.Name, "(Exp)") {
					t.Fatalf("retired experimental label: %s", model.Name)
				}
			}
			if !found {
				t.Fatal("model missing from routed provider")
			}
		})
	}
	if _, ok := registry.ResolveModelRouteForProvider("qwen", "corp-deepseek-v4.1-flash"); ok {
		t.Fatal("official public ID still routes to Qwen")
	}
	planRoute, ok := registry.ResolveModelRouteForProvider("qwen", "corp-qwen-deepseek-v4.1-flash")
	if !ok || planRoute.UpstreamModel != "deepseek-v4.1-flash" {
		t.Fatalf("Qwen's explicitly named copy is unavailable: %#v", planRoute)
	}
}

func TestNormalizeCatalogModelsPreservesSynchronizedMetadataAndIsIdempotent(t *testing.T) {
	input := []providers.Model{
		{PublicID: "corp-deepseek-flash", UpstreamID: "deepseek-flash", Input: []string{"text", "image"}, ContextWindow: 1000000, MaxTokens: 384000, Cost: &providers.ModelCost{Input: 0.30, Output: 1.20}},
		{PublicID: "corp-deepseek-v4-flash-vision-exp", UpstreamID: "deepseek-v4-flash-vision-exp", Name: "Experimental", Cost: &providers.ModelCost{Input: 0.14}},
	}
	got := providers.NormalizeCatalogModels(input, providers.CatalogDeepSeek)
	if !reflect.DeepEqual(got, providers.NormalizeCatalogModels(got, providers.CatalogDeepSeek)) {
		t.Fatal("normalization must not duplicate compatibility models")
	}
	if input[1].UpstreamID != "deepseek-v4-flash-vision-exp" || input[1].Name != "Experimental" {
		t.Fatal("normalization mutated the source snapshot")
	}
	for _, model := range got {
		if model.UpstreamID != "deepseek-flash" || model.ContextWindow != input[0].ContextWindow || model.Cost.Input != input[0].Cost.Input {
			t.Fatalf("alias did not inherit current synchronized metadata: %#v", model)
		}
	}
	if got := providers.NormalizeCatalogModels(input, "other-provider"); !reflect.DeepEqual(got, input) {
		t.Fatal("unrelated provider catalog changed")
	}
}
