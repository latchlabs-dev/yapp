package proxymodels

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPiModelsJSONDerivesFromCatalog(t *testing.T) {
	out, err := PiModelsJSON("https://proxy.example.com/", "test-key-value")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Providers map[string]struct {
			BaseURL string         `json:"baseUrl"`
			APIKey  string         `json:"apiKey"`
			API     string         `json:"api"`
			Compat  map[string]any `json:"compat"`
			Models  []struct {
				ID               string             `json:"id"`
				Name             string             `json:"name"`
				ThinkingLevelMap map[string]any     `json:"thinkingLevelMap"`
				Cost             map[string]float64 `json:"cost"`
				Compat           map[string]any     `json:"compat"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	catalog := BuildResponse()
	if len(doc.Providers) != len(catalog.Providers) {
		t.Errorf("providers = %d, want %d", len(doc.Providers), len(catalog.Providers))
	}

	for _, p := range catalog.Providers {
		got, ok := doc.Providers[p.Name]
		if !ok {
			t.Fatalf("provider %s missing from models.json", p.Name)
		}
		if got.APIKey != "test-key-value" {
			t.Errorf("apiKey = %q", got.APIKey)
		}
		if got.BaseURL != "https://proxy.example.com"+p.BaseURL {
			t.Errorf("baseUrl = %q", got.BaseURL)
		}
		if len(got.Models) != len(p.Models) {
			t.Fatalf("provider %s: models = %d, want %d", p.Name, len(got.Models), len(p.Models))
		}
		for i, m := range p.Models {
			if got.Models[i].ID != m.ID {
				t.Errorf("model id = %q, want %q", got.Models[i].ID, m.ID)
			}
			if !strings.HasSuffix(got.Models[i].Name, " (corp)") {
				t.Errorf("model name %q missing (corp) badge", got.Models[i].Name)
			}
			// Pi requires cost on every model; a missing table crashes its
			// cost tracking.
			if _, ok := got.Models[i].Cost["input"]; !ok {
				t.Errorf("model %s missing cost table", m.ID)
			}
			// thinkingLevelMap is top-level and must preserve null entries
			// (null = level unsupported).
			if len(m.ThinkingLevelMap) > 0 {
				levels := got.Models[i].ThinkingLevelMap
				if len(levels) != len(m.ThinkingLevelMap) {
					t.Errorf("model %s thinkingLevelMap = %v", m.ID, levels)
				}
				for k, v := range m.ThinkingLevelMap {
					if v == nil {
						if got, present := levels[k]; !present || got != nil {
							t.Errorf("model %s level %s should be explicit null", m.ID, k)
						}
					}
				}
			}
		}
	}
}
