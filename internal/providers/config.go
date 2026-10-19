package providers

import (
	"context"
	"fmt"
	"strings"
)

// Store is the interface for loading provider configuration from a persistent store.
// It allows the ConfigLoader to work with Postgres, mock stores, or other backends.
type Store interface {
	// ListProviders returns all enabled provider accounts.
	ListProviders(ctx context.Context) ([]*Provider, error)
	// ListAliases returns all enabled model aliases.
	ListAliases(ctx context.Context) ([]*ModelAlias, error)
}

// ConfigLoader loads provider configuration from Postgres.
type ConfigLoader struct {
	registry *Registry
	store    Store // optional; nil means default providers only
}

// NewConfigLoader creates a new ConfigLoader.
func NewConfigLoader(registry *Registry) *ConfigLoader {
	return &ConfigLoader{registry: registry}
}

// SetStore sets the persistent store the loader will query.
func (l *ConfigLoader) SetStore(s Store) {
	l.store = s
}

// LoadDefaultProviders registers the default built-in providers.
func (l *ConfigLoader) LoadDefaultProviders() error {
	for _, p := range DefaultProviders() {
		if err := l.registry.RegisterProvider(p); err != nil {
			return fmt.Errorf("register default provider %s: %w", p.Type, err)
		}
	}
	return nil
}

// LoadFromDB loads provider configurations from the database via the configured store.
// If no store is set, it falls back to default providers.
func (l *ConfigLoader) LoadFromDB(ctx context.Context) error {
	if l.store == nil {
		return l.LoadDefaultProviders()
	}

	dbProviders, err := l.store.ListProviders(ctx)
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}

	aliases, err := l.store.ListAliases(ctx)
	if err != nil {
		return fmt.Errorf("list aliases: %w", err)
	}

	mergedProviders := mergeDefaultProviders(dbProviders)
	if err := l.registry.ReplaceProviders(mergedProviders); err != nil {
		return fmt.Errorf("replace providers: %w", err)
	}

	if err := l.registry.ReplaceAliases(aliases); err != nil {
		return fmt.Errorf("replace aliases: %w", err)
	}

	return nil
}

func mergeDefaultProviders(dbProviders []*Provider) []*Provider {
	merged := make([]*Provider, 0, len(DefaultProviders())+len(dbProviders))
	merged = append(merged, DefaultProviders()...)
	for _, p := range dbProviders {
		if p != nil {
			merged = append(merged, mergeProviderWithDefault(p))
		}
	}
	return merged
}

func mergeProviderWithDefault(p *Provider) *Provider {
	if p == nil {
		return nil
	}
	merged := *p
	for _, def := range DefaultProviders() {
		if def.Type != p.Type {
			continue
		}
		catalogKey := CatalogKeyForProvider(p.Name, p.UpstreamBaseURL)
		if catalogKey != "" {
			merged = *ApplyCatalogDefaults(&Provider{}, catalogKey)
		} else {
			merged = *def
		}
		if p.ID != "" {
			merged.ID = p.ID
		}
		if p.Name != "" {
			merged.Name = p.Name
		}
		if p.PublicName != "" {
			merged.PublicName = p.PublicName
		}
		if p.PublicBasePath != "" {
			merged.PublicBasePath = p.PublicBasePath
		}
		if p.PublicAPIBasePath != "" {
			merged.PublicAPIBasePath = p.PublicAPIBasePath
		}
		if p.UpstreamBaseURL != "" {
			merged.UpstreamBaseURL = p.UpstreamBaseURL
		}
		if len(p.AllowedPaths) > 0 {
			merged.AllowedPaths = p.AllowedPaths
		}
		if len(p.AllowedHosts) > 0 {
			merged.AllowedHosts = p.AllowedHosts
		}
		if p.AllowPrivateEgress {
			merged.AllowPrivateEgress = true
		}
		if p.CredentialID != "" {
			merged.CredentialID = p.CredentialID
		}
		if p.Timeout > 0 {
			merged.Timeout = p.Timeout
		}
		if p.MaxBodyBytes > 0 {
			merged.MaxBodyBytes = p.MaxBodyBytes
		}
		if p.Streaming {
			merged.Streaming = true
		}
		if len(p.Models) > 0 {
			merged.Models = p.Models
		}
		merged.ModelAllowlist = append([]string(nil), p.ModelAllowlist...)
		// Billing belongs to the provider account, not the protocol default.
		// Preserve it when an OpenAI-compatible account is merged with the
		// built-in OpenAI transport settings.
		merged.Billing = p.Billing
		merged.Enabled = p.Enabled
		if catalogKey == "" {
			catalogKey = CatalogKeyForProvider(merged.Name, merged.UpstreamBaseURL)
		}
		if catalogKey != "" {
			return ApplyModelAllowlist(ApplyCatalogDefaults(&merged, catalogKey))
		}
		return ApplyModelAllowlist(&merged)
	}
	if catalogKey := CatalogKeyForProvider(p.Name, p.UpstreamBaseURL); catalogKey != "" {
		return ApplyModelAllowlist(ApplyCatalogDefaults(p, catalogKey))
	}
	return ApplyModelAllowlist(p)
}

// ProviderConfigFromPath extracts the provider type and API path from a request path.
// e.g., "/openai/v1/chat/completions" → (ProviderOpenAI, "/v1/chat/completions")
func ProviderConfigFromPath(path string) (ProviderType, string, error) {
	// Strip leading slash and split.
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid path: %s", path)
	}

	providerName := parts[0]
	apiPath := "/" + parts[1]

	switch providerName {
	case "openai":
		return ProviderOpenAI, apiPath, nil
	case "anthropic":
		return ProviderAnthropic, apiPath, nil
	default:
		return ProviderType(providerName), apiPath, nil
	}
}
