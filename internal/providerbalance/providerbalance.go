// Package providerbalance periodically records the balance or remaining
// subscription usage reported by supported upstream providers.
package providerbalance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const maxResponseBytes = 1 << 20

// Account is the non-secret provider configuration required by the poller.
type Account struct {
	ID              string
	Provider        string
	CatalogKey      string
	UpstreamBaseURL string
	CredentialRef   string
	BillingMode     string
	BillingCurrency string
	BalanceEnabled  bool
	BalanceURL      string
}

// Credential is the minimum decrypted credential material required to query
// provider balance endpoints.
type Credential struct {
	Secret    string
	AccountID string
}

// UsageWindow describes one upstream subscription allowance window.
type UsageWindow struct {
	Name             string     `json:"name"`
	Limit            *float64   `json:"limit,omitempty"`
	Used             *float64   `json:"used,omitempty"`
	Remaining        *float64   `json:"remaining,omitempty"`
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	WindowSeconds    int64      `json:"window_seconds,omitempty"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
}

// Snapshot is one timestamped upstream observation. Failed observations are
// retained as status=error without overwriting the last successful row.
type Snapshot struct {
	ID                   int64               `json:"id,omitempty"`
	ProviderAccountID    string              `json:"provider_account_id"`
	BillingMode          string              `json:"billing_mode"`
	Status               string              `json:"status"`
	BalanceKind          string              `json:"balance_kind,omitempty"`
	Currency             string              `json:"currency,omitempty"`
	Unit                 string              `json:"unit,omitempty"`
	AmountLimit          *float64            `json:"amount_limit,omitempty"`
	AmountUsed           *float64            `json:"amount_used,omitempty"`
	AmountSpent          *float64            `json:"amount_spent,omitempty"`
	AmountRemaining      *float64            `json:"amount_remaining,omitempty"`
	UsageWindows         []UsageWindow       `json:"usage_windows,omitempty"`
	NormalizationVersion int                 `json:"normalization_version"`
	NormalizationError   string              `json:"normalization_error,omitempty"`
	RawPayload           json.RawMessage     `json:"raw_payload,omitempty"`
	RawHeaders           map[string][]string `json:"raw_headers,omitempty"`
	Source               string              `json:"source,omitempty"`
	ErrorCode            string              `json:"error_code,omitempty"`
	ErrorMessage         string              `json:"error_message,omitempty"`
	ObservedAt           time.Time           `json:"observed_at"`
}

// Store supplies accounts and persists observations.
type Store interface {
	ListBalanceAccounts(ctx context.Context) ([]Account, error)
	GetBalanceAccount(ctx context.Context, accountID string) (*Account, error)
	InsertBalanceSnapshot(ctx context.Context, snapshot *Snapshot) error
}

// CredentialResolver decrypts an active provider credential by reference.
type CredentialResolver interface {
	ResolveCredential(ctx context.Context, ref string) (*Credential, error)
}

// CredentialResolverFunc adapts a function to CredentialResolver.
type CredentialResolverFunc func(context.Context, string) (*Credential, error)

func (f CredentialResolverFunc) ResolveCredential(ctx context.Context, ref string) (*Credential, error) {
	return f(ctx, ref)
}

// Worker polls supported upstreams on an interval.
type Worker struct {
	store       Store
	credentials CredentialResolver
	client      *http.Client
	interval    time.Duration
	logger      zerolog.Logger
}

// NewWorker creates a provider balance worker. A non-positive interval disables
// the background loop while retaining manual RefreshAccount support.
func NewWorker(store Store, credentials CredentialResolver, client *http.Client, interval time.Duration, logger zerolog.Logger) *Worker {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Worker{store: store, credentials: credentials, client: client, interval: interval, logger: logger}
}

// Start begins an immediate refresh followed by periodic refreshes until ctx is
// cancelled. It is safe to call only once.
func (w *Worker) Start(ctx context.Context) {
	if w == nil || w.interval <= 0 || w.store == nil || w.credentials == nil {
		return
	}
	go func() {
		w.RefreshAll(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.RefreshAll(ctx)
			}
		}
	}()
}

// RefreshAll refreshes every active, supported provider account.
func (w *Worker) RefreshAll(ctx context.Context) {
	accounts, err := w.store.ListBalanceAccounts(ctx)
	if err != nil {
		w.logger.Error().Err(err).Msg("list provider accounts for balance refresh")
		return
	}
	for i := range accounts {
		if _, err := w.refresh(ctx, &accounts[i]); err != nil {
			w.logger.Warn().Err(err).Str("provider_account_id", accounts[i].ID).
				Msg("provider balance refresh failed")
		}
	}
}

// RefreshAccount performs an on-demand refresh and returns the recorded
// observation, including status=error observations.
func (w *Worker) RefreshAccount(ctx context.Context, accountID string) (*Snapshot, error) {
	if w == nil || w.store == nil || w.credentials == nil {
		return nil, fmt.Errorf("provider balance refresh is unavailable")
	}
	account, err := w.store.GetBalanceAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return w.refresh(ctx, account)
}

func (w *Worker) refresh(ctx context.Context, account *Account) (*Snapshot, error) {
	now := time.Now().UTC()
	snapshot := &Snapshot{
		ProviderAccountID:    account.ID,
		BillingMode:          displayBillingMode(account.BillingMode),
		Status:               "error",
		NormalizationVersion: 1,
		ObservedAt:           now,
	}

	if !account.BalanceEnabled {
		return nil, nil
	}
	if account.CredentialRef == "" {
		return w.recordError(ctx, snapshot, "credential_unavailable", "no active credential")
	}
	credential, err := w.credentials.ResolveCredential(ctx, account.CredentialRef)
	if err != nil || credential == nil || strings.TrimSpace(credential.Secret) == "" {
		return w.recordError(ctx, snapshot, "credential_unavailable", "active credential could not be loaded")
	}

	switch {
	case isZAIAccount(account):
		err = w.fetchZAI(ctx, account, credential, snapshot)
	case normalizedProvider(account.Provider) == "deepseek":
		err = w.fetchDeepSeek(ctx, account, credential, snapshot)
	case normalizedProvider(account.Provider) == "openai-codex":
		err = w.fetchCodex(ctx, account, credential, snapshot)
	default:
		if account.BillingMode != "subscription" {
			return nil, nil
		}
		if strings.TrimSpace(account.BalanceURL) == "" {
			return w.recordError(ctx, snapshot, "balance_api_unavailable", "upstream does not expose a supported balance API")
		}
		err = w.fetchGenericSubscription(ctx, account, credential, snapshot)
	}
	if err != nil {
		return w.recordError(ctx, snapshot, "upstream_query_failed", err.Error())
	}
	if err := w.store.InsertBalanceSnapshot(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (w *Worker) recordError(ctx context.Context, snapshot *Snapshot, code, message string) (*Snapshot, error) {
	snapshot.ErrorCode = code
	snapshot.ErrorMessage = message
	if err := w.store.InsertBalanceSnapshot(ctx, snapshot); err != nil {
		return nil, err
	}
	return snapshot, fmt.Errorf("%s", message)
}

func (w *Worker) fetchDeepSeek(ctx context.Context, account *Account, credential *Credential, snapshot *Snapshot) error {
	endpoint, err := balanceEndpoint(account, "/user/balance")
	if err != nil {
		return err
	}
	var payload struct {
		Available bool `json:"is_available"`
		Balances  []struct {
			Currency string `json:"currency"`
			Total    string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	rawPayload, rawHeaders, err := w.getJSON(ctx, endpoint, credential, false, &payload)
	if err != nil {
		return err
	}
	snapshot.Status = "success"
	snapshot.RawPayload = rawPayload
	snapshot.RawHeaders = rawHeaders
	snapshot.Source = safeSource(endpoint)
	if len(payload.Balances) == 0 {
		snapshot.NormalizationError = "upstream response did not include a recognized balance"
		return nil
	}
	selected := payload.Balances[0]
	for _, candidate := range payload.Balances {
		if strings.EqualFold(candidate.Currency, account.BillingCurrency) ||
			(account.BillingCurrency == "" && strings.EqualFold(candidate.Currency, "USD")) {
			selected = candidate
			break
		}
	}
	amount, err := strconv.ParseFloat(selected.Total, 64)
	if err != nil {
		snapshot.NormalizationError = "upstream returned an invalid balance amount"
		return nil
	}
	snapshot.BalanceKind = "monetary"
	snapshot.Currency = strings.ToUpper(selected.Currency)
	snapshot.Unit = strings.ToUpper(selected.Currency)
	snapshot.AmountRemaining = &amount
	return nil
}

func (w *Worker) fetchCodex(ctx context.Context, account *Account, credential *Credential, snapshot *Snapshot) error {
	endpoint, err := codexBalanceEndpoint(account)
	if err != nil {
		return err
	}
	var payload codexUsagePayload
	rawPayload, rawHeaders, err := w.getJSON(ctx, endpoint, credential, true, &payload)
	if err != nil {
		return err
	}
	snapshot.BillingMode = "subscription"
	snapshot.Status = "success"
	snapshot.RawPayload = rawPayload
	snapshot.RawHeaders = rawHeaders
	snapshot.Source = safeSource(endpoint)
	windows := make([]UsageWindow, 0, 3)
	if payload.RateLimit.Primary != nil {
		windows = append(windows, codexWindow("primary", payload.RateLimit.Primary))
	}
	if payload.RateLimit.Secondary != nil {
		windows = append(windows, codexWindow("secondary", payload.RateLimit.Secondary))
	}
	if payload.SpendControl.IndividualLimit != nil && payload.SpendControl.IndividualLimit.RemainingPercent != nil {
		remaining := clampPercent(*payload.SpendControl.IndividualLimit.RemainingPercent)
		used := 100 - remaining
		window := UsageWindow{Name: "monthly", UsedPercent: used, RemainingPercent: remaining}
		window.ResetsAt = unixTime(payload.SpendControl.IndividualLimit.ResetAt)
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		snapshot.NormalizationError = "upstream response did not include recognized subscription usage windows"
		return nil
	}
	remaining := windows[0].RemainingPercent
	for _, window := range windows[1:] {
		if window.RemainingPercent < remaining {
			remaining = window.RemainingPercent
		}
	}
	snapshot.BalanceKind = "usage"
	snapshot.Currency = "percent"
	snapshot.Unit = "percent"
	limit := 100.0
	used := 100 - remaining
	snapshot.AmountLimit = &limit
	snapshot.AmountUsed = &used
	snapshot.AmountRemaining = &remaining
	snapshot.UsageWindows = windows
	return nil
}

// fetchZAI records the same quota, model-usage, and tool-usage documents used
// by ZAI's official GLM Coding Plan usage plugin. The quota document is
// required; the two detailed usage documents are best-effort so a temporary
// reporting outage does not discard an otherwise useful allowance snapshot.
func (w *Worker) fetchZAI(ctx context.Context, account *Account, credential *Credential, snapshot *Snapshot) error {
	endpoints, err := zaiUsageEndpoints(account)
	if err != nil {
		return err
	}

	var quotaLimits []zaiLimit
	var quotaRaw json.RawMessage
	var rawHeaders map[string][]string
	var bearerAuth bool
	var queryErr error
	for _, useBearer := range []bool{true, false} {
		candidate := zaiUsageResponse{}
		candidateRaw, candidateHeaders, candidateErr := w.getZAIJSON(ctx, endpoints.quota, credential, useBearer, &candidate)
		if candidateErr != nil {
			queryErr = candidateErr
			continue
		}
		candidateLimits, hasLimits := candidate.limits()
		if !hasLimits {
			queryErr = fmt.Errorf("upstream quota endpoint did not return a quota document")
			continue
		}
		quotaLimits = candidateLimits
		quotaRaw = candidateRaw
		rawHeaders = candidateHeaders
		bearerAuth = useBearer
		queryErr = nil
		break
	}
	if queryErr != nil {
		return w.recordValidZAIWithoutQuota(ctx, account, credential, endpoints, snapshot, queryErr)
	}

	raw := map[string]json.RawMessage{"quota": quotaRaw}
	start, end := zaiUsageRange(time.Now().UTC())
	for name, endpoint := range map[string]string{
		"model_usage": addUsageRange(endpoints.model, start, end),
		"tool_usage":  addUsageRange(endpoints.tool, start, end),
	} {
		var detail json.RawMessage
		detailRaw, _, detailErr := w.getZAIJSON(ctx, endpoint, credential, bearerAuth, &detail)
		if detailErr == nil {
			raw[name] = detailRaw
		}
	}
	combinedRaw, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode upstream usage response")
	}

	snapshot.Status = "success"
	snapshot.BillingMode = zaiBillingMode(account)
	snapshot.RawPayload = combinedRaw
	snapshot.RawHeaders = rawHeaders
	snapshot.Source = safeSource(endpoints.quota)

	windows := make([]UsageWindow, 0, len(quotaLimits))
	for _, limit := range quotaLimits {
		window, ok := zaiWindow(limit)
		if ok {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		snapshot.NormalizationError = "upstream response did not include recognized ZAI usage limits"
		return nil
	}

	remaining := 100.0
	hasPlanWindow := false
	for _, window := range windows {
		if window.Name == "mcp" {
			continue
		}
		hasPlanWindow = true
		if window.RemainingPercent < remaining {
			remaining = window.RemainingPercent
		}
	}
	if !hasPlanWindow {
		remaining = windows[0].RemainingPercent
	}
	limit := 100.0
	used := 100 - remaining
	snapshot.BalanceKind = "usage"
	snapshot.Currency = "percent"
	snapshot.Unit = "percent"
	snapshot.AmountLimit = &limit
	snapshot.AmountUsed = &used
	snapshot.AmountRemaining = &remaining
	snapshot.UsageWindows = windows
	return nil
}

func (w *Worker) recordValidZAIWithoutQuota(ctx context.Context, account *Account, credential *Credential, endpoints zaiEndpoints, snapshot *Snapshot, quotaErr error) error {
	var models struct {
		Data json.RawMessage `json:"data"`
	}
	modelsRaw, rawHeaders, err := w.getZAIJSON(ctx, endpoints.models, credential, true, &models)
	if err != nil || len(models.Data) == 0 || string(models.Data) == "null" {
		return quotaErr
	}
	combinedRaw, err := json.Marshal(map[string]json.RawMessage{"models": modelsRaw})
	if err != nil {
		return fmt.Errorf("encode upstream model response")
	}
	snapshot.Status = "success"
	snapshot.BillingMode = zaiBillingMode(account)
	snapshot.RawPayload = combinedRaw
	snapshot.RawHeaders = rawHeaders
	snapshot.Source = safeSource(endpoints.models)
	snapshot.NormalizationError = "ZAI token is valid, but upstream quota usage is unavailable"
	return nil
}

// fetchGenericSubscription queries an explicitly configured balance endpoint
// and accepts only explicit plan/quota headers. It deliberately ignores ordinary
// x-ratelimit-* request-throttling headers because those are not proof of the
// remaining subscription allowance.
func (w *Worker) fetchGenericSubscription(ctx context.Context, account *Account, credential *Credential, snapshot *Snapshot) error {
	endpoint, err := genericSubscriptionEndpoint(account)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build upstream balance request")
	}
	req.Header.Set("Authorization", "Bearer "+credential.Secret)
	req.Header.Set("Accept", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("upstream balance request failed")
	}
	defer resp.Body.Close()
	body, err := readResponseBody(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream balance endpoint returned HTTP %d", resp.StatusCode)
	}
	snapshot.Status = "success"
	snapshot.BillingMode = "subscription"
	snapshot.RawPayload = validJSON(body)
	snapshot.RawHeaders = safeUsageHeaders(resp.Header)
	snapshot.Source = safeSource(endpoint)

	remaining, unit, ok := firstNumericHeader(resp.Header,
		"X-Plan-Remaining-Credits", "X-Quota-Remaining-Credits", "X-Quota-Remaining", "X-Usage-Remaining")
	if !ok {
		if snapshot.RawPayload == nil && len(snapshot.RawHeaders) == 0 {
			return fmt.Errorf("upstream balance response did not contain JSON or usage headers")
		}
		snapshot.NormalizationError = "upstream response did not include recognized subscription quota fields"
		return nil
	}
	limit, _, hasLimit := firstNumericHeader(resp.Header,
		"X-Plan-Limit-Credits", "X-Quota-Limit-Credits", "X-Quota-Limit", "X-Usage-Limit")
	window := UsageWindow{Name: "subscription", Remaining: &remaining}
	if hasLimit && limit > 0 {
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		window.Limit = &limit
		window.Used = &used
		window.RemainingPercent = clampPercent(remaining / limit * 100)
		window.UsedPercent = 100 - window.RemainingPercent
		snapshot.AmountLimit = &limit
		snapshot.AmountUsed = &used
	}
	snapshot.BalanceKind = "usage"
	snapshot.Unit = unit
	snapshot.AmountRemaining = &remaining
	snapshot.UsageWindows = []UsageWindow{window}
	return nil
}

func firstNumericHeader(headers http.Header, names ...string) (float64, string, bool) {
	for _, name := range names {
		raw := strings.TrimSpace(headers.Get(name))
		if raw == "" {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		unit := "requests"
		if strings.Contains(strings.ToLower(name), "credit") {
			unit = "credits"
		}
		return value, unit, true
	}
	return 0, "", false
}

func (w *Worker) getJSON(ctx context.Context, endpoint string, credential *Credential, codex bool, dst any) (json.RawMessage, map[string][]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream balance request")
	}
	req.Header.Set("Authorization", "Bearer "+credential.Secret)
	req.Header.Set("Accept", "application/json")
	if codex && credential.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", credential.AccountID)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream balance request failed")
	}
	defer resp.Body.Close()
	body, err := readResponseBody(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("upstream balance endpoint returned HTTP %d", resp.StatusCode)
	}
	raw := validJSON(body)
	if raw == nil {
		return nil, nil, fmt.Errorf("decode upstream balance response")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return nil, nil, fmt.Errorf("decode upstream balance response")
	}
	return raw, safeUsageHeaders(resp.Header), nil
}

func (w *Worker) getZAIJSON(ctx context.Context, endpoint string, credential *Credential, bearerAuth bool, dst any) (json.RawMessage, map[string][]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream usage request")
	}
	authorization := credential.Secret
	if bearerAuth {
		authorization = "Bearer " + credential.Secret
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("upstream usage request failed")
	}
	defer resp.Body.Close()
	body, err := readResponseBody(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("upstream usage endpoint returned HTTP %d", resp.StatusCode)
	}
	raw := validJSON(body)
	if raw == nil {
		return nil, nil, fmt.Errorf("decode upstream usage response")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return nil, nil, fmt.Errorf("decode upstream usage response")
	}
	return raw, safeUsageHeaders(resp.Header), nil
}

func readResponseBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream balance response")
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("upstream balance response exceeded %d bytes", maxResponseBytes)
	}
	return data, nil
}

func validJSON(data []byte) json.RawMessage {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || !json.Valid(data) {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

// safeUsageHeaders retains provider quota metadata without persisting cookies,
// authorization material, or unrelated response headers.
func safeUsageHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string)
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-plan-") &&
			!strings.HasPrefix(lower, "x-quota-") &&
			!strings.HasPrefix(lower, "x-usage-") &&
			!strings.HasPrefix(lower, "x-ratelimit-") &&
			!strings.HasPrefix(lower, "ratelimit-") {
			continue
		}
		result[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

type codexUsagePayload struct {
	RateLimit struct {
		Primary   *codexWindowPayload `json:"primary_window"`
		Secondary *codexWindowPayload `json:"secondary_window"`
	} `json:"rate_limit"`
	SpendControl struct {
		IndividualLimit *struct {
			RemainingPercent *float64 `json:"remaining_percent"`
			ResetAt          int64    `json:"reset_at"`
		} `json:"individual_limit"`
	} `json:"spend_control"`
}

type zaiUsageResponse struct {
	Code    int  `json:"code"`
	Success bool `json:"success"`
	Data    *struct {
		Limits []zaiLimit `json:"limits"`
	} `json:"data"`
	Limits []zaiLimit `json:"limits"`
}

func (r zaiUsageResponse) limits() ([]zaiLimit, bool) {
	if r.Data != nil && r.Data.Limits != nil {
		return r.Data.Limits, true
	}
	if r.Limits != nil {
		return r.Limits, true
	}
	return nil, false
}

type zaiLimit struct {
	Type          string   `json:"type"`
	Unit          int      `json:"unit"`
	Number        int      `json:"number"`
	Usage         *float64 `json:"usage"`
	CurrentValue  *float64 `json:"currentValue"`
	Remaining     *float64 `json:"remaining"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime int64    `json:"nextResetTime"`
}

func zaiWindow(limit zaiLimit) (UsageWindow, bool) {
	name := ""
	switch limit.Type {
	case "TOKENS_LIMIT", "CREDIT_LIMIT":
		name = "plan"
	case "TIME_LIMIT":
		name = "mcp"
	default:
		return UsageWindow{}, false
	}

	windowSeconds := zaiWindowSeconds(limit.Unit, limit.Number)
	if name == "mcp" && limit.Unit == 5 && limit.Number == 1 {
		windowSeconds = 30 * 24 * 60 * 60
	}
	if windowSeconds == 5*60*60 {
		name = "primary"
	} else if windowSeconds == 7*24*60*60 {
		name = "secondary"
	}

	usedPercent := 0.0
	if limit.Percentage != nil {
		usedPercent = clampPercent(*limit.Percentage)
	}
	if limit.Usage != nil && *limit.Usage > 0 {
		var used *float64
		if limit.CurrentValue != nil {
			value := *limit.CurrentValue
			used = &value
		}
		if limit.Remaining != nil {
			fromRemaining := *limit.Usage - *limit.Remaining
			if used == nil || fromRemaining > *used {
				used = &fromRemaining
			}
		}
		if used != nil {
			usedPercent = clampPercent(*used / *limit.Usage * 100)
		}
	}
	remainingPercent := 100 - usedPercent

	window := UsageWindow{
		Name:             name,
		Limit:            limit.Usage,
		Used:             limit.CurrentValue,
		Remaining:        limit.Remaining,
		UsedPercent:      usedPercent,
		RemainingPercent: remainingPercent,
		WindowSeconds:    windowSeconds,
		ResetsAt:         unixMillis(limit.NextResetTime),
	}
	if window.Used == nil && window.Limit != nil && window.Remaining != nil {
		value := *window.Limit - *window.Remaining
		window.Used = &value
	}
	if window.Remaining == nil && window.Limit != nil && window.Used != nil {
		value := *window.Limit - *window.Used
		window.Remaining = &value
	}
	return window, true
}

func zaiWindowSeconds(unit, number int) int64 {
	if number <= 0 {
		return 0
	}
	minutesPerUnit := map[int]int64{1: 24 * 60, 3: 60, 5: 1, 6: 7 * 24 * 60}
	minutes, ok := minutesPerUnit[unit]
	if !ok {
		return 0
	}
	return int64(number) * minutes * 60
}

func unixMillis(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	result := time.UnixMilli(value).UTC()
	return &result
}

type codexWindowPayload struct {
	UsedPercent float64 `json:"used_percent"`
	WindowSecs  int64   `json:"limit_window_seconds"`
	ResetAt     int64   `json:"reset_at"`
}

func codexWindow(name string, payload *codexWindowPayload) UsageWindow {
	used := clampPercent(payload.UsedPercent)
	return UsageWindow{
		Name:             name,
		UsedPercent:      used,
		RemainingPercent: 100 - used,
		WindowSeconds:    payload.WindowSecs,
		ResetsAt:         unixTime(payload.ResetAt),
	}
}

func unixTime(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	result := time.Unix(value, 0).UTC()
	return &result
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func balanceEndpoint(account *Account, defaultPath string) (string, error) {
	raw := strings.TrimSpace(account.BalanceURL)
	if raw == "" {
		raw = strings.TrimRight(account.UpstreamBaseURL, "/") + defaultPath
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("balance endpoint must be an absolute HTTPS URL")
	}
	return parsed.String(), nil
}

func safeSource(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.User = nil
	return parsed.String()
}

func codexBalanceEndpoint(account *Account) (string, error) {
	if strings.TrimSpace(account.BalanceURL) != "" {
		return balanceEndpoint(account, "")
	}
	base := strings.TrimRight(account.UpstreamBaseURL, "/")
	if strings.Contains(base, "chatgpt.com/backend-api") {
		if index := strings.Index(base, "/codex"); index >= 0 {
			base = base[:index]
		}
		copy := *account
		copy.UpstreamBaseURL = base
		return balanceEndpoint(&copy, "/wham/usage")
	}
	return balanceEndpoint(account, "/api/codex/usage")
}

func genericSubscriptionEndpoint(account *Account) (string, error) {
	return balanceEndpoint(account, "")
}

type zaiEndpoints struct {
	quota  string
	model  string
	tool   string
	models string
}

func zaiUsageEndpoints(account *Account) (zaiEndpoints, error) {
	raw := strings.TrimSpace(account.BalanceURL)
	if raw == "" {
		raw = strings.TrimSpace(account.UpstreamBaseURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return zaiEndpoints{}, fmt.Errorf("ZAI usage endpoint must be an absolute HTTPS URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.TrimSpace(account.BalanceURL) == "" && host != "api.z.ai" && host != "open.bigmodel.cn" && host != "dev.bigmodel.cn" {
		return zaiEndpoints{}, fmt.Errorf("unsupported ZAI usage host")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	quota := origin + "/api/monitor/usage/quota/limit"
	if strings.TrimSpace(account.BalanceURL) != "" {
		quota = parsed.String()
	}
	return zaiEndpoints{
		quota:  quota,
		model:  origin + "/api/monitor/usage/model-usage",
		tool:   origin + "/api/monitor/usage/tool-usage",
		models: strings.TrimRight(account.UpstreamBaseURL, "/") + "/models",
	}, nil
}

func isZAIAccount(account *Account) bool {
	if strings.EqualFold(strings.TrimSpace(account.CatalogKey), "zai") ||
		strings.EqualFold(strings.TrimSpace(account.CatalogKey), "zai-coding-cn") {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(account.UpstreamBaseURL))
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "api.z.ai", "open.bigmodel.cn", "dev.bigmodel.cn":
		return true
	default:
		return false
	}
}

func zaiBillingMode(account *Account) string {
	if account.BillingMode == "subscription" {
		return "subscription"
	}
	parsed, err := url.Parse(strings.TrimSpace(account.UpstreamBaseURL))
	if err == nil && (strings.Contains(parsed.Path, "/api/coding/") || strings.HasPrefix(parsed.Path, "/api/anthropic")) {
		return "subscription"
	}
	return "pay_as_you_go"
}

func zaiUsageRange(now time.Time) (time.Time, time.Time) {
	end := now.UTC().Truncate(time.Hour).Add(time.Hour - time.Second)
	start := end.Add(-24*time.Hour + time.Second)
	return start, end
}

func addUsageRange(endpoint string, start, end time.Time) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	query := parsed.Query()
	query.Set("startTime", start.Format("2006-01-02 15:04:05"))
	query.Set("endTime", end.Format("2006-01-02 15:04:05"))
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func normalizedProvider(provider string) string {
	switch provider {
	case "deepseek", "deepseek-completions":
		return "deepseek"
	case "openai-codex", "openai-codex-responses":
		return "openai-codex"
	default:
		return provider
	}
}

func displayBillingMode(mode string) string {
	if mode == "subscription" {
		return mode
	}
	return "pay_as_you_go"
}
