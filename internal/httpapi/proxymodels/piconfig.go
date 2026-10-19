package proxymodels

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Pi's ~/.pi/agent/models.json schema (camelCase, providers keyed by name).
// Generated from the same provider definitions the /proxy/v1/models endpoint
// serves, so the extension-less onboarding path can never drift from it.

type piModelJSON struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Reasoning        bool               `json:"reasoning"`
	Input            []string           `json:"input"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap,omitempty"`
	Cost             map[string]float64 `json:"cost"`
	ContextWindow    int                `json:"contextWindow"`
	MaxTokens        int                `json:"maxTokens"`
	Compat           map[string]any     `json:"compat,omitempty"`
}

type piProviderJSON struct {
	BaseURL string         `json:"baseUrl"`
	APIKey  string         `json:"apiKey"`
	API     string         `json:"api"`
	Compat  map[string]any `json:"compat,omitempty"`
	Models  []piModelJSON  `json:"models"`
}

// PiModelsJSON renders the canonical provider catalog as a Pi models.json
// document with the user's key and proxy base URL baked in. Model names get
// the same "(corp)" badge the extension applies, so proxy models stay
// distinguishable in Pi's model selector either way.
func PiModelsJSON(baseURL, apiKey string) (string, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	resp := BuildResponse()

	providers := make(map[string]piProviderJSON, len(resp.Providers))
	for _, p := range resp.Providers {
		provider := piProviderJSON{
			BaseURL: baseURL + p.BaseURL,
			APIKey:  apiKey,
			API:     p.APIType,
			Models:  make([]piModelJSON, 0, len(p.Models)),
		}
		if p.Compat != nil {
			compat := map[string]any{}
			if p.Compat.SupportsDeveloperRole != nil {
				compat["supportsDeveloperRole"] = *p.Compat.SupportsDeveloperRole
			}
			if p.Compat.SupportsReasoningEffort != nil {
				compat["supportsReasoningEffort"] = *p.Compat.SupportsReasoningEffort
			}
			if p.Compat.MaxTokensField != "" {
				compat["maxTokensField"] = p.Compat.MaxTokensField
			}
			if len(compat) > 0 {
				provider.Compat = compat
			}
		}

		for _, m := range p.Models {
			model := piModelJSON{
				ID:               m.ID,
				Name:             m.Name + " (corp)",
				Reasoning:        m.Reasoning,
				Input:            m.Input,
				ThinkingLevelMap: m.ThinkingLevelMap,
				// Pi requires cost on every model (its cost tracking reads
				// model.cost.input unconditionally).
				Cost:          map[string]float64{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
				ContextWindow: m.ContextWindow,
				MaxTokens:     m.MaxTokens,
			}
			if m.Cost != nil {
				model.Cost = map[string]float64{
					"input":      m.Cost.Input,
					"output":     m.Cost.Output,
					"cacheRead":  m.Cost.CacheRead,
					"cacheWrite": m.Cost.CacheWrite,
				}
			}
			if m.Compat != nil {
				compat := map[string]any{}
				if m.Compat.ThinkingFormat != "" {
					compat["thinkingFormat"] = m.Compat.ThinkingFormat
				}
				if m.Compat.RequiresReasoningContentOnAssistantMessages != nil {
					compat["requiresReasoningContentOnAssistantMessages"] = *m.Compat.RequiresReasoningContentOnAssistantMessages
				}
				if len(compat) > 0 {
					model.Compat = compat
				}
			}
			provider.Models = append(provider.Models, model)
		}
		providers[p.Name] = provider
	}

	out, err := json.MarshalIndent(map[string]any{"providers": providers}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render pi models.json: %w", err)
	}
	return string(out), nil
}
