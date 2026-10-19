package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/usage"
)

func ClassifyAgentEvent(body []byte) hooks.ActorKind {
	if isToolOnlyOpenAI(body) || isToolOnlyAnthropic(body) {
		return hooks.ActorTool
	}
	return hooks.ActorUser
}

func EmitUserPreSendHook(ctx context.Context, r *http.Request, svcs *Services, provider *providers.Provider, model string, streaming bool, requestBody []byte, startedAt time.Time) (*hooks.Decision, error) {
	decision, _, err := ApplyUserPreSendHook(ctx, r, svcs, provider, model, streaming, requestBody, startedAt)
	return decision, err
}

// ApplyUserPreSendHook runs the pre-send hooks and returns the request body to
// forward, which differs from the one passed in when a hook with mutation
// rights rewrote it.
//
// The returned body is what gets sent upstream *and* what capture records, so
// the stored payload reflects what the model actually saw rather than what the
// client originally submitted. A rewrite that does not line up with the request
// is discarded and logged rather than applied, because a partial application
// would silently drop turns or strip attachments.
func ApplyUserPreSendHook(ctx context.Context, r *http.Request, svcs *Services, provider *providers.Provider, model string, streaming bool, requestBody []byte, startedAt time.Time) (*hooks.Decision, []byte, error) {
	if svcs == nil || svcs.Hooks == nil || ClassifyAgentEvent(requestBody) != hooks.ActorUser {
		return nil, requestBody, nil
	}
	ev := baseEvent(r, provider, model, hooks.EventUserMessagePreSend, requestBody, startedAt)
	ev.Stats.Streaming = streaming
	ev.Content = extractHookContent(requestBody)

	decision, err := svcs.Hooks.EmitPreSend(ctx, ev)
	if err != nil || decision == nil || decision.Abort || !decision.Mutates() {
		return decision, requestBody, err
	}

	rewritten, rewriteErr := applyHookRewrite(requestBody, decision)
	if rewriteErr != nil {
		logging.RequestError(middleware.GetRequestLogger(r.Context()), "hook.rewrite_rejected", rewriteErr, logging.Fields{
			Model: model,
		})
		return decision, requestBody, nil
	}
	return decision, rewritten, nil
}

// WillBufferForHooks reports whether a mutating post-send hook is registered
// for this request. Streaming has to be given up when one is: a token already
// flushed to the client cannot be recalled, so there is nothing left to rewrite
// by the time the hook would run.
func WillBufferForHooks(r *http.Request, svcs *Services, provider *providers.Provider, model string, requestBody []byte) bool {
	if svcs == nil || svcs.Hooks == nil || ClassifyAgentEvent(requestBody) != hooks.ActorUser {
		return false
	}
	ev := baseEvent(r, provider, model, hooks.EventUserMessagePostSend, requestBody, time.Now())
	return svcs.Hooks.WillMutateOutput(ev)
}

// ApplyPostSendHook runs the post-send hooks synchronously and returns the
// output to deliver, which a mutating hook may have replaced. Callers that
// stream must not use it; they call EmitUserPostSendHook instead, which is
// asynchronous and cannot alter what has already been sent.
func ApplyPostSendHook(ctx context.Context, r *http.Request, svcs *Services, provider *providers.Provider, model, status, errorClass string, httpStatus int, requestBody []byte, output string, tokenUsage usage.TokenUsage, estimated bool, startedAt, completedAt time.Time) (string, *hooks.Decision) {
	if svcs == nil || svcs.Hooks == nil || ClassifyAgentEvent(requestBody) != hooks.ActorUser {
		return output, nil
	}
	ev := postSendEvent(r, svcs, provider, model, status, errorClass, httpStatus, false, requestBody, tokenUsage, estimated, startedAt, completedAt)
	ev.Content = extractHookContent(requestBody)
	if ev.Content == nil {
		ev.Content = &hooks.EventContent{}
	}
	ev.Content.Output = output

	decision, err := svcs.Hooks.EmitPreSend(ctx, ev)
	if err != nil || decision == nil {
		return output, decision
	}
	if decision.RewrittenOutput != "" {
		return decision.RewrittenOutput, decision
	}
	return output, decision
}

func EmitUserPostSendHook(r *http.Request, svcs *Services, provider *providers.Provider, model, status, errorClass string, httpStatus int, streaming bool, requestBody []byte, tokenUsage usage.TokenUsage, estimated bool, startedAt time.Time, completedAt time.Time) {
	if svcs == nil || svcs.Hooks == nil || ClassifyAgentEvent(requestBody) != hooks.ActorUser {
		return
	}
	ev := postSendEvent(r, svcs, provider, model, status, errorClass, httpStatus,
		streaming, requestBody, tokenUsage, estimated, startedAt, completedAt)
	if hooks.HasOutputReview(r.Context()) {
		hooks.RecordOutputReviewEvent(r.Context(), ev)
		return
	}
	svcs.Hooks.EmitAsync(ev)
}

// postSendEvent assembles the completed-turn event, including the cost
// arithmetic, so the async and synchronous post-send paths report identically.
func postSendEvent(r *http.Request, svcs *Services, provider *providers.Provider, model, status, errorClass string, httpStatus int, streaming bool, requestBody []byte, tokenUsage usage.TokenUsage, estimated bool, startedAt, completedAt time.Time) *hooks.Event {
	totalTokens := tokenUsage.TotalTokens
	if totalTokens == 0 {
		totalTokens = tokenUsage.InputTokens + tokenUsage.OutputTokens + tokenUsage.CacheReadTokens + tokenUsage.CacheWriteTokens
	}
	costCents := 0.0
	if cost, _, basis, explicit := ProviderCostTableForModel(svcs, provider, model); explicit {
		if basis == "subscription_rate_card" {
			costCents = float64(totalTokens) / 1_000_000 * cost.InputPricePerMillion * 100
		} else {
			costCents = usage.CalculateCost(tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.CacheReadTokens, tokenUsage.CacheWriteTokens, cost)
		}
	} else if provider != nil && provider.Billing.Mode != "subscription" && svcs.CostTable != nil {
		costCents = usage.CalculateCost(tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.CacheReadTokens, tokenUsage.CacheWriteTokens, svcs.CostTable.GetCost(model))
	}
	ev := baseEvent(r, provider, model, hooks.EventUserMessagePostSend, requestBody, startedAt)
	ev.Stats.InputTokens = tokenUsage.InputTokens
	ev.Stats.OutputTokens = tokenUsage.OutputTokens
	ev.Stats.TotalTokens = totalTokens
	ev.Stats.CostCents = costCents
	ev.Stats.Estimated = estimated
	ev.Stats.Streaming = streaming
	ev.Stats.CompletedAt = &completedAt
	ev.Stats.DurationMs = completedAt.Sub(startedAt).Milliseconds()
	ev.Result.Status = status
	ev.Result.ErrorClass = errorClass
	ev.Result.HTTPStatus = httpStatus
	return ev
}

func EmitSessionUploadedHook(r *http.Request, manager *hooks.Manager, conversationID, sessionFile, project, projectID, model string, messageCount int, totalTokens int64, totalCostCents float64, startedAt, endedAt *time.Time) {
	if manager == nil {
		return
	}
	ev := &hooks.Event{
		Type:           hooks.EventSessionUploaded,
		RequestID:      middleware.GetRequestID(r.Context()),
		ConversationID: conversationID,
		SessionFile:    sessionFile,
		Project:        project,
		ProjectID:      projectID,
		Model:          model,
		Actor:          hooks.EventActor{Kind: hooks.ActorUser},
		Stats: hooks.EventStats{
			MessageCount: messageCount,
			TotalTokens:  totalTokens,
			CostCents:    totalCostCents,
			StartedAt:    startedAt,
			CompletedAt:  endedAt,
		},
		Result: hooks.EventResult{Status: "success", HTTPStatus: http.StatusCreated},
	}
	if startedAt != nil && endedAt != nil {
		ev.Stats.DurationMs = endedAt.Sub(*startedAt).Milliseconds()
	}
	if authCtx := middleware.GetAuthContext(r.Context()); authCtx != nil {
		ev.TenantID = authCtx.TenantID
		ev.UserID = authCtx.UserID
		ev.KeyID = authCtx.KeyID
	}
	manager.EmitAsync(ev)
}

func baseEvent(r *http.Request, provider *providers.Provider, model string, typ hooks.EventType, requestBody []byte, startedAt time.Time) *hooks.Event {
	providerType := ""
	if provider != nil {
		providerType = string(provider.Type)
	}
	ev := &hooks.Event{
		Type:           typ,
		RequestID:      middleware.GetRequestID(r.Context()),
		ConversationID: capture.ExtractConversationID(r.Header, requestBody),
		Provider:       providerType,
		Model:          model,
		Route:          r.URL.Path,
		Actor:          hooks.EventActor{Kind: hooks.ActorUser},
		Stats: hooks.EventStats{
			StartedAt: &startedAt,
		},
	}
	if authCtx := middleware.GetAuthContext(r.Context()); authCtx != nil {
		ev.TenantID = authCtx.TenantID
		ev.UserID = authCtx.UserID
		ev.KeyID = authCtx.KeyID
	}
	return ev
}

func isToolOnlyOpenAI(body []byte) bool {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return false
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role := req.Messages[i].Role
		if role == "system" || role == "developer" {
			continue
		}
		return role == "tool"
	}
	return false
}

func isToolOnlyAnthropic(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return false
	}
	items, ok := last.Content.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != "tool_result" {
			return false
		}
	}
	return true
}

// HookAnnotationsHeader carries a hook's annotations back to the caller. The
// web console turns it into a visible badge on the message, so a user can tell
// that a guardrail acted rather than silently receiving altered content. CLI
// clients ignore it.
const HookAnnotationsHeader = "X-Piproxy-Hook-Annotations"

// SetHookAnnotations publishes a decision's annotations on the response.
func SetHookAnnotations(w http.ResponseWriter, decision *hooks.Decision) {
	if w == nil || decision == nil || len(decision.Annotations) == 0 {
		return
	}
	encoded, err := json.Marshal(decision.Annotations)
	if err != nil {
		return
	}
	w.Header().Set(HookAnnotationsHeader, string(encoded))
}
