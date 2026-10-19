package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/earlgrey/piproxy/internal/credentialref"
	"github.com/earlgrey/piproxy/internal/providerbalance"
)

// ProviderBalanceStore persists normalized upstream balance observations.
type ProviderBalanceStore struct {
	pool *Pool
}

// NewProviderBalanceStore creates a provider balance store.
func NewProviderBalanceStore(pool *Pool) *ProviderBalanceStore {
	return &ProviderBalanceStore{pool: pool}
}

// ListBalanceAccounts returns active accounts supported by the balance worker.
func (s *ProviderBalanceStore) ListBalanceAccounts(ctx context.Context) ([]providerbalance.Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pa.id, pa.provider, pa.config, pc.id
		FROM provider_accounts pa
		LEFT JOIN LATERAL (
			SELECT id
			FROM provider_credentials
			WHERE provider_account_id = pa.id AND status = 'active'
			ORDER BY version DESC
			LIMIT 1
		) pc ON true
		WHERE pa.status = 'active'
		ORDER BY pa.id
	`)
	if err != nil {
		return nil, fmt.Errorf("list provider balance accounts: %w", err)
	}
	defer rows.Close()

	result := []providerbalance.Account{}
	for rows.Next() {
		var id, provider string
		var config map[string]interface{}
		var credentialID *string
		if err := rows.Scan(&id, &provider, &config, &credentialID); err != nil {
			return nil, fmt.Errorf("scan provider balance account: %w", err)
		}
		account := balanceAccountFromConfig(id, provider, config, credentialID)
		if account.BalanceEnabled {
			result = append(result, account)
		}
	}
	return result, rows.Err()
}

// GetBalanceAccount returns one account for an on-demand refresh.
func (s *ProviderBalanceStore) GetBalanceAccount(ctx context.Context, accountID string) (*providerbalance.Account, error) {
	var id, provider string
	var config map[string]interface{}
	var credentialID *string
	err := s.pool.QueryRow(ctx, `
		SELECT pa.id, pa.provider, pa.config, pc.id
		FROM provider_accounts pa
		LEFT JOIN LATERAL (
			SELECT id
			FROM provider_credentials
			WHERE provider_account_id = pa.id AND status = 'active'
			ORDER BY version DESC
			LIMIT 1
		) pc ON true
		WHERE pa.id = $1 AND pa.status = 'active'
	`, accountID).Scan(&id, &provider, &config, &credentialID)
	if err != nil {
		return nil, fmt.Errorf("get provider balance account %s: %w", accountID, err)
	}
	account := balanceAccountFromConfig(id, provider, config, credentialID)
	return &account, nil
}

// InsertBalanceSnapshot appends one immutable observation.
func (s *ProviderBalanceStore) InsertBalanceSnapshot(ctx context.Context, snapshot *providerbalance.Snapshot) error {
	if snapshot.NormalizationVersion <= 0 {
		snapshot.NormalizationVersion = 1
	}
	windows, err := json.Marshal(snapshot.UsageWindows)
	if err != nil {
		return fmt.Errorf("marshal provider usage windows: %w", err)
	}
	rawHeaders, err := json.Marshal(snapshot.RawHeaders)
	if err != nil {
		return fmt.Errorf("marshal provider usage headers: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO provider_balance_snapshots (
			provider_account_id, billing_mode, status, balance_kind, currency, unit,
			amount_limit, amount_used, amount_spent, amount_remaining, usage_windows,
			normalization_version, normalization_error, raw_payload, raw_headers,
			source, error_code, error_message, observed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		RETURNING id
	`, snapshot.ProviderAccountID, snapshot.BillingMode, snapshot.Status,
		nullableString(snapshot.BalanceKind), nullableString(snapshot.Currency), nullableString(snapshot.Unit),
		snapshot.AmountLimit, snapshot.AmountUsed, snapshot.AmountSpent, snapshot.AmountRemaining,
		windows, snapshot.NormalizationVersion, nullableString(snapshot.NormalizationError),
		nullableRawJSON(snapshot.RawPayload), rawHeaders, nullableString(snapshot.Source),
		nullableString(snapshot.ErrorCode), nullableString(snapshot.ErrorMessage), snapshot.ObservedAt).Scan(&snapshot.ID)
	if err != nil {
		return fmt.Errorf("insert provider balance snapshot: %w", err)
	}
	return nil
}

// ListBalanceHistory returns recent observations in chronological order for
// charting. Both successes and failures are retained so callers can distinguish
// missing data from a genuinely flat balance.
func (s *ProviderBalanceStore) ListBalanceHistory(ctx context.Context, accountID string, query providerbalance.HistoryQuery) ([]providerbalance.Snapshot, error) {
	if query.Limit < 1 || query.Limit > 10000 {
		return nil, fmt.Errorf("invalid history limit")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, provider_account_id, billing_mode, status,
		       COALESCE(balance_kind, ''), COALESCE(currency, ''), COALESCE(unit, ''),
		       amount_limit::double precision, amount_used::double precision,
		       amount_spent::double precision, amount_remaining::double precision, usage_windows,
		       normalization_version, COALESCE(normalization_error, ''),
		       COALESCE(source, ''), COALESCE(error_code, ''), COALESCE(error_message, ''), observed_at
		FROM (
			(SELECT *
			FROM provider_balance_snapshots
			WHERE provider_account_id = $1
			  AND ($3::timestamptz IS NULL OR observed_at >= $3)
			  AND ($4::timestamptz IS NULL OR observed_at < $4)
			ORDER BY observed_at DESC, id DESC
			LIMIT $2)
			UNION ALL
			(SELECT * FROM provider_balance_snapshots
			 WHERE provider_account_id = $1 AND observed_at < $3
			   AND status = 'success' AND balance_kind = 'usage'
			   AND COALESCE(normalization_error, '') = ''
			 ORDER BY observed_at DESC, id DESC LIMIT 1)
		) recent
		ORDER BY observed_at, id
	`, accountID, query.Limit+1, query.From, query.To)
	if err != nil {
		return nil, fmt.Errorf("list provider balance history: %w", err)
	}
	defer rows.Close()

	history := []providerbalance.Snapshot{}
	for rows.Next() {
		var snapshot providerbalance.Snapshot
		var windows []byte
		if err := rows.Scan(
			&snapshot.ID, &snapshot.ProviderAccountID, &snapshot.BillingMode, &snapshot.Status,
			&snapshot.BalanceKind, &snapshot.Currency, &snapshot.Unit,
			&snapshot.AmountLimit, &snapshot.AmountUsed, &snapshot.AmountSpent, &snapshot.AmountRemaining,
			&windows, &snapshot.NormalizationVersion, &snapshot.NormalizationError,
			&snapshot.Source, &snapshot.ErrorCode, &snapshot.ErrorMessage, &snapshot.ObservedAt,
		); err != nil {
			return nil, fmt.Errorf("scan provider balance history: %w", err)
		}
		if err := json.Unmarshal(windows, &snapshot.UsageWindows); err != nil {
			return nil, fmt.Errorf("decode provider balance history windows: %w", err)
		}
		history = append(history, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list provider balance history rows: %w", err)
	}
	return history, nil
}

// GetBalanceSnapshot returns one full observation, including its raw successful
// upstream payload and allowlisted usage headers.
func (s *ProviderBalanceStore) GetBalanceSnapshot(ctx context.Context, accountID string, snapshotID int64) (*providerbalance.Snapshot, error) {
	var snapshot providerbalance.Snapshot
	var windows, rawPayload, rawHeaders []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, provider_account_id, billing_mode, status,
		       COALESCE(balance_kind, ''), COALESCE(currency, ''), COALESCE(unit, ''),
		       amount_limit::double precision, amount_used::double precision,
		       amount_spent::double precision, amount_remaining::double precision, usage_windows,
		       normalization_version, COALESCE(normalization_error, ''),
		       raw_payload, raw_headers,
		       COALESCE(source, ''), COALESCE(error_code, ''), COALESCE(error_message, ''), observed_at
		FROM provider_balance_snapshots
		WHERE provider_account_id = $1 AND id = $2
	`, accountID, snapshotID).Scan(
		&snapshot.ID, &snapshot.ProviderAccountID, &snapshot.BillingMode, &snapshot.Status,
		&snapshot.BalanceKind, &snapshot.Currency, &snapshot.Unit,
		&snapshot.AmountLimit, &snapshot.AmountUsed, &snapshot.AmountSpent, &snapshot.AmountRemaining,
		&windows, &snapshot.NormalizationVersion, &snapshot.NormalizationError, &rawPayload, &rawHeaders,
		&snapshot.Source, &snapshot.ErrorCode, &snapshot.ErrorMessage, &snapshot.ObservedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get provider balance snapshot %d: %w", snapshotID, err)
	}
	if err := json.Unmarshal(windows, &snapshot.UsageWindows); err != nil {
		return nil, fmt.Errorf("decode provider balance snapshot windows: %w", err)
	}
	if len(rawPayload) > 0 && string(rawPayload) != "null" {
		snapshot.RawPayload = append(json.RawMessage(nil), rawPayload...)
	}
	if len(rawHeaders) > 0 && string(rawHeaders) != "null" {
		if err := json.Unmarshal(rawHeaders, &snapshot.RawHeaders); err != nil {
			return nil, fmt.Errorf("decode provider balance snapshot headers: %w", err)
		}
	}
	return &snapshot, nil
}

func nullableRawJSON(value json.RawMessage) interface{} {
	if len(value) == 0 {
		return nil
	}
	return value
}

func balanceAccountFromConfig(id, provider string, config map[string]interface{}, credentialID *string) providerbalance.Account {
	account := providerbalance.Account{
		ID:             id,
		Provider:       provider,
		BalanceEnabled: supportedBalanceProvider(provider),
	}
	if credentialID != nil {
		account.CredentialRef = credentialref.ProviderCredential(*credentialID)
	}
	if value, ok := config["upstream_base_url"].(string); ok {
		account.UpstreamBaseURL = strings.TrimSpace(value)
	}
	if value, ok := config["catalog_key"].(string); ok {
		account.CatalogKey = strings.TrimSpace(value)
	}
	if billing, ok := config["billing"].(map[string]interface{}); ok {
		account.BillingMode, _ = billing["mode"].(string)
		account.BillingCurrency, _ = billing["currency"].(string)
		if account.BillingMode == "subscription" {
			account.BalanceEnabled = true
		}
	}
	balanceEnabledExplicit := false
	if balance, ok := config["balance"].(map[string]interface{}); ok {
		if value, exists := balance["enabled"].(bool); exists {
			account.BalanceEnabled = value
			balanceEnabledExplicit = true
		}
		account.BalanceURL, _ = balance["url"].(string)
	}
	if !balanceEnabledExplicit && isZAIBalanceAccount(account) {
		account.BalanceEnabled = true
	}
	return account
}

func isZAIBalanceAccount(account providerbalance.Account) bool {
	if strings.EqualFold(account.CatalogKey, "zai") || strings.EqualFold(account.CatalogKey, "zai-coding-cn") {
		return true
	}
	base, err := url.Parse(account.UpstreamBaseURL)
	if err != nil || base.Scheme != "https" {
		return false
	}
	switch strings.ToLower(base.Hostname()) {
	case "api.z.ai", "open.bigmodel.cn", "dev.bigmodel.cn":
		return true
	default:
		return false
	}
}

func supportedBalanceProvider(provider string) bool {
	switch provider {
	case "deepseek", "deepseek-completions", "openai-codex", "openai-codex-responses":
		return true
	default:
		return false
	}
}
