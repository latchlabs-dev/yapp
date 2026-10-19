// Package modelaccess evaluates tenant-specific model allow/deny settings.
package modelaccess

import (
	"context"
	"strings"
	"time"
)

const (
	ModeBlacklist   = "blacklist"
	ModeWhitelist   = "whitelist"
	CorpModelPrefix = "corp-"
)

// Store loads tenant model access configuration.
type Store interface {
	Get(ctx context.Context, tenantID string) (*Config, error)
}

// Config controls which models a tenant can see and invoke.
type Config struct {
	TenantID  string    `json:"tenant_id"`
	Mode      string    `json:"mode"`
	ModelIDs  []string  `json:"model_ids"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// DefaultConfig returns the no-op blacklist configuration.
func DefaultConfig(tenantID string) *Config {
	return &Config{TenantID: tenantID, Mode: ModeBlacklist, ModelIDs: []string{}}
}

// NormalizeMode returns a valid mode, defaulting to blacklist.
func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeWhitelist:
		return ModeWhitelist
	default:
		return ModeBlacklist
	}
}

// Allows reports whether model is permitted by the config. Model ids are
// matched in both public corp-prefixed and upstream unprefixed forms.
func (c *Config) Allows(model string) bool {
	if c == nil {
		return true
	}
	mode := NormalizeMode(c.Mode)
	model = strings.TrimSpace(model)
	models := make(map[string]bool, len(c.ModelIDs)*2)
	for _, id := range c.ModelIDs {
		for _, candidate := range equivalentModelIDs(id) {
			models[candidate] = true
		}
	}
	matched := false
	for _, candidate := range equivalentModelIDs(model) {
		if models[candidate] {
			matched = true
			break
		}
	}
	if mode == ModeWhitelist {
		return matched
	}
	return !matched
}

// AllowsSynthetic applies model policy to an advertised synthetic name and its
// concrete target. A blacklist cannot be bypassed by renaming a denied target;
// a whitelist may explicitly allow either the stable synthetic name or the
// underlying catalog model.
func (c *Config) AllowsSynthetic(publicModel, targetModel string) bool {
	if strings.TrimSpace(targetModel) == "" || publicModel == targetModel {
		return c.Allows(publicModel)
	}
	if NormalizeMode(c.Mode) == ModeWhitelist {
		return c.Allows(publicModel) || c.Allows(targetModel)
	}
	return c.Allows(publicModel) && c.Allows(targetModel)
}

func equivalentModelIDs(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return []string{""}
	}
	if strings.HasPrefix(model, CorpModelPrefix) {
		return []string{model, strings.TrimPrefix(model, CorpModelPrefix)}
	}
	return []string{model, CorpModelPrefix + model}
}
