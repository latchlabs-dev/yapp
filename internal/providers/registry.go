// Package providers manages LLM provider configuration and routing.
package providers

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProviderType categorizes the API family of a provider.
type ProviderType string

const (
	ProviderOpenAI          ProviderType = "openai-completions"
	ProviderOpenAIResponses ProviderType = "openai-responses"
	ProviderAnthropic       ProviderType = "anthropic-messages"
	ProviderDeepSeek        ProviderType = "deepseek-completions"

	// Additional Pi-supported API protocols — these are handled by the
	// generic passthrough handler (no protocol-specific parsing).
	ProviderMistralConversations ProviderType = "mistral-conversations"
	ProviderGoogleGenerativeAI   ProviderType = "google-generative-ai"
	ProviderGoogleVertex         ProviderType = "google-vertex"
	ProviderAzureOpenAIResponses ProviderType = "azure-openai-responses"
	ProviderOpenAICodexResponses ProviderType = "openai-codex-responses"
	ProviderBedrockConverse      ProviderType = "bedrock-converse-stream"
)

// Provider holds the runtime configuration for a single LLM provider.
type Provider struct {
	ID                 string        `json:"id"`
	Name               string        `json:"name"`
	Type               ProviderType  `json:"type"`
	PublicName         string        `json:"public_name,omitempty"`
	PublicBasePath     string        `json:"public_base_path"` // e.g., "/openai"
	PublicAPIBasePath  string        `json:"public_api_base_path,omitempty"`
	UpstreamBaseURL    string        `json:"upstream_base_url"` // e.g., "https://api.openai.com"
	AllowedPaths       []string      `json:"allowed_paths"`
	AllowedHosts       []string      `json:"allowed_hosts,omitempty"`
	AllowPrivateEgress bool          `json:"allow_private_egress,omitempty"`
	CredentialID       string        `json:"credential_id"`
	Timeout            time.Duration `json:"timeout"`
	Streaming          bool          `json:"streaming"`
	MaxBodyBytes       int64         `json:"max_body_bytes"`
	Enabled            bool          `json:"enabled"`
	Models             []Model       `json:"models,omitempty"`
	ModelAllowlist     []string      `json:"model_allowlist,omitempty"`
	Billing            BillingConfig `json:"billing"`
}

// RouteHeader pins a proxy request to the provider account selected by model
// discovery. Several upstream plans advertise the same model IDs, so model
// name alone is not sufficient to select the correct credential/account.
const RouteHeader = "X-Corp-Provider-ID"

// BillingConfig controls provider-account-specific cost accounting.
type BillingConfig struct {
	AllocationMethod          string  `json:"allocation_method,omitempty"`
	Mode                      string  `json:"mode"`
	Currency                  string  `json:"currency,omitempty"`
	MonthlyCostCents          float64 `json:"monthly_cost_cents,omitempty"`
	FallbackUSDPerMillion     float64 `json:"fallback_usd_per_million_tokens,omitempty"`
	InputPricePerMillion      float64 `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion     float64 `json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million,omitempty"`
	RollingFiveHourQuota      int64   `json:"rolling_five_hour_quota,omitempty"`
	RollingSevenDayQuota      int64   `json:"rolling_seven_day_quota,omitempty"`
}

// Model defines a model advertised by a provider account and the upstream
// model identifier to send to that account.
type Model struct {
	PublicID         string             `json:"public_id"`
	UpstreamID       string             `json:"upstream_id"`
	Name             string             `json:"name"`
	Reasoning        bool               `json:"reasoning"`
	Input            []string           `json:"input"`
	ThinkingLevelMap map[string]*string `json:"thinking_level_map,omitempty"`
	Cost             *ModelCost         `json:"cost,omitempty"`
	ContextWindow    int                `json:"context_window"`
	MaxTokens        int                `json:"max_tokens"`
	Compat           *ModelCompat       `json:"compat,omitempty"`
}

type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type ModelCompat struct {
	ThinkingFormat                              string `json:"thinking_format,omitempty"`
	RequiresReasoningContentOnAssistantMessages *bool  `json:"requires_reasoning_content_on_assistant_messages,omitempty"`
}

// ModelAlias defines a mapping from a public model name to an upstream model.
type ModelAlias struct {
	ID                string `json:"id"`
	TenantID          string `json:"tenant_id"`
	PublicProvider    string `json:"public_provider"`
	PublicModel       string `json:"public_model"`
	UpstreamProvider  string `json:"upstream_provider"`
	UpstreamModel     string `json:"upstream_model"`
	ProviderAccountID string `json:"provider_account_id"`
	APIType           string `json:"api_type"`
	Enabled           bool   `json:"enabled"`
}

// Registry holds all known providers and model aliases.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]*Provider   // keyed by ProviderType for legacy/type lookup
	accounts  map[string]*Provider   // keyed by provider account ID
	aliases   map[string]*ModelAlias // keyed by "tenant:provider:model"
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]*Provider),
		accounts:  make(map[string]*Provider),
		aliases:   make(map[string]*ModelAlias),
	}
}

// RegisterProvider adds a provider to the registry.
func (r *Registry) RegisterProvider(p *Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p.Type == "" {
		return fmt.Errorf("provider type is required")
	}
	if existing, ok := r.providers[string(p.Type)]; !ok || providerSortKey(p) < providerSortKey(existing) {
		r.providers[string(p.Type)] = p
	}
	if p.ID != "" {
		r.accounts[p.ID] = p
	}
	return nil
}

// ReplaceProviders atomically replaces the provider registry with the supplied
// provider snapshot.
func (r *Registry) ReplaceProviders(providers []*Provider) error {
	next := make(map[string]*Provider, len(providers))
	nextAccounts := make(map[string]*Provider, len(providers))
	for _, p := range providers {
		if p.Type == "" {
			return fmt.Errorf("provider type is required")
		}
		if existing, ok := next[string(p.Type)]; !ok || providerSortKey(p) < providerSortKey(existing) {
			next[string(p.Type)] = p
		}
		if p.ID != "" {
			nextAccounts[p.ID] = p
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = next
	r.accounts = nextAccounts
	return nil
}

// GetProvider retrieves a provider by type.
func (r *Registry) GetProvider(providerType ProviderType) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[string(providerType)]
	if !ok {
		return nil, false
	}
	return p, p.Enabled
}

// GetProviderByPath finds a provider whose public base path matches the given
// path. When several providers share a base path, the provider whose type name
// matches the base path segment wins, then lexicographic type order — map
// iteration must not decide routing.
func (r *Registry) GetProviderByPath(path string) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *Provider
	for _, p := range r.accountsAndProvidersLocked() {
		if !p.Enabled || !providerPathMatches(path, p.PublicBasePath) {
			continue
		}
		if best == nil || pathProviderLess(p, best) {
			best = p
		}
	}
	return best, best != nil
}

// pathProviderLess reports whether a should be preferred over b for path-based
// routing: native providers (type name prefixed by the base path segment)
// first, then lexicographic type order for determinism.
func pathProviderLess(a, b *Provider) bool {
	aNative := strings.HasPrefix(string(a.Type), strings.Trim(a.PublicBasePath, "/"))
	bNative := strings.HasPrefix(string(b.Type), strings.Trim(b.PublicBasePath, "/"))
	if aNative != bNative {
		return aNative
	}
	return a.Type < b.Type
}

// GetProviderByModel finds a provider that can handle a configured model name.
func (r *Registry) GetProviderByModel(model string) (*Provider, bool) {
	route, ok := r.ResolveModelRoute("", "", "", model)
	if !ok {
		return nil, false
	}
	return route.Provider, true
}

type ModelRoute struct {
	Provider      *Provider
	PublicModel   string
	UpstreamModel string
}

// SyntheticModel is an enabled model alias resolved against the concrete
// provider account and catalog model that supply its runtime behavior and Pi
// metadata.
type SyntheticModel struct {
	Alias       *ModelAlias
	Provider    *Provider
	TargetModel Model
}

// ResolveModelRoute resolves a model to a provider account and upstream model.
// Catalog models win on collisions; aliases are explicit, discoverable
// synthetic names and must not silently override a real model.
func (r *Registry) ResolveModelRoute(tenantID, publicProvider, path, model string) (*ModelRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, p := range r.accountsAndProvidersLocked() {
		if !p.Enabled {
			continue
		}
		// Never route a model request across provider-family paths. Without
		// this check, a Qwen-plan DeepSeek model submitted on /openai could be
		// captured by the dedicated /deepseek provider and then rejected by
		// its path guard.
		if path != "" && !providerPathMatches(path, p.PublicBasePath) {
			continue
		}
		if route, ok := modelRouteForProvider(p, model); ok {
			return route, true
		}
	}

	if publicProvider != "" {
		if route, ok := r.resolveAliasRouteLocked(tenantID, publicProvider, model); ok {
			return route, true
		}
	}

	return nil, false
}

// ResolveModelRouteForProvider resolves a model only within the provider
// account explicitly selected by the client. The account ID is emitted by
// /proxy/v1/models and returned by the corp-proxy extension in RouteHeader.
func (r *Registry) ResolveModelRouteForProvider(providerID, model string) (*ModelRoute, bool) {
	return r.ResolveModelRouteForProviderAndTenant(providerID, "", model)
}

// ResolveModelRouteForProviderAndTenant resolves a catalog or synthetic model
// within the provider account explicitly selected by model discovery. Tenant
// aliases take precedence over global aliases with the same public model ID.
func (r *Registry) ResolveModelRouteForProviderAndTenant(providerID, tenantID, model string) (*ModelRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.accounts[providerID]
	if !ok || !p.Enabled {
		return nil, false
	}
	if route, ok := modelRouteForProvider(p, model); ok {
		return route, true
	}
	return r.resolveAliasRouteForAccountLocked(providerID, tenantID, model)
}

func (r *Registry) resolveAliasRouteForAccountLocked(providerID, tenantID, model string) (*ModelRoute, bool) {
	scopes := []string{tenantID}
	if tenantID != "" {
		scopes = append(scopes, "")
	}
	for _, scope := range scopes {
		for _, alias := range r.aliases {
			if alias == nil || !alias.Enabled || alias.TenantID != scope ||
				alias.ProviderAccountID != providerID || alias.PublicModel != model {
				continue
			}
			p, ok := r.accounts[providerID]
			if !ok || !p.Enabled {
				return nil, false
			}
			target, ok := modelForProvider(p, alias.UpstreamModel)
			if !ok {
				return nil, false
			}
			upstreamModel := target.UpstreamID
			if upstreamModel == "" {
				upstreamModel = target.PublicID
			}
			return &ModelRoute{Provider: p, PublicModel: alias.PublicModel, UpstreamModel: upstreamModel}, true
		}
	}
	return nil, false
}

func modelRouteForProvider(p *Provider, model string) (*ModelRoute, bool) {
	if p == nil {
		return nil, false
	}
	for _, m := range p.Models {
		if m.PublicID != model && m.UpstreamID != model {
			continue
		}
		upstreamID := m.UpstreamID
		if upstreamID == "" {
			upstreamID = m.PublicID
		}
		return &ModelRoute{Provider: p, PublicModel: m.PublicID, UpstreamModel: upstreamID}, true
	}
	return nil, false
}

// ApplyModelAllowlist filters a provider account's catalog to the exact model
// IDs enabled for its current credential/subscription. Allowlists use upstream
// IDs so they remain stable even when public IDs are namespaced for clients.
func ApplyModelAllowlist(p *Provider) *Provider {
	if p == nil || len(p.ModelAllowlist) == 0 {
		return p
	}
	allowed := make(map[string]struct{}, len(p.ModelAllowlist))
	for _, model := range p.ModelAllowlist {
		model = strings.TrimSpace(model)
		if model != "" {
			allowed[model] = struct{}{}
		}
	}
	filtered := make([]Model, 0, len(p.Models))
	for _, model := range p.Models {
		if _, ok := allowed[model.UpstreamID]; ok {
			filtered = append(filtered, model)
			continue
		}
		if _, ok := allowed[model.PublicID]; ok {
			filtered = append(filtered, model)
		}
	}
	clone := *p
	clone.Models = filtered
	return &clone
}

// ResolveProvider resolves a provider for a request, first by configured model
// route, then by path for endpoints that are not model-routed.
func (r *Registry) ResolveProvider(path, model string) (*Provider, bool, bool) {
	if route, modelOK := r.ResolveModelRoute("", "", path, model); modelOK {
		return route.Provider, true, true
	}

	pathProvider, pathOK := r.GetProviderByPath(path)
	return pathProvider, false, pathOK
}

func (r *Registry) accountsAndProvidersLocked() []*Provider {
	result := make([]*Provider, 0, len(r.providers)+len(r.accounts))
	seen := make(map[*Provider]bool, len(r.providers)+len(r.accounts))
	for _, p := range r.accounts {
		if !seen[p] {
			result = append(result, p)
			seen[p] = true
		}
	}
	for _, p := range r.providers {
		if !seen[p] {
			result = append(result, p)
			seen[p] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return providerSortKey(result[i]) < providerSortKey(result[j])
	})
	return result
}

func (r *Registry) resolveAliasRouteLocked(tenantID, publicProvider, model string) (*ModelRoute, bool) {
	for _, key := range []string{
		aliasKey(tenantID, publicProvider, model),
		aliasKey("", publicProvider, model),
	} {
		alias, ok := r.aliases[key]
		if !ok || !alias.Enabled {
			continue
		}
		var p *Provider
		if alias.ProviderAccountID != "" {
			p, ok = r.accounts[alias.ProviderAccountID]
		}
		if !ok {
			p, ok = r.providers[alias.UpstreamProvider]
		}
		if ok && p.Enabled {
			target, targetOK := modelForProvider(p, alias.UpstreamModel)
			if !targetOK {
				continue
			}
			upstreamModel := target.UpstreamID
			if upstreamModel == "" {
				upstreamModel = target.PublicID
			}
			return &ModelRoute{Provider: p, PublicModel: alias.PublicModel, UpstreamModel: upstreamModel}, true
		}
	}
	return nil, false
}

func providerSortKey(p *Provider) string {
	if p == nil {
		return ""
	}
	if p.ID != "" {
		return "1:" + p.ID
	}
	return "0:" + string(p.Type) + ":" + p.Name
}

// RegisterAlias adds a model alias.
func (r *Registry) RegisterAlias(alias *ModelAlias) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := aliasKey(alias.TenantID, alias.PublicProvider, alias.PublicModel)
	r.aliases[key] = alias
	return nil
}

// ReplaceAliases atomically replaces the alias registry with the supplied
// alias snapshot.
func (r *Registry) ReplaceAliases(aliases []*ModelAlias) error {
	next := make(map[string]*ModelAlias, len(aliases))
	for _, alias := range aliases {
		if alias == nil {
			continue
		}
		key := aliasKey(alias.TenantID, alias.PublicProvider, alias.PublicModel)
		next[key] = alias
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases = next
	return nil
}

// GetAlias retrieves a model alias.
func (r *Registry) GetAlias(tenantID, provider, model string) (*ModelAlias, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	alias, ok := r.aliases[aliasKey(tenantID, provider, model)]
	if !ok {
		// Try without tenant.
		alias, ok = r.aliases[aliasKey("", provider, model)]
	}
	if !ok || !alias.Enabled {
		return nil, false
	}
	return alias, true
}

// ListProviders returns all registered providers.
func (r *Registry) ListProviders() []*Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.accountsAndProvidersLocked()
}

// ListSyntheticModels returns the effective discoverable aliases for a tenant.
// A tenant alias overrides a global alias with the same provider/model name.
// Aliases whose account or target model is unavailable are intentionally
// omitted so discovery cannot advertise a route that invocation cannot honor.
func (r *Registry) ListSyntheticModels(tenantID string) []SyntheticModel {
	r.mu.RLock()
	defer r.mu.RUnlock()

	effective := make(map[string]*ModelAlias)
	for _, alias := range r.aliases {
		if alias == nil || !alias.Enabled || alias.TenantID != "" {
			continue
		}
		effective[alias.PublicProvider+":"+alias.PublicModel] = alias
	}
	if tenantID != "" {
		for _, alias := range r.aliases {
			if alias == nil || !alias.Enabled || alias.TenantID != tenantID {
				continue
			}
			effective[alias.PublicProvider+":"+alias.PublicModel] = alias
		}
	}

	result := make([]SyntheticModel, 0, len(effective))
	for _, alias := range effective {
		p, ok := r.accounts[alias.ProviderAccountID]
		if !ok || !p.Enabled || p.CredentialID == "" {
			continue
		}
		target, ok := modelForProvider(p, alias.UpstreamModel)
		if !ok {
			continue
		}
		result = append(result, SyntheticModel{Alias: alias, Provider: p, TargetModel: target})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Alias.PublicModel == result[j].Alias.PublicModel {
			return result[i].Provider.ID < result[j].Provider.ID
		}
		return result[i].Alias.PublicModel < result[j].Alias.PublicModel
	})
	return result
}

func modelForProvider(p *Provider, model string) (Model, bool) {
	if p == nil {
		return Model{}, false
	}
	for _, candidate := range p.Models {
		if candidate.PublicID == model || candidate.UpstreamID == model {
			return candidate, true
		}
	}
	return Model{}, false
}

// RemoveProvider removes a provider from the registry.
func (r *Registry) RemoveProvider(providerType ProviderType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.providers[string(providerType)]; ok && p.ID != "" {
		delete(r.accounts, p.ID)
	}
	delete(r.providers, string(providerType))
}

func aliasKey(tenantID, provider, model string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, provider, model)
}

func providerPathMatches(requestPath, basePath string) bool {
	if basePath == "" {
		return false
	}
	if requestPath == basePath {
		return true
	}
	return len(requestPath) > len(basePath) &&
		requestPath[:len(basePath)] == basePath &&
		requestPath[len(basePath)] == '/'
}

// DefaultProviders returns the built-in providers that the proxy supports.
func DefaultProviders() []*Provider {
	return []*Provider{
		{
			Type:              ProviderOpenAI,
			Name:              "OpenAI",
			PublicBasePath:    "/openai",
			PublicAPIBasePath: "/openai/v1",
			UpstreamBaseURL:   "https://api.openai.com",
			AllowedPaths:      []string{"/v1/chat/completions", "/v1/models", "/v1/responses"},
			CredentialID:      "providers/openai/api-key",
			Streaming:         true,
			Timeout:           10 * time.Minute,
			MaxBodyBytes:      20 * 1024 * 1024,
			Enabled:           true,
		},
		{
			Type:              ProviderAnthropic,
			Name:              "Anthropic",
			PublicBasePath:    "/anthropic",
			PublicAPIBasePath: "/anthropic/v1",
			UpstreamBaseURL:   "https://api.anthropic.com",
			AllowedPaths:      []string{"/v1/messages", "/v1/models"},
			CredentialID:      "providers/anthropic/api-key",
			Streaming:         true,
			Timeout:           10 * time.Minute,
			MaxBodyBytes:      20 * 1024 * 1024,
			Enabled:           true,
		},
		{
			Type:              ProviderDeepSeek,
			Name:              "DeepSeek",
			PublicName:        "corp",
			PublicBasePath:    "/deepseek",
			PublicAPIBasePath: "/deepseek/v1",
			UpstreamBaseURL:   "https://api.deepseek.com",
			AllowedPaths:      []string{"/v1/chat/completions", "/v1/models"},
			CredentialID:      "providers/deepseek/api-key",
			Streaming:         true,
			Timeout:           10 * time.Minute,
			MaxBodyBytes:      20 * 1024 * 1024,
			Enabled:           true,
			Models:            DeepSeekModels(),
		},
		// Passthrough-gated providers — these need admin-configured
		// credentials and upstream URLs before they can route traffic.
		// They are registered as disabled by default so the registry
		// knows about the protocol but won't match routes until an
		// admin creates a provider account.
		{
			Type:            ProviderMistralConversations,
			Name:            "Mistral",
			PublicBasePath:  "/mistral",
			UpstreamBaseURL: "https://api.mistral.ai",
			AllowedPaths:    []string{"/v1/chat/completions", "/v1/models"},
			CredentialID:    "",
			Streaming:       true,
			Timeout:         10 * time.Minute,
			MaxBodyBytes:    20 * 1024 * 1024,
			Enabled:         false,
		},
		{
			Type:            ProviderGoogleGenerativeAI,
			Name:            "Google Gemini",
			PublicBasePath:  "/google",
			UpstreamBaseURL: "https://generativelanguage.googleapis.com",
			AllowedPaths:    []string{"/v1beta/"},
			CredentialID:    "",
			Streaming:       true,
			Timeout:         10 * time.Minute,
			MaxBodyBytes:    20 * 1024 * 1024,
			Enabled:         false,
		},
		{
			Type:            ProviderGoogleVertex,
			Name:            "Google Vertex AI",
			PublicBasePath:  "/vertex",
			UpstreamBaseURL: "https://{location}-aiplatform.googleapis.com",
			AllowedPaths:    []string{"/v1/"},
			CredentialID:    "",
			Streaming:       true,
			Timeout:         10 * time.Minute,
			MaxBodyBytes:    20 * 1024 * 1024,
			Enabled:         false,
		},
		{
			Type:           ProviderAzureOpenAIResponses,
			Name:           "Azure OpenAI Responses",
			PublicBasePath: "/azure-openai",
			AllowedPaths:   []string{"/"},
			CredentialID:   "",
			Streaming:      true,
			Timeout:        10 * time.Minute,
			MaxBodyBytes:   20 * 1024 * 1024,
			Enabled:        false,
		},
		{
			Type:            ProviderOpenAICodexResponses,
			Name:            "OpenAI Codex",
			PublicBasePath:  "/codex",
			UpstreamBaseURL: "https://chatgpt.com/backend-api",
			AllowedPaths:    []string{"/"},
			CredentialID:    "",
			Streaming:       true,
			Timeout:         10 * time.Minute,
			MaxBodyBytes:    20 * 1024 * 1024,
			Enabled:         false,
		},
		{
			Type:            ProviderBedrockConverse,
			Name:            "Amazon Bedrock",
			PublicBasePath:  "/bedrock",
			UpstreamBaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
			AllowedPaths:    []string{"/"},
			CredentialID:    "",
			Streaming:       true,
			Timeout:         10 * time.Minute,
			MaxBodyBytes:    20 * 1024 * 1024,
			Enabled:         false,
		},
	}
}
