package dataplane

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
	"github.com/earlgrey/piproxy/internal/usage"
)

// Services groups optional side-effect dependencies for provider handlers.
type Services struct {
	Credentials        *secrets.CredentialStore
	Usage              *usage.Recorder
	CostTable          *usage.KnownCostTable
	Audit              *audit.Logger
	Quota              *quota.Enforcer
	CodexQuotaDisabled bool
	Capture            *capture.Recorder
	CaptureConfig      CaptureSettings
	Hooks              *hooks.Manager
	ModelAccess        modelaccess.Store
	ModelGuards        modelguard.Rules
	SubscriptionRates  subscriptioncost.RateResolver
}

// CaptureSettings controls per-request capture behavior.
type CaptureSettings struct {
	MaxPayloadBytes int64
	RetentionDays   int
}

// ApplyCredential injects the configured upstream credential into headers.
func ApplyCredential(ctx context.Context, headers http.Header, provider *providers.Provider, store *secrets.CredentialStore) error {
	if provider == nil || provider.CredentialID == "" {
		return nil
	}
	if store == nil {
		return fmt.Errorf("credential store is not configured")
	}
	cred, err := store.GetCredential(ctx, provider.CredentialID)
	if err != nil {
		return err
	}
	secret := cred.SecretValue()
	if secret == "" {
		return fmt.Errorf("credential %s does not contain usable secret material", provider.CredentialID)
	}
	rewrite.InjectCredentialHeaders(headers, provider.Type, secret)
	// Codex (ChatGPT) backend requires the account id header alongside the
	// bearer access token; it is part of the codex auth material, not a
	// client-identity header, so it is injected here from the stored credential.
	if provider.Type == providers.ProviderOpenAICodexResponses && cred.AccountID != "" {
		headers.Set("chatgpt-account-id", cred.AccountID)
	}
	return nil
}

// RecordUsage queues a usage event when a recorder is configured.
func RecordUsage(r *http.Request, recorder *usage.Recorder, provider *providers.Provider, model, status, errorClass string, inputTokens, outputTokens int64, estimated bool, startedAt time.Time) {
	RecordUsageWithCache(r, recorder, provider, model, status, errorClass, usage.TokenUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}, estimated, startedAt)
}

// RecordUsageWithCache queues a usage event with provider cache token classes.
func RecordUsageWithCache(r *http.Request, recorder *usage.Recorder, provider *providers.Provider, model, status, errorClass string, tokenUsage usage.TokenUsage, estimated bool, startedAt time.Time) {
	totalTokens := tokenUsage.TotalTokens
	if totalTokens == 0 {
		outputTokens := tokenUsage.OutputTokens
		if outputTokens < tokenUsage.ReasoningTokens {
			outputTokens = tokenUsage.ReasoningTokens
		}
		totalTokens = tokenUsage.InputTokens + outputTokens + tokenUsage.CacheReadTokens + tokenUsage.CacheWriteTokens
	}
	if provider != nil {
		middleware.AddRequestLogFields(r.Context(), logging.Fields{
			Provider:   string(provider.Type),
			Model:      model,
			ErrorClass: errorClass,
			Tokens:     totalTokens,
		})
	}
	if recorder == nil || provider == nil {
		return
	}
	authCtx := middleware.GetAuthContext(r.Context())
	now := time.Now()
	event := &usage.Event{
		ID:                id.New("use"),
		RequestID:         middleware.GetRequestID(r.Context()),
		ProjectID:         capture.ExtractProjectIDFromHeaders(r.Header),
		ExtensionVersion:  capture.ExtractExtensionVersionFromHeaders(r.Header),
		Provider:          string(provider.Type),
		Model:             model,
		UpstreamProvider:  string(provider.Type),
		UpstreamModel:     model,
		ProviderAccountID: provider.ID,
		Status:            status,
		ErrorClass:        errorClass,
		InputTokens:       tokenUsage.InputTokens,
		OutputTokens:      tokenUsage.OutputTokens,
		ReasoningTokens:   tokenUsage.ReasoningTokens,
		CacheReadTokens:   tokenUsage.CacheReadTokens,
		CacheWriteTokens:  tokenUsage.CacheWriteTokens,
		TotalTokens:       totalTokens,
		EstimatedUsage:    estimated,
		ClientIP:          ratelimit.ClientIP(r),
		StartedAt:         startedAt,
		CompletedAt:       &now,
		DurationMs:        now.Sub(startedAt).Milliseconds(),
	}
	pricingServices := &Services{}
	if recorder != nil {
		pricingServices.SubscriptionRates = recorder.SubscriptionRates()
	}
	if cost, rateCardID, basis, explicit := ProviderCostTableForModel(pricingServices, provider, model); explicit {
		event.CostInputCents = float64(tokenUsage.InputTokens) / 1_000_000 * cost.InputPricePerMillion * 100
		event.CostOutputCents = float64(tokenUsage.OutputTokens) / 1_000_000 * cost.OutputPricePerMillion * 100
		event.CostCacheReadCents = float64(tokenUsage.CacheReadTokens) / 1_000_000 * cost.CacheReadPricePerMillion * 100
		event.CostCacheWriteCents = float64(tokenUsage.CacheWriteTokens) / 1_000_000 * cost.CacheWritePricePerMillion * 100
		event.CostTotalCents = event.CostInputCents + event.CostOutputCents + event.CostCacheReadCents + event.CostCacheWriteCents
		if basis == "subscription_rate_card" {
			// The subscription rate is explicitly a blended total-token price.
			// Prefer the upstream total so provider-specific token detail cannot
			// accidentally under- or double-charge the subscription allocation.
			event.CostTotalCents = float64(totalTokens) / 1_000_000 * cost.InputPricePerMillion * 100
		}
		event.CostCalculated = true
		event.SubscriptionRateCardID = rateCardID
		event.CostBasis = basis
	} else if provider.Billing.Mode == "subscription" {
		// Subscription traffic must never silently fall through to the recorder's
		// public model list prices. Authenticated requests are rejected earlier
		// when no active card exists; this also keeps pre-quota error events honest.
		event.CostCalculated = true
		event.CostBasis = "subscription_rate_card"
	}
	if authCtx != nil {
		event.TenantID = authCtx.TenantID
		event.UserID = authCtx.UserID
		event.KeyID = authCtx.KeyID
	}
	recorder.Record(event)
}

// RecordAudit queues a data-plane audit event when a logger is configured.
func RecordAudit(r *http.Request, logger *audit.Logger, provider *providers.Provider, model, result string) {
	if logger == nil || provider == nil {
		return
	}
	authCtx := middleware.GetAuthContext(r.Context())
	event := &audit.Event{
		ID:         id.New("aud"),
		ActorType:  "user",
		Action:     audit.ActionProxyRequest,
		TargetType: "llm_request",
		TargetID:   model,
		Result:     result,
		RequestID:  middleware.GetRequestID(r.Context()),
	}
	if authCtx != nil {
		event.TenantID = authCtx.TenantID
		event.ActorID = authCtx.UserID
	}
	if event.ActorID == "" {
		event.ActorID = "anonymous"
	}
	logger.Log(event)
}

// CapturePayload submits raw request/response bodies to the async capture
// recorder. It never blocks proxy success; overload and backend failures are
// handled inside the recorder.
func CapturePayload(r *http.Request, svcs *Services, provider *providers.Provider, model, status, errorClass string, streaming bool, requestBody, responseBody []byte, responseFormat string, startedAt time.Time, completedAt *time.Time, opts ...CaptureOption) {
	fields := logging.Fields{
		Model:      model,
		ErrorClass: errorClass,
		Streaming:  streaming,
	}
	if provider != nil {
		fields.Provider = string(provider.Type)
	}
	middleware.AddRequestLogFields(r.Context(), fields)

	if svcs == nil || svcs.Capture == nil {
		return
	}
	authCtx := middleware.GetAuthContext(r.Context())
	conversationID := capture.ExtractConversationIDFromHeaders(r.Header)

	var expiresAt *time.Time
	if svcs.CaptureConfig.RetentionDays > 0 {
		expires := startedAt.Add(time.Duration(svcs.CaptureConfig.RetentionDays) * 24 * time.Hour)
		expiresAt = &expires
	}
	if completedAt == nil {
		now := time.Now()
		completedAt = &now
	}
	if responseFormat == "" {
		responseFormat = capture.ResponseFormatJSON
	}
	providerType := ""
	if provider != nil {
		providerType = string(provider.Type)
	}

	payload := &capture.Payload{
		ID:              id.New("cap"),
		RequestID:       middleware.GetRequestID(r.Context()),
		ConversationID:  conversationID,
		Provider:        providerType,
		Model:           model,
		Status:          status,
		ErrorClass:      errorClass,
		Streaming:       streaming,
		RequestBody:     requestBody,
		ResponseBody:    responseBody,
		ResponseFormat:  responseFormat,
		CreatedAt:       startedAt,
		CompletedAt:     completedAt,
		ExpiresAt:       expiresAt,
		MaxPayloadBytes: svcs.CaptureConfig.MaxPayloadBytes,
	}
	if authCtx != nil {
		payload.TenantID = authCtx.TenantID
		payload.UserID = authCtx.UserID
		payload.KeyID = authCtx.KeyID
	}

	options := captureOptions{payload: payload}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.stream != nil && options.stream.Truncated() {
		payload.ResponseTruncated = true
		// The SSE transcript was cut at its ceiling mid-relay; the payload row
		// keeps the partial transcript, and this marker records that loss so
		// the session can be audited for replayability.
		svcs.Capture.RecordGap(capture.Gap{
			ConversationID: payload.ConversationID,
			TenantID:       payload.TenantID,
			UserID:         payload.UserID,
			RequestID:      payload.RequestID,
			Provider:       providerType,
			Model:          model,
			Streaming:      true,
			Reason:         capture.GapReasonResponseTruncated,
			Detail:         fmt.Sprintf("sse transcript capped: ceiling=%d captured_bytes=%d", options.stream.Limit(), len(options.stream.Bytes())),
		})
	}
	svcs.Capture.Submit(payload)
}

// captureOptions carries CapturePayload tweaks. Simple flags mutate the
// payload directly; stream capture additionally owns gap-marker emission.
type captureOptions struct {
	payload *capture.Payload
	stream  *StreamCapture
}

// CaptureOption customizes a captured payload. Options never clear
// truncation flags that earlier options or the recorder already set.
type CaptureOption func(*captureOptions)

// WithResponseTruncated marks the captured response as partial.
func WithResponseTruncated(v bool) CaptureOption {
	return func(o *captureOptions) {
		if v {
			o.payload.ResponseTruncated = true
		}
	}
}

// WithStreamCapture attaches a relay-side SSE transcript. When the transcript
// hit its ceiling, the payload is flagged response-truncated and a capture
// gap marker is submitted alongside it.
func WithStreamCapture(sc *StreamCapture) CaptureOption {
	return func(o *captureOptions) {
		o.stream = sc
	}
}

// ResolveQuotaScope picks the most specific quota scope from the auth context.
// Prefer ResolveQuotaScopes for enforcement paths so tenant/user/key policies
// are all applied.
func ResolveQuotaScope(r *http.Request) (quota.ScopeType, string) {
	scopes := ResolveQuotaScopes(r)
	if len(scopes) == 0 {
		return quota.ScopeTenant, ""
	}
	return scopes[0].Type, scopes[0].ID
}

// QuotaScope identifies a single quota scope applicable to a request.
type QuotaScope struct {
	Type quota.ScopeType
	ID   string
}

// ResolveQuotaScopes returns every quota scope that should constrain a request.
// Key-specific policies are checked first, followed by user and tenant limits.
func ResolveQuotaScopes(r *http.Request) []QuotaScope {
	authCtx := middleware.GetAuthContext(r.Context())
	if authCtx == nil {
		return nil
	}
	scopes := make([]QuotaScope, 0, 3)
	seen := map[string]struct{}{}
	add := func(scopeType quota.ScopeType, scopeID string) {
		if scopeID == "" {
			return
		}
		key := string(scopeType) + ":" + scopeID
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		scopes = append(scopes, QuotaScope{Type: scopeType, ID: scopeID})
	}
	add(quota.ScopeKey, authCtx.KeyID)
	if authCtx.UserID != "" {
		add(quota.ScopeUser, authCtx.UserID)
	}
	if authCtx.TenantID != "" {
		add(quota.ScopeTenant, authCtx.TenantID)
	}
	return scopes
}

// CheckQuota checks quota and reserves capacity for a request.
// Returns the reservation (which must be completed or rolled back) or nil
// if no quota enforcer is configured or quotas are disabled for this provider.
func CheckQuota(r *http.Request, svcs *Services, provider *providers.Provider, model string, estimatedInputTokens, estimatedOutputTokens int64) (*quota.Reservation, error) {
	if svcs == nil || svcs.Quota == nil {
		return nil, nil
	}
	// The selected provider determines the exemption, including model aliases.
	// A nil reservation also skips settlement; usage recording stays independent.
	if svcs.CodexQuotaDisabled && provider != nil && provider.Type == providers.ProviderOpenAICodexResponses {
		return nil, nil
	}
	scopes := ResolveQuotaScopes(r)
	if len(scopes) == 0 {
		return nil, nil
	}
	if estimatedOutputTokens <= 0 {
		estimatedOutputTokens = effectiveMinLimit(svcs.Quota, scopes, quota.LimitMaxOutputTokens)
	}
	deferredSubscriptionSpend := provider != nil && provider.Billing.Mode == "subscription"
	if deferredSubscriptionSpend {
		if _, _, _, ok := ProviderCostTableForModel(svcs, provider, model); !ok {
			return nil, fmt.Errorf("subscription pricing unavailable for provider account %s", provider.ID)
		}
	}
	estimatedSpend := EstimateSpendMillicents(svcs, provider, model, estimatedInputTokens, estimatedOutputTokens)
	reservations := make([]*quota.Reservation, 0, len(scopes))
	for _, scope := range scopes {
		var resv *quota.Reservation
		var err error
		if deferredSubscriptionSpend {
			resv, err = svcs.Quota.CheckAndReserveDeferredSpend(r.Context(), scope.Type, scope.ID, model, estimatedInputTokens)
		} else {
			resv, err = svcs.Quota.CheckAndReserve(r.Context(), scope.Type, scope.ID, model, estimatedInputTokens, estimatedSpend)
		}
		if err != nil {
			for _, existing := range reservations {
				_ = svcs.Quota.Rollback(r.Context(), existing)
			}
			return nil, err
		}
		reservations = append(reservations, resv)
	}
	if len(reservations) == 1 {
		return reservations[0], nil
	}
	return &quota.Reservation{
		ID:                       middleware.GetRequestID(r.Context()),
		Model:                    model,
		EstimatedTokens:          estimatedInputTokens,
		EstimatedSpendMillicents: estimatedSpend,
		Children:                 reservations,
		CreatedAt:                time.Now(),
	}, nil
}

func effectiveMinLimit(enforcer *quota.Enforcer, scopes []QuotaScope, limitType quota.LimitType) int64 {
	var effective int64
	for _, scope := range scopes {
		limit := enforcer.EffectiveLimit(scope.Type, scope.ID, limitType)
		if limit <= 0 {
			continue
		}
		if effective == 0 || limit < effective {
			effective = limit
		}
	}
	return effective
}

// CompleteQuota releases a quota reservation after a request completes.
func CompleteQuota(r *http.Request, svcs *Services, resv *quota.Reservation, actualTokens int64) {
	if svcs == nil || svcs.Quota == nil || resv == nil {
		return
	}
	ctx, cancel := quotaSettlementContext(r)
	defer cancel()
	if err := svcs.Quota.Complete(ctx, resv, actualTokens); err != nil {
		logging.RequestError(middleware.GetRequestLogger(r.Context()), "quota.complete_error", err, logging.Fields{})
	}
}

// CompleteQuotaWithUsage releases a quota reservation and reconciles spend with
// the actual model usage observed from the upstream response.
func CompleteQuotaWithUsage(r *http.Request, svcs *Services, resv *quota.Reservation, provider *providers.Provider, model string, inputTokens, outputTokens int64) {
	CompleteQuotaWithTokenUsage(r, svcs, resv, provider, model, usage.TokenUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	})
}

// CompleteQuotaWithTokenUsage releases a quota reservation and reconciles
// spend with provider-specific cache token classes.
func CompleteQuotaWithTokenUsage(r *http.Request, svcs *Services, resv *quota.Reservation, provider *providers.Provider, model string, tokenUsage usage.TokenUsage) {
	if svcs == nil || svcs.Quota == nil || resv == nil {
		return
	}
	actualSpend := EstimateSpendMillicentsWithCache(svcs, provider, model, tokenUsage)
	actualTokens := tokenUsage.TotalTokens
	if actualTokens == 0 {
		outputTokens := tokenUsage.OutputTokens
		if outputTokens < tokenUsage.ReasoningTokens {
			outputTokens = tokenUsage.ReasoningTokens
		}
		actualTokens = tokenUsage.InputTokens + outputTokens + tokenUsage.CacheReadTokens + tokenUsage.CacheWriteTokens
	}
	ctx, cancel := quotaSettlementContext(r)
	defer cancel()
	if err := svcs.Quota.Complete(ctx, resv, actualTokens, actualSpend); err != nil {
		logging.RequestError(middleware.GetRequestLogger(r.Context()), "quota.complete_error", err, logging.Fields{})
	}
}

// RollbackQuota releases a quota reservation without recording usage.
func RollbackQuota(r *http.Request, svcs *Services, resv *quota.Reservation) {
	if svcs == nil || svcs.Quota == nil || resv == nil {
		return
	}
	ctx, cancel := quotaSettlementContext(r)
	defer cancel()
	if err := svcs.Quota.Rollback(ctx, resv); err != nil {
		logging.RequestError(middleware.GetRequestLogger(r.Context()), "quota.rollback_error", err, logging.Fields{})
	}
}

// quotaSettlementContext lets quota slots and spend reservations settle after
// the client disconnects. The HTTP request context is cancelled at exactly the
// moment streaming handlers most need to release their Redis reservations, so
// settlement must not inherit that cancellation. Keep it bounded so a Redis
// outage cannot hold a request goroutine indefinitely.
func quotaSettlementContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
}

// EstimateSpendMillicents returns a fixed-point cost estimate suitable for
// Redis quota counters. It rounds up to avoid under-reserving fractional cents.
func EstimateSpendMillicents(svcs *Services, provider *providers.Provider, model string, inputTokens, outputTokens int64) int64 {
	return EstimateSpendMillicentsWithCache(svcs, provider, model, usage.TokenUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	})
}

// EstimateSpendMillicentsWithCache returns fixed-point cost for all billable
// token classes. It rounds up to avoid under-reserving fractional cents.
func EstimateSpendMillicentsWithCache(svcs *Services, provider *providers.Provider, model string, tokenUsage usage.TokenUsage) int64 {
	var table *usage.KnownCostTable
	if svcs != nil {
		table = svcs.CostTable
	}
	if table == nil {
		table = usage.NewCostTable()
	}
	cost, _, basis, explicit := ProviderCostTableForModel(svcs, provider, model)
	if provider == nil || provider.Billing.Mode == "" || provider.Billing.Mode == "default" {
		cost = table.GetCost(model)
	} else if !explicit {
		return 0
	}
	cents := 0.0
	if basis == "subscription_rate_card" {
		totalTokens := tokenUsage.TotalTokens
		if totalTokens == 0 {
			outputTokens := tokenUsage.OutputTokens
			if outputTokens < tokenUsage.ReasoningTokens {
				outputTokens = tokenUsage.ReasoningTokens
			}
			totalTokens = tokenUsage.InputTokens + outputTokens + tokenUsage.CacheReadTokens + tokenUsage.CacheWriteTokens
		}
		cents = float64(totalTokens) / 1_000_000 * cost.InputPricePerMillion * 100
	} else {
		cents = usage.CalculateCost(
			tokenUsage.InputTokens,
			tokenUsage.OutputTokens,
			tokenUsage.CacheReadTokens,
			tokenUsage.CacheWriteTokens,
			cost,
		)
	}
	if cents <= 0 {
		return 0
	}
	return int64(math.Ceil(cents * float64(quota.SpendCounterScale)))
}

// ProviderCostTable returns static provider-account overrides. Dynamic
// subscription rates require ProviderCostTableForModel and a Services value.
func ProviderCostTable(provider *providers.Provider) (usage.CostTable, bool) {
	cost, _, _, ok := ProviderCostTableForModel(nil, provider, "")
	return cost, ok
}

// ProviderCostTableForModel resolves the daily subscription rate for a model.
// Subscription prices are blended total-token rates, so the same value applies
// to input, output, and cache token classes.
func ProviderCostTableForModel(svcs *Services, provider *providers.Provider, model string) (usage.CostTable, int64, string, bool) {
	if provider == nil {
		return usage.CostTable{}, 0, "", false
	}
	switch provider.Billing.Mode {
	case "subscription":
		if provider.Billing.AllocationMethod == subscriptioncost.ObservedDelta {
			// Observations settle cost later, inside the shared paid-period cap.
			// An inferred historical token rate is never a request price.
			return usage.CostTable{}, 0, subscriptioncost.PendingBasis, true
		}
		if svcs == nil || svcs.SubscriptionRates == nil {
			return usage.CostTable{}, 0, "", false
		}
		card, ok := svcs.SubscriptionRates.Resolve(provider.ID, model)
		if !ok || card.USDPerMillionTokens <= 0 {
			return usage.CostTable{}, 0, "", false
		}
		price := card.USDPerMillionTokens
		return usage.CostTable{
			InputPricePerMillion:      price,
			OutputPricePerMillion:     price,
			CacheReadPricePerMillion:  price,
			CacheWritePricePerMillion: price,
		}, card.ID, "subscription_rate_card", true
	case "metered":
		return usage.CostTable{
			InputPricePerMillion:      provider.Billing.InputPricePerMillion,
			OutputPricePerMillion:     provider.Billing.OutputPricePerMillion,
			CacheReadPricePerMillion:  provider.Billing.CacheReadPricePerMillion,
			CacheWritePricePerMillion: provider.Billing.CacheWritePricePerMillion,
		}, 0, "metered", true
	default:
		return usage.CostTable{}, 0, "", false
	}
}
