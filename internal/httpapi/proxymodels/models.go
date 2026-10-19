// Package proxymodels provides the data model and handler for the proxy's
// model-discovery endpoint, which Pi extensions use to dynamically discover
// available providers and their model configurations.
package proxymodels

// PiModelConfig represents a model configuration for Pi extension registration.
// This maps directly to the Pi extension API's model definition format.
type PiModelConfig struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Reasoning bool     `json:"reasoning"`
	Input     []string `json:"input"`
	// ThinkingLevelMap maps Pi thinking levels to provider values. It is a
	// TOP-LEVEL Pi model field (not compat); a null value marks the level
	// unsupported, so entries must serialize as JSON null — hence *string.
	ThinkingLevelMap map[string]*string `json:"thinking_level_map,omitempty"`
	// Cost per Mtok in USD. REQUIRED by Pi: registering a model without cost
	// crashes Pi's cost tracking after each message.
	Cost          *ModelCost   `json:"cost,omitempty"`
	ContextWindow int          `json:"context_window"`
	MaxTokens     int          `json:"max_tokens"`
	Compat        *ModelCompat `json:"compat,omitempty"`
	TargetID      string       `json:"-"`
}

// ModelCost mirrors Pi's per-model cost table (USD per Mtok).
type ModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// ModelCompat holds Pi-specific compat settings for an individual model.
type ModelCompat struct {
	ThinkingFormat                              string `json:"thinking_format,omitempty"`
	RequiresReasoningContentOnAssistantMessages *bool  `json:"requires_reasoning_content_on_assistant_messages,omitempty"`
}

// ProviderCompat holds Pi-specific compat settings for a provider.
type ProviderCompat struct {
	SupportsDeveloperRole   *bool  `json:"supports_developer_role,omitempty"`
	SupportsReasoningEffort *bool  `json:"supports_reasoning_effort,omitempty"`
	MaxTokensField          string `json:"max_tokens_field,omitempty"`
}

// ProviderConfig represents a provider configuration for Pi extension registration.
type ProviderConfig struct {
	Name    string          `json:"name"`
	BaseURL string          `json:"base_url"`
	APIType string          `json:"api_type"`
	RouteID string          `json:"route_id,omitempty"`
	Compat  *ProviderCompat `json:"compat,omitempty"`
	Models  []PiModelConfig `json:"models"`
}

// ProvidersResponse is the top-level response from the /proxy/v1/models endpoint.
type ProvidersResponse struct {
	Providers []ProviderConfig `json:"providers"`
}
