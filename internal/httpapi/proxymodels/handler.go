package proxymodels

import (
	"encoding/json"
	"net/http"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
)

// NewHandler returns an HTTP handler that serves active provider and model
// configurations in Pi-extension format at /proxy/v1/models.
func NewHandler(registry ...*providers.Registry) http.HandlerFunc {
	return NewTenantAwareHandler(firstRegistry(registry...), nil)
}

// ResponseFilter narrows a built catalog further, after tenant model policy has
// been applied. It exists for clients that can invoke only part of what the
// tenant is entitled to — the web console speaks two protocols, the CLI speaks
// all of them — so such a client can serve a truthful catalog without a second
// copy of the build pipeline.
type ResponseFilter func(*ProvidersResponse) *ProvidersResponse

// KeepAPITypes returns a filter that keeps only providers whose api_type is one
// of the given values.
func KeepAPITypes(apiTypes ...string) ResponseFilter {
	allowed := make(map[string]struct{}, len(apiTypes))
	for _, apiType := range apiTypes {
		allowed[apiType] = struct{}{}
	}
	return func(resp *ProvidersResponse) *ProvidersResponse {
		if resp == nil {
			return resp
		}
		kept := &ProvidersResponse{Providers: make([]ProviderConfig, 0, len(resp.Providers))}
		for _, provider := range resp.Providers {
			if _, ok := allowed[provider.APIType]; ok {
				kept.Providers = append(kept.Providers, provider)
			}
		}
		return kept
	}
}

// NewTenantAwareHandler returns the model discovery handler with optional
// tenant model-policy filtering and optional client-capability filters.
func NewTenantAwareHandler(registry *providers.Registry, modelStore modelaccess.Store, filters ...ResponseFilter) http.HandlerFunc {
	return newTenantAwareHandler(registry, modelStore, nil, filters)
}

// NewGuardedTenantAwareHandler additionally drops models stopped by model
// guard rules from the advertised catalog. Advertising a model that every
// send rejects is a picker that lies: guards and catalog must agree.
func NewGuardedTenantAwareHandler(registry *providers.Registry, modelStore modelaccess.Store, guards modelguard.Rules, filters ...ResponseFilter) http.HandlerFunc {
	return newTenantAwareHandler(registry, modelStore, guards, filters)
}

func newTenantAwareHandler(registry *providers.Registry, modelStore modelaccess.Store, guards modelguard.Rules, filters []ResponseFilter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := ""
		if authCtx := middleware.GetAuthContext(r.Context()); authCtx != nil {
			tenantID = authCtx.TenantID
		}
		resp := BuildResponse()
		if registry != nil {
			resp = BuildResponseFromRegistry(registry, tenantID)
		}
		if modelStore != nil && tenantID != "" {
			if cfg, err := modelStore.Get(r.Context(), tenantID); err == nil {
				resp = FilterResponse(resp, cfg)
			} else {
				http.Error(w, "failed to load tenant model settings", http.StatusInternalServerError)
				return
			}
		}
		if len(guards) > 0 {
			resp = DropGuardedModels(resp, guards, tenantID)
		}
		for _, filter := range filters {
			resp = filter(resp)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func firstRegistry(registry ...*providers.Registry) *providers.Registry {
	if len(registry) == 0 {
		return nil
	}
	return registry[0]
}

// BuildResponse returns the canonical provider/model catalog. It is the
// single source of truth consumed by the /proxy/v1/models endpoint, the
// onboarding models.json generator, and (transitively) the extension.
// BuildResponse returns the default provider/model catalog.
func BuildResponse() *ProvidersResponse {
	return BuildResponseFromProviders(providers.DefaultProviders())
}

// BuildResponseFromRegistry builds the runtime catalog and adds effective
// global/tenant aliases as synthetic models on the provider account they
// target. Their capability, cost, and token metadata is inherited from that
// target model while their public ID remains the administrator-defined name.
func BuildResponseFromRegistry(registry *providers.Registry, tenantID string) *ProvidersResponse {
	if registry == nil {
		return BuildResponse()
	}
	resp := BuildResponseFromProviders(registry.ListProviders())
	providerIndex := make(map[string]int, len(resp.Providers))
	for i, provider := range resp.Providers {
		providerIndex[provider.RouteID] = i
	}

	for _, synthetic := range registry.ListSyntheticModels(tenantID) {
		i, ok := providerIndex[synthetic.Provider.ID]
		if !ok {
			continue
		}
		model := synthetic.TargetModel
		model.PublicID = synthetic.Alias.PublicModel
		model.Name = synthetic.Alias.PublicModel
		advertised := convertModel(model)
		advertised.TargetID = synthetic.TargetModel.UpstreamID
		appendModelIfMissing(&resp.Providers[i], advertised)
	}
	return resp
}

func BuildResponseFromProviders(runtimeProviders []*providers.Provider) *ProvidersResponse {
	falseVal := false
	trueVal := true

	resp := &ProvidersResponse{Providers: []ProviderConfig{}}
	for _, p := range runtimeProviders {
		if p == nil || !p.Enabled || p.CredentialID == "" || len(p.Models) == 0 {
			continue
		}
		out := ProviderConfig{
			Name:    publicProviderName(p),
			BaseURL: publicAPIBasePath(p),
			APIType: string(p.Type),
			RouteID: p.ID,
			Compat: &ProviderCompat{
				SupportsDeveloperRole:   &falseVal,
				SupportsReasoningEffort: &trueVal,
				MaxTokensField:          "max_tokens",
			},
			Models: make([]PiModelConfig, 0, len(p.Models)),
		}
		for _, model := range p.Models {
			out.Models = append(out.Models, convertModel(model))
		}
		resp.Providers = append(resp.Providers, out)
	}
	return resp
}

func FilterResponse(resp *ProvidersResponse, cfg *modelaccess.Config) *ProvidersResponse {
	if resp == nil || cfg == nil {
		return resp
	}
	filtered := &ProvidersResponse{Providers: make([]ProviderConfig, 0, len(resp.Providers))}
	for _, provider := range resp.Providers {
		next := provider
		next.Models = make([]PiModelConfig, 0, len(provider.Models))
		for _, model := range provider.Models {
			if cfg.AllowsSynthetic(model.ID, model.TargetID) {
				next.Models = append(next.Models, model)
			}
		}
		if len(next.Models) > 0 {
			filtered.Providers = append(filtered.Providers, next)
		}
	}
	return filtered
}

// DropGuardedModels removes models stopped by model guard rules from the
// advertised catalog, checking both the public id and a synthetic alias's
// upstream target so an alias routing to a guarded model cannot sneak it
// back into the picker. Tenant-scoped rules are honored via tenantID.
func DropGuardedModels(resp *ProvidersResponse, guards modelguard.Rules, tenantID string) *ProvidersResponse {
	if resp == nil || len(guards) == 0 {
		return resp
	}
	filtered := &ProvidersResponse{Providers: make([]ProviderConfig, 0, len(resp.Providers))}
	for _, provider := range resp.Providers {
		next := provider
		next.Models = make([]PiModelConfig, 0, len(provider.Models))
		for _, model := range provider.Models {
			if guards.Check(model.ID, tenantID) != nil {
				continue
			}
			if model.TargetID != "" && guards.Check(model.TargetID, tenantID) != nil {
				continue
			}
			next.Models = append(next.Models, model)
		}
		if len(next.Models) > 0 {
			filtered.Providers = append(filtered.Providers, next)
		}
	}
	return filtered
}

func convertModel(model providers.Model) PiModelConfig {
	out := PiModelConfig{
		ID:               model.PublicID,
		TargetID:         model.UpstreamID,
		Name:             model.Name,
		Reasoning:        model.Reasoning,
		Input:            model.Input,
		ThinkingLevelMap: model.ThinkingLevelMap,
		ContextWindow:    model.ContextWindow,
		MaxTokens:        model.MaxTokens,
	}
	if model.Cost != nil {
		out.Cost = &ModelCost{
			Input:      model.Cost.Input,
			Output:     model.Cost.Output,
			CacheRead:  model.Cost.CacheRead,
			CacheWrite: model.Cost.CacheWrite,
		}
	}
	if model.Compat != nil {
		out.Compat = &ModelCompat{
			ThinkingFormat: model.Compat.ThinkingFormat,
			RequiresReasoningContentOnAssistantMessages: model.Compat.RequiresReasoningContentOnAssistantMessages,
		}
	}
	return out
}

func appendModelIfMissing(provider *ProviderConfig, model PiModelConfig) {
	for i := range provider.Models {
		if provider.Models[i].ID == model.ID {
			return
		}
	}
	provider.Models = append(provider.Models, model)
}

func publicProviderName(p *providers.Provider) string {
	if p.PublicName != "" {
		return p.PublicName
	}
	return p.Name
}

func publicAPIBasePath(p *providers.Provider) string {
	if p.PublicAPIBasePath != "" {
		return p.PublicAPIBasePath
	}
	if p.PublicBasePath != "" {
		return p.PublicBasePath
	}
	return "/"
}
