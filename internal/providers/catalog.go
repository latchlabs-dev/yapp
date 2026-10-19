package providers

import "time"

const (
	CatalogDeepSeek         = "deepseek"
	CatalogXiaomiTokenSGP   = "xiaomi-token-plan-sgp"
	CorpModelPrefix         = "corp-"
	defaultProviderTimeout  = 10 * time.Minute
	defaultProviderBodySize = 20 * 1024 * 1024
)

type CatalogEntry struct {
	Key                    string
	Name                   string
	PublicName             string
	Type                   ProviderType
	PublicBasePath         string
	PublicAPIBasePath      string
	UpstreamBaseURL        string
	DeprecatedUpstreamURLs []string
	AllowedPaths           []string
	Streaming              bool
	Timeout                time.Duration
	MaxBodyBytes           int64
	Models                 []Model
}

func CatalogByKey(key string) (*CatalogEntry, bool) {
	for _, entry := range KnownCatalog() {
		if entry.Key == key {
			return &entry, true
		}
	}
	return nil, false
}

func CatalogKeyForProvider(name, upstreamBaseURL string) string {
	for _, entry := range KnownCatalog() {
		if upstreamBaseURL == entry.UpstreamBaseURL || stringInSlice(upstreamBaseURL, entry.DeprecatedUpstreamURLs) {
			return entry.Key
		}
	}
	switch name {
	case "DeepSeek":
		return CatalogDeepSeek
	case "Xiaomi MiMo Token Plan (SGP)":
		return CatalogXiaomiTokenSGP
	default:
		return ""
	}
}

func ApplyCatalogDefaults(p *Provider, key string) *Provider {
	if p == nil || key == "" {
		return p
	}
	entry, ok := CatalogByKey(key)
	if !ok {
		return p
	}
	merged := *p
	if merged.Name == "" {
		merged.Name = entry.Name
	}
	if merged.PublicName == "" {
		merged.PublicName = entry.PublicName
	}
	merged.Type = entry.Type
	if merged.PublicBasePath == "" {
		merged.PublicBasePath = entry.PublicBasePath
	}
	if merged.PublicAPIBasePath == "" {
		merged.PublicAPIBasePath = entry.PublicAPIBasePath
	}
	if merged.UpstreamBaseURL == "" {
		merged.UpstreamBaseURL = entry.UpstreamBaseURL
	} else if stringInSlice(merged.UpstreamBaseURL, entry.DeprecatedUpstreamURLs) {
		merged.UpstreamBaseURL = entry.UpstreamBaseURL
	}
	if len(merged.AllowedPaths) == 0 {
		merged.AllowedPaths = append([]string(nil), entry.AllowedPaths...)
	}
	if merged.Timeout == 0 {
		merged.Timeout = entry.Timeout
	}
	if merged.MaxBodyBytes == 0 {
		merged.MaxBodyBytes = entry.MaxBodyBytes
	}
	if !merged.Streaming {
		merged.Streaming = entry.Streaming
	}
	if len(merged.Models) == 0 {
		merged.Models = append([]Model(nil), entry.Models...)
	}
	merged.Models = NormalizeCatalogModels(merged.Models, key)
	return &merged
}

func KnownCatalog() []CatalogEntry {
	return []CatalogEntry{
		{
			Key:               CatalogDeepSeek,
			Name:              "DeepSeek",
			PublicName:        "corp",
			Type:              ProviderDeepSeek,
			PublicBasePath:    "/deepseek",
			PublicAPIBasePath: "/deepseek/v1",
			UpstreamBaseURL:   "https://api.deepseek.com",
			AllowedPaths:      []string{"/v1/chat/completions", "/v1/models"},
			Streaming:         true,
			Timeout:           defaultProviderTimeout,
			MaxBodyBytes:      defaultProviderBodySize,
			Models:            DeepSeekModels(),
		},
		{
			Key:                    CatalogXiaomiTokenSGP,
			Name:                   "Xiaomi MiMo Token Plan (SGP)",
			Type:                   ProviderOpenAI,
			PublicBasePath:         "/openai",
			PublicAPIBasePath:      "/openai/v1",
			UpstreamBaseURL:        "https://token-plan-sgp.xiaomimimo.com",
			DeprecatedUpstreamURLs: []string{"https://token-plan-sgp.xiaomimimo.com/v1"},
			AllowedPaths:           []string{"/v1/chat/completions", "/v1/models"},
			Streaming:              true,
			Timeout:                defaultProviderTimeout,
			MaxBodyBytes:           defaultProviderBodySize,
			Models: []Model{
				{
					PublicID:      CorpModelPrefix + "mimo-v2.5-pro",
					UpstreamID:    "mimo-v2.5-pro",
					Name:          "Xiaomi MiMo V2.5 Pro",
					Reasoning:     true,
					Input:         []string{"text", "image"},
					Cost:          &ModelCost{Input: 0.28, Output: 1.1, CacheRead: 0, CacheWrite: 0},
					ContextWindow: 200000,
					MaxTokens:     64000,
				},
			},
		},
	}
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func DeepSeekModels() []Model {
	requiresReasoningContent := true
	low, high, max := "low", "high", "max"
	deepseekLevels := map[string]*string{
		"minimal": nil, "low": &low, "medium": nil, "high": &high, "xhigh": &max, "max": &max,
	}
	return NormalizeCatalogModels([]Model{
		{
			PublicID:         CorpModelPrefix + "deepseek-flash",
			UpstreamID:       "deepseek-flash",
			Name:             "DeepSeek V4.1 Flash",
			Reasoning:        true,
			Input:            []string{"text", "image"},
			ThinkingLevelMap: deepseekLevels,
			// Peak rates; off-peak provider billing is half these prices.
			Cost:          &ModelCost{Input: 0.30, Output: 1.20, CacheRead: 0.006, CacheWrite: 0},
			ContextWindow: 1048576,
			MaxTokens:     393216,
			Compat: &ModelCompat{
				ThinkingFormat: "deepseek",
				RequiresReasoningContentOnAssistantMessages: &requiresReasoningContent,
			},
		},
		{
			PublicID:         CorpModelPrefix + "deepseek-v4-pro",
			UpstreamID:       "deepseek-v4-pro",
			Name:             "DeepSeek V4 Pro",
			Reasoning:        true,
			Input:            []string{"text"},
			ThinkingLevelMap: map[string]*string{"minimal": nil, "low": nil, "medium": nil, "high": &high, "xhigh": &max, "max": &max},
			Cost:             &ModelCost{Input: 0.435, Output: 0.87, CacheRead: 0.003625, CacheWrite: 0},
			ContextWindow:    1000000,
			MaxTokens:        384000,
			Compat: &ModelCompat{
				ThinkingFormat: "deepseek",
				RequiresReasoningContentOnAssistantMessages: &requiresReasoningContent,
			},
		},
	}, CatalogDeepSeek)
}

// NormalizeCatalogModels keeps public DeepSeek names separate from token-plan
// copies and maps official Flash compatibility names to the current upstream.
// It applies to both the built-in catalog and synchronized database catalogs.
func NormalizeCatalogModels(models []Model, key string) []Model {
	result := append([]Model(nil), models...)
	if key == "qwen-token-plan" {
		for i := range result {
			if result[i].UpstreamID == "deepseek-v4.1-flash" {
				result[i].PublicID = CorpModelPrefix + "qwen-deepseek-v4.1-flash"
				result[i].Name = "DeepSeek V4.1 Flash (Qwen Token Plan)"
			}
		}
		return result
	}
	if key != CatalogDeepSeek {
		return result
	}
	var flash *Model
	for i := range result {
		if result[i].UpstreamID == "deepseek-flash" {
			flash = &result[i]
			break
		}
	}
	if flash == nil {
		return result
	}
	current := *flash
	for _, alias := range []string{"deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		model := current
		model.PublicID = CorpModelPrefix + alias
		model.Name = "DeepSeek V4.1 Flash"
		if alias != "deepseek-v4.1-flash" {
			model.Name += " (legacy " + alias + ")"
		}
		found := false
		for i := range result {
			if result[i].PublicID == model.PublicID {
				result[i] = model
				found = true
				break
			}
		}
		if !found {
			result = append(result, model)
		}
	}
	return result
}
