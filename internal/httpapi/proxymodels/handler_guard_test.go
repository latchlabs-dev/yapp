package proxymodels

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/earlgrey/piproxy/internal/modelguard"
)

// The default catalog advertises corp-deepseek-v4-pro; with an explicit guard it
// must disappear from /proxy/v1/models so the picker never offers a model that
// every send rejects.
func TestGuardedHandlerDropsGuardedModels(t *testing.T) {
	handler := NewGuardedTenantAwareHandler(nil, nil, modelguard.Rules{{Model: "deepseek-v4-pro", Message: "retired"}})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/v1/models", nil)
	handler.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp ProvidersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	sawPro, sawFlash := false, false
	for _, p := range resp.Providers {
		for _, m := range p.Models {
			switch m.ID {
			case "corp-deepseek-v4-pro":
				sawPro = true
			case "corp-deepseek-v4-flash":
				sawFlash = true
			}
		}
	}
	if sawPro {
		t.Error("guarded model corp-deepseek-v4-pro must not be advertised")
	}
	if !sawFlash {
		t.Error("unguarded corp-deepseek-v4-flash must remain advertised")
	}
}

func TestDropGuardedModelsHonorsSyntheticTarget(t *testing.T) {
	resp := &ProvidersResponse{Providers: []ProviderConfig{{
		Name: "corp",
		Models: []PiModelConfig{
			{ID: "cheap-alias", TargetID: "deepseek-v4-pro"},
			{ID: "corp-deepseek-v4-flash"},
		},
	}}}
	guards := modelguard.Rules{{Model: "deepseek-v4-pro", Message: "retired"}}

	got := DropGuardedModels(resp, guards, "")
	if len(got.Providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(got.Providers))
	}
	models := got.Providers[0].Models
	if len(models) != 1 || models[0].ID != "corp-deepseek-v4-flash" {
		t.Fatalf("alias routing to guarded target must be dropped, got %+v", models)
	}
}

func TestDropGuardedModelsHonorsOfficialFlashCompatibilityTargets(t *testing.T) {
	resp := BuildResponse()
	guards := modelguard.Rules{{Model: "deepseek-flash", Message: "temporarily unavailable"}}
	got := DropGuardedModels(resp, guards, "")
	for _, provider := range got.Providers {
		for _, model := range provider.Models {
			if model.TargetID == "deepseek-flash" {
				t.Fatalf("guarded official Flash alias still advertised: %s", model.ID)
			}
		}
	}
}
