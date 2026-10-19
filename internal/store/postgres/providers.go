// Package postgres provides database access for the proxy.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/credentialref"
	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/providers"
)

// ProviderStore loads provider accounts and model aliases from Postgres.
type ProviderStore struct {
	pool *Pool
}

// ProviderCatalogRecord describes a provider supported by a synchronized catalog.
type ProviderCatalogRecord struct {
	Namespace         string         `json:"namespace"`
	ProviderKey       string         `json:"provider_key"`
	DisplayName       string         `json:"display_name"`
	APIType           string         `json:"api_type"`
	BaseURL           string         `json:"base_url"`
	PublicBasePath    string         `json:"public_base_path"`
	PublicAPIBasePath string         `json:"public_api_base_path"`
	Metadata          map[string]any `json:"metadata"`
	SourceVersion     string         `json:"source_version"`
}

// NewProviderStore creates a new ProviderStore.
func NewProviderStore(pool *Pool) *ProviderStore {
	return &ProviderStore{pool: pool}
}

// ListProviderCatalogs returns synchronized provider presets in display order.
func (s *ProviderStore) ListProviderCatalogs(ctx context.Context, namespace string) ([]*ProviderCatalogRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT namespace, provider_key, display_name, api_type, base_url,
		       public_base_path, public_api_base_path, metadata, source_version
		FROM provider_catalogs
		WHERE namespace = $1
		ORDER BY lower(display_name), provider_key
	`, namespace)
	if err != nil {
		return nil, fmt.Errorf("query provider_catalogs: %w", err)
	}
	defer rows.Close()

	result := []*ProviderCatalogRecord{}
	for rows.Next() {
		record := &ProviderCatalogRecord{}
		if err := rows.Scan(
			&record.Namespace, &record.ProviderKey, &record.DisplayName,
			&record.APIType, &record.BaseURL, &record.PublicBasePath,
			&record.PublicAPIBasePath, &record.Metadata, &record.SourceVersion,
		); err != nil {
			return nil, fmt.Errorf("scan provider_catalog: %w", err)
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider_catalogs: %w", err)
	}
	return result, nil
}

// ListProviders returns all enabled provider accounts.
func (s *ProviderStore) ListProviders(ctx context.Context) ([]*providers.Provider, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pa.id, pa.provider, pa.name, pa.config, pc.id
		FROM provider_accounts pa
		LEFT JOIN LATERAL (
			SELECT id
			FROM provider_credentials
			WHERE provider_account_id = pa.id AND status = 'active'
			ORDER BY version DESC
			LIMIT 1
		) pc ON true
		WHERE pa.status = 'active'
	`)
	if err != nil {
		return nil, fmt.Errorf("query provider_accounts: %w", err)
	}
	defer rows.Close()

	var result []*providers.Provider
	for rows.Next() {
		var (
			id       string
			pType    string
			name     string
			config   map[string]interface{}
			activeID *string
		)
		if err := rows.Scan(&id, &pType, &name, &config, &activeID); err != nil {
			return nil, fmt.Errorf("scan provider_account: %w", err)
		}

		apiType := providerTypeFromDB(pType)
		p := &providers.Provider{
			ID:   id,
			Name: name,
			Type: apiType,
		}

		// Parse config fields from JSONB.
		if publicPath, ok := config["public_base_path"].(string); ok {
			p.PublicBasePath = publicPath
		}
		if publicName, ok := config["public_name"].(string); ok {
			p.PublicName = publicName
		}
		if publicAPIPath, ok := config["public_api_base_path"].(string); ok {
			p.PublicAPIBasePath = publicAPIPath
		}
		if upstreamURL, ok := config["upstream_base_url"].(string); ok {
			p.UpstreamBaseURL = upstreamURL
		}
		catalogKey, _ := config["catalog_key"].(string)
		if catalogKey == "" {
			catalogKey, _ = config["preset"].(string)
		}
		if credID, ok := config["credential_id"].(string); ok && credID != "" {
			p.CredentialID = credID
		} else if activeID != nil {
			p.CredentialID = credentialref.ProviderCredential(*activeID)
		}
		if enabled, ok := config["enabled"].(bool); ok {
			p.Enabled = enabled
		} else {
			p.Enabled = true
		}
		if paths, ok := config["allowed_paths"].([]interface{}); ok {
			for _, path := range paths {
				if s, ok := path.(string); ok {
					p.AllowedPaths = append(p.AllowedPaths, s)
				}
			}
		}
		if hosts, ok := config["allowed_hosts"].([]interface{}); ok {
			for _, host := range hosts {
				if s, ok := host.(string); ok {
					p.AllowedHosts = append(p.AllowedHosts, s)
				}
			}
		}
		if allowPrivate, ok := config["allow_private_egress"].(bool); ok {
			p.AllowPrivateEgress = allowPrivate
		}
		p.ModelAllowlist = decodeStringList(config["model_allowlist"])
		p.Billing = decodeProviderBilling(config["billing"])
		if catalogKey == "" {
			catalogKey = providers.CatalogKeyForProvider(p.Name, p.UpstreamBaseURL)
		}
		if catalogKey != "" {
			catalogNamespace, _ := config["catalog_namespace"].(string)
			if catalogNamespace == "" {
				catalogNamespace = "pi"
			}
			catalogProvider, ok, err := s.applyProviderCatalog(ctx, p, catalogNamespace, catalogKey)
			if err != nil {
				return nil, err
			}
			if ok {
				p = catalogProvider
			} else {
				p = providers.ApplyCatalogDefaults(p, catalogKey)
			}
		}
		p = providers.ApplyModelAllowlist(p)

		result = append(result, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider_accounts rows: %w", err)
	}

	// If nothing came from DB, fall back to defaults.
	if len(result) == 0 {
		return providers.DefaultProviders(), nil
	}
	return result, nil
}

func decodeStringList(value any) []string {
	var result []string
	switch values := value.(type) {
	case []string:
		result = append(result, values...)
	case []interface{}:
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
	}
	return result
}

func decodeProviderBilling(value any) providers.BillingConfig {
	raw, err := json.Marshal(value)
	if err != nil {
		return providers.BillingConfig{Mode: "default", Currency: "USD"}
	}
	var billing providers.BillingConfig
	if err := json.Unmarshal(raw, &billing); err != nil {
		return providers.BillingConfig{Mode: "default", Currency: "USD"}
	}
	if billing.Mode == "" {
		billing.Mode = "default"
	}
	if billing.Currency == "" {
		billing.Currency = "USD"
	}
	return billing
}

func (s *ProviderStore) applyProviderCatalog(ctx context.Context, p *providers.Provider, namespace, key string) (*providers.Provider, bool, error) {
	var (
		displayName       string
		apiType           string
		baseURL           string
		publicBasePath    string
		publicAPIBasePath string
		allowedPaths      []string
	)

	err := s.pool.QueryRow(ctx, `
		SELECT display_name, api_type, base_url, public_base_path, public_api_base_path, allowed_paths
		FROM provider_catalogs
		WHERE namespace = $1 AND provider_key = $2
	`, namespace, key).Scan(&displayName, &apiType, &baseURL, &publicBasePath, &publicAPIBasePath, &allowedPaths)
	if err != nil {
		if err == pgx.ErrNoRows {
			return p, false, nil
		}
		return nil, false, fmt.Errorf("query provider catalog %s/%s: %w", namespace, key, err)
	}

	merged := *p
	if merged.Name == "" {
		merged.Name = displayName
	}
	if merged.Type == "" {
		merged.Type = providerTypeFromDB(apiType)
	}
	if merged.PublicBasePath == "" {
		merged.PublicBasePath = publicBasePath
	}
	if merged.PublicAPIBasePath == "" {
		merged.PublicAPIBasePath = publicAPIBasePath
	}
	if merged.UpstreamBaseURL == "" {
		merged.UpstreamBaseURL = baseURL
	} else if shouldUseNormalizedCatalogBaseURL(merged.UpstreamBaseURL, baseURL, publicBasePath) {
		merged.UpstreamBaseURL = baseURL
	}
	if len(merged.AllowedPaths) == 0 {
		merged.AllowedPaths = append([]string(nil), allowedPaths...)
	}
	if merged.Timeout == 0 {
		merged.Timeout = 10 * time.Minute
	}
	if merged.MaxBodyBytes == 0 {
		merged.MaxBodyBytes = 20 * 1024 * 1024
	}
	if !merged.Streaming {
		merged.Streaming = true
	}

	models, err := s.loadProviderCatalogModels(ctx, namespace, key)
	if err != nil {
		return nil, false, err
	}
	if len(merged.Models) == 0 {
		merged.Models = models
	}
	return &merged, true, nil
}

func shouldUseNormalizedCatalogBaseURL(current, normalized, publicBasePath string) bool {
	current = strings.TrimRight(current, "/")
	normalized = strings.TrimRight(normalized, "/")
	if normalized == "" {
		return false
	}
	return publicBasePath == "/openai" && current == normalized+"/v1"
}

func (s *ProviderStore) loadProviderCatalogModels(ctx context.Context, namespace, key string) ([]providers.Model, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model_id, display_name, reasoning, input, thinking_level_map, compat,
		       cost, context_window, max_tokens
		FROM provider_catalog_models
		WHERE namespace = $1 AND provider_key = $2
		ORDER BY model_id
	`, namespace, key)
	if err != nil {
		return nil, fmt.Errorf("query provider catalog models %s/%s: %w", namespace, key, err)
	}
	defer rows.Close()

	var models []providers.Model
	for rows.Next() {
		var (
			modelID       string
			displayName   string
			reasoning     bool
			input         []string
			thinkingJSON  []byte
			compatJSON    []byte
			costJSON      []byte
			contextWindow int
			maxTokens     int
		)
		if err := rows.Scan(&modelID, &displayName, &reasoning, &input, &thinkingJSON, &compatJSON, &costJSON, &contextWindow, &maxTokens); err != nil {
			return nil, fmt.Errorf("scan provider catalog model: %w", err)
		}

		models = append(models, providers.Model{
			PublicID:         publicModelID(modelID),
			UpstreamID:       modelID,
			Name:             displayName,
			Reasoning:        reasoning,
			Input:            input,
			ThinkingLevelMap: decodeThinkingLevels(thinkingJSON),
			Cost:             decodeModelCost(costJSON),
			ContextWindow:    contextWindow,
			MaxTokens:        maxTokens,
			Compat:           decodeModelCompat(compatJSON),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider catalog models: %w", err)
	}
	return providers.NormalizeCatalogModels(models, key), nil
}

func publicModelID(upstreamID string) string {
	if strings.HasPrefix(upstreamID, providers.CorpModelPrefix) {
		return upstreamID
	}
	return providers.CorpModelPrefix + upstreamID
}

func decodeThinkingLevels(data []byte) map[string]*string {
	if len(data) == 0 {
		return nil
	}
	var levels map[string]*string
	if err := json.Unmarshal(data, &levels); err != nil {
		return nil
	}
	return levels
}

func decodeModelCost(data []byte) *providers.ModelCost {
	if len(data) == 0 {
		return &providers.ModelCost{}
	}
	var raw struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return &providers.ModelCost{}
	}
	return &providers.ModelCost{
		Input:      raw.Input,
		Output:     raw.Output,
		CacheRead:  raw.CacheRead,
		CacheWrite: raw.CacheWrite,
	}
}

func decodeModelCompat(data []byte) *providers.ModelCompat {
	if len(data) == 0 {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	compat := &providers.ModelCompat{}
	if value, ok := raw["thinking_format"].(string); ok {
		compat.ThinkingFormat = value
	}
	if value, ok := raw["thinkingFormat"].(string); ok && compat.ThinkingFormat == "" {
		compat.ThinkingFormat = value
	}
	if value, ok := raw["requires_reasoning_content_on_assistant_messages"].(bool); ok {
		compat.RequiresReasoningContentOnAssistantMessages = &value
	}
	if value, ok := raw["requiresReasoningContentOnAssistantMessages"].(bool); ok && compat.RequiresReasoningContentOnAssistantMessages == nil {
		compat.RequiresReasoningContentOnAssistantMessages = &value
	}
	if compat.ThinkingFormat == "" && compat.RequiresReasoningContentOnAssistantMessages == nil {
		return nil
	}
	return compat
}

// ListAliases returns all enabled model aliases.
func (s *ProviderStore) ListAliases(ctx context.Context) ([]*providers.ModelAlias, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, public_provider, public_model,
		       upstream_provider, upstream_model, provider_account_id, api_type
		FROM model_aliases
		WHERE status = 'active'
	`)
	if err != nil {
		return nil, fmt.Errorf("query model_aliases: %w", err)
	}
	defer rows.Close()

	var result []*providers.ModelAlias
	for rows.Next() {
		var (
			id                string
			tenantID          *string
			publicProvider    string
			publicModel       string
			upstreamProvider  string
			upstreamModel     string
			providerAccountID string
			apiType           string
		)
		if err := rows.Scan(&id, &tenantID, &publicProvider, &publicModel,
			&upstreamProvider, &upstreamModel, &providerAccountID, &apiType); err != nil {
			return nil, fmt.Errorf("scan model_alias: %w", err)
		}

		tenantStr := ""
		if tenantID != nil {
			tenantStr = *tenantID
		}

		result = append(result, &providers.ModelAlias{
			ID:                id,
			TenantID:          tenantStr,
			PublicProvider:    publicProvider,
			PublicModel:       publicModel,
			UpstreamProvider:  upstreamProvider,
			UpstreamModel:     upstreamModel,
			ProviderAccountID: providerAccountID,
			APIType:           apiType,
			Enabled:           true,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model_aliases rows: %w", err)
	}

	return result, nil
}

// ProviderAccountRecord represents a full provider account record for admin use.
type ProviderAccountRecord struct {
	ID        string                    `json:"id"`
	TenantID  string                    `json:"tenant_id,omitempty"`
	Provider  string                    `json:"provider"`
	Name      string                    `json:"name"`
	Status    string                    `json:"status"`
	Config    map[string]interface{}    `json:"config"`
	Balance   *providerbalance.Snapshot `json:"balance,omitempty"`
	CreatedAt time.Time                 `json:"created_at"`
	UpdatedAt time.Time                 `json:"updated_at"`
}

// ListProviderAccounts returns all provider accounts (admin view).
func (s *ProviderStore) ListProviderAccounts(ctx context.Context, tenantID string) ([]*ProviderAccountRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pa.id, pa.tenant_id, pa.provider, pa.name, pa.status, pa.config,
		       CASE WHEN pb.id IS NULL THEN NULL ELSE to_jsonb(pb) END,
		       pa.created_at, pa.updated_at
		FROM provider_accounts pa
		LEFT JOIN LATERAL (
			SELECT id, provider_account_id, billing_mode, status, balance_kind,
			       currency, unit, amount_limit, amount_used, amount_spent,
			       amount_remaining, usage_windows, normalization_version, normalization_error, source,
			       error_code, error_message, observed_at
			FROM provider_balance_snapshots
			WHERE provider_account_id = pa.id
			ORDER BY observed_at DESC, id DESC
			LIMIT 1
		) pb ON true
		WHERE pa.tenant_id IS NULL OR pa.tenant_id = $1
		ORDER BY pa.provider, pa.name
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list provider accounts: %w", err)
	}
	defer rows.Close()

	var result []*ProviderAccountRecord
	for rows.Next() {
		r := &ProviderAccountRecord{}
		var tenantIDPtr *string
		var balanceJSON []byte
		if err := rows.Scan(&r.ID, &tenantIDPtr, &r.Provider, &r.Name, &r.Status, &r.Config,
			&balanceJSON, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan provider account: %w", err)
		}
		if err := decodeProviderBalance(balanceJSON, r); err != nil {
			return nil, err
		}
		if tenantIDPtr != nil {
			r.TenantID = *tenantIDPtr
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetProviderAccount retrieves a provider account by ID.
func (s *ProviderStore) GetProviderAccount(ctx context.Context, id string) (*ProviderAccountRecord, error) {
	r := &ProviderAccountRecord{}
	var tenantIDPtr *string
	var balanceJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT pa.id, pa.tenant_id, pa.provider, pa.name, pa.status, pa.config,
		       CASE WHEN pb.id IS NULL THEN NULL ELSE to_jsonb(pb) END,
		       pa.created_at, pa.updated_at
		FROM provider_accounts pa
		LEFT JOIN LATERAL (
			SELECT id, provider_account_id, billing_mode, status, balance_kind,
			       currency, unit, amount_limit, amount_used, amount_spent,
			       amount_remaining, usage_windows, normalization_version, normalization_error, source,
			       error_code, error_message, observed_at
			FROM provider_balance_snapshots
			WHERE provider_account_id = pa.id
			ORDER BY observed_at DESC, id DESC
			LIMIT 1
		) pb ON true
		WHERE pa.id = $1
	`, id).Scan(&r.ID, &tenantIDPtr, &r.Provider, &r.Name, &r.Status, &r.Config,
		&balanceJSON, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get provider account %s: %w", id, err)
	}
	if err := decodeProviderBalance(balanceJSON, r); err != nil {
		return nil, err
	}
	if tenantIDPtr != nil {
		r.TenantID = *tenantIDPtr
	}
	return r, nil
}

func decodeProviderBalance(data []byte, record *ProviderAccountRecord) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var snapshot providerbalance.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode provider balance snapshot: %w", err)
	}
	record.Balance = &snapshot
	return nil
}

// InsertProviderAccount creates a new provider account.
func (s *ProviderStore) InsertProviderAccount(ctx context.Context, record *ProviderAccountRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO provider_accounts (id, tenant_id, provider, name, status, config, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, record.ID, nullableString(record.TenantID), record.Provider, record.Name, record.Status, record.Config, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert provider account: %w", err)
	}
	return nil
}

// UpdateProviderAccount modifies an existing provider account.
func (s *ProviderStore) UpdateProviderAccount(ctx context.Context, record *ProviderAccountRecord) error {
	record.UpdatedAt = time.Now()
	_, err := s.pool.Exec(ctx, `
		UPDATE provider_accounts SET provider=$3, name=$4, status=$5, config=$6, updated_at=$7
		WHERE id=$1 AND (tenant_id=$2 OR (tenant_id IS NULL AND $2 IS NULL))
	`, record.ID, nullableString(record.TenantID), record.Provider, record.Name, record.Status, record.Config, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update provider account: %w", err)
	}
	return nil
}

// DeleteProviderAccount removes a provider account by ID.
func (s *ProviderStore) DeleteProviderAccount(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM provider_accounts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete provider account: %w", err)
	}
	return nil
}

// providerTypeFromDB maps the DB provider field to a ProviderType.
func providerTypeFromDB(dbType string) providers.ProviderType {
	switch dbType {
	case "openai", "openai-completions":
		return providers.ProviderOpenAI
	case "openai-responses":
		return providers.ProviderOpenAIResponses
	case "anthropic", "anthropic-messages":
		return providers.ProviderAnthropic
	case "deepseek", "deepseek-completions":
		return providers.ProviderDeepSeek
	case "mistral", "mistral-conversations":
		return providers.ProviderMistralConversations
	case "google", "google-generative-ai":
		return providers.ProviderGoogleGenerativeAI
	case "google-vertex":
		return providers.ProviderGoogleVertex
	case "azure-openai-responses":
		return providers.ProviderAzureOpenAIResponses
	case "openai-codex", "openai-codex-responses":
		return providers.ProviderOpenAICodexResponses
	case "amazon-bedrock", "bedrock", "bedrock-converse-stream":
		return providers.ProviderBedrockConverse
	default:
		return providers.ProviderType(dbType)
	}
}
