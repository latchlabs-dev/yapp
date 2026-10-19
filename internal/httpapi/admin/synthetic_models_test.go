package admin

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/providers"
)

func TestResolveSyntheticTargetCanonicalizesCatalogModel(t *testing.T) {
	runtimeProviders := []*providers.Provider{{
		ID: "zai", Type: providers.ProviderOpenAI, PublicBasePath: "/openai",
		CredentialID: "provider-credential:test", Enabled: true,
		Models: []providers.Model{{PublicID: "corp-glm-5.3-flash", UpstreamID: "glm-5.3-flash"}},
	}}

	provider, model, ok := resolveSyntheticTarget(runtimeProviders, "zai", "corp-glm-5.3-flash")
	if !ok || provider.ID != "zai" || model.UpstreamID != "glm-5.3-flash" {
		t.Fatalf("target = provider %#v model %#v ok=%v", provider, model, ok)
	}
}

func TestSyntheticNameCollisionChecksPublicAndUpstreamIDs(t *testing.T) {
	runtimeProviders := []*providers.Provider{{
		ID: "zai", CredentialID: "provider-credential:test", Enabled: true,
		Models: []providers.Model{{PublicID: "corp-glm-5.3-flash", UpstreamID: "glm-5.3-flash"}},
	}}

	for _, name := range []string{"corp-glm-5.3-flash", "glm-5.3-flash"} {
		if !syntheticNameCollidesWithCatalog(runtimeProviders, name) {
			t.Fatalf("expected collision for %q", name)
		}
	}
	if syntheticNameCollidesWithCatalog(runtimeProviders, "standard-flash") {
		t.Fatal("unexpected collision for standard-flash")
	}
}

func TestValidSyntheticModelName(t *testing.T) {
	for _, name := range []string{"standard-flash", "team/model:v2", "model_1.0"} {
		if !validSyntheticModelName(name) {
			t.Fatalf("expected %q to be valid", name)
		}
	}
	for _, name := range []string{"", "has space", "bad?name"} {
		if validSyntheticModelName(name) {
			t.Fatalf("expected %q to be invalid", name)
		}
	}
}

func TestPublicProviderKey(t *testing.T) {
	if got := publicProviderKey("/openai/v1"); got != "openai" {
		t.Fatalf("publicProviderKey = %q", got)
	}
}
