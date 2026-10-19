// Package openai provides OpenAI-compatible API handlers.
package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/errors"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	quotapkg "github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/stream"
	"github.com/earlgrey/piproxy/internal/upstream"
	usagepkg "github.com/earlgrey/piproxy/internal/usage"
)

// ChatHandler handles POST /openai/v1/chat/completions requests.
type ChatHandler struct {
	upstream *upstream.Client
	rewriter *rewrite.RequestRewriter
	registry *providers.Registry
	services dataplane.Services
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(upstreamClient *upstream.Client, rewriter *rewrite.RequestRewriter, registry *providers.Registry, services dataplane.Services) *ChatHandler {
	return &ChatHandler{
		upstream: upstreamClient,
		rewriter: rewriter,
		registry: registry,
		services: services,
	}
}

// Handle processes a chat completion request with structured logging.
func (h *ChatHandler) Handle(w http.ResponseWriter, r *http.Request) {
	log := middleware.GetRequestLogger(r.Context())
	timing := logging.StartTiming()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logging.RequestError(log, "openai.read_body", err, logging.Fields{})
		errors.WriteErrorJSON(w, errors.ErrorBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var chatReq ChatCompletionRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		logging.RequestEvent(log, "openai.parse_error", logging.Fields{ErrorClass: "bad_request"})
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, "", "error", "bad_request", false, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorBadRequest, fmt.Sprintf("invalid request: %s", err.Error()))
		return
	}

	logging.RequestEvent(log, "openai.request", logging.Fields{
		Model:     chatReq.Model,
		Streaming: chatReq.Stream,
	})

	// Model guards run before routing and quota: a guarded model is denied with
	// the operator's message (e.g. "use deepseek-v4-flash instead") instead of
	// a provider_unavailable or upstream "model not found" error, and never
	// reserves spend.
	if violation := dataplane.CheckModelGuard(r, &h.services, chatReq.Model); violation != nil {
		h.denyModelGuard(w, r, violation, nil, chatReq.Model, chatReq.Stream, body, timing, log)
		return
	}

	requestedModel := chatReq.Model
	provider, routedModel, ok := h.resolveProvider(r, chatReq.Model)
	if !ok {
		logging.RequestEvent(log, "openai.provider_unavailable", logging.Fields{Model: chatReq.Model})
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, chatReq.Model, "error", "provider_unavailable", chatReq.Stream, body, []byte("OpenAI provider not configured"), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorProviderUnavailable, "OpenAI/DeepSeek provider not configured")
		return
	}
	if routedModel != "" && routedModel != chatReq.Model {
		body = dataplane.RewriteModel(body, routedModel)
		chatReq.Model = routedModel
	}
	// A synthetic alias may route to a guarded target; check the resolved
	// upstream model too so renaming cannot reach it either.
	if violation := dataplane.CheckModelGuard(r, &h.services, chatReq.Model); violation != nil {
		h.denyModelGuard(w, r, violation, provider, chatReq.Model, chatReq.Stream, body, timing, log)
		return
	}
	if err := dataplane.CheckModelRouteAccess(r, &h.services, requestedModel, chatReq.Model); err != nil {
		logging.RequestEvent(log, "openai.model_denied", logging.Fields{
			Model:      chatReq.Model,
			ErrorClass: "model_denied",
		})
		dataplane.RecordAudit(r, h.services.Audit, provider, chatReq.Model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, chatReq.Model, "error", "model_denied", chatReq.Stream, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorForbidden, err.Error())
		return
	}

	// Estimate input tokens for quota check.
	estimatedTokens := usagepkg.EstimateRequestTokens(body)
	resv, err := dataplane.CheckQuota(r, &h.services, provider, chatReq.Model, estimatedTokens, chatReq.EstimatedOutputTokens())
	if err != nil {
		logging.RequestEvent(log, "openai.quota_denied", logging.Fields{
			Model:      chatReq.Model,
			ErrorClass: "quota_exceeded",
		})
		dataplane.RecordAudit(r, h.services.Audit, provider, chatReq.Model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, chatReq.Model, "error", "quota_exceeded", chatReq.Stream, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorQuotaExceeded, err.Error())
		return
	}

	// Register a consume-once rollback closure so the Recover middleware can
	// release the reservation if anything between here and completion panics.
	// The existing inline RollbackQuota/CompleteQuota* calls and this panic path
	// are mutually safe: whichever fires first wins, the rest are no-ops.
	r = r.WithContext(middleware.WithRollback(r.Context(), func() {
		dataplane.RollbackQuota(r, &h.services, resv)
	}))

	result, err := h.rewriter.Rewrite(r, provider)
	if err != nil {
		logging.RequestEvent(log, "openai.rewrite_error", logging.Fields{
			Model:      chatReq.Model,
			ErrorClass: "bad_request",
		})
		dataplane.RollbackQuota(r, &h.services, resv)
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, chatReq.Model, "error", "bad_request", chatReq.Stream, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorUnprocessable, err.Error())
		dataplane.RecordAudit(r, h.services.Audit, provider, chatReq.Model, "error")
		return
	}

	hookDecision, hookedBody, hookErr := dataplane.ApplyUserPreSendHook(r.Context(), r, &h.services, provider, chatReq.Model, chatReq.Stream, body, timing.StartedAt())
	if hookErr != nil || (hookDecision != nil && hookDecision.Abort) {
		dataplane.RollbackQuota(r, &h.services, resv)
		message := "request blocked by hook"
		errorType := errors.ErrorForbidden
		errorClass := "hook_blocked"
		if hookDecision != nil {
			if hookDecision.Message != "" {
				message = hookDecision.Message
			}
			if hookDecision.ErrorType != "" {
				errorClass = hookDecision.ErrorType
			}
		} else if hookErr != nil {
			message = "pre-send hook failed"
			errorType = errors.ErrorUpstreamFailure
			errorClass = "hook_failed"
		}
		now := time.Now()
		dataplane.RecordUsage(r, h.services.Usage, provider, chatReq.Model, "error", errorClass, 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, chatReq.Model, "denied")
		dataplane.CapturePayload(r, &h.services, provider, chatReq.Model, "error", errorClass, chatReq.Stream, body, []byte(message), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errorType, message)
		return
	}
	// A hook with mutation rights may have rewritten the request. Everything
	// downstream — the upstream call and the captured payload — uses the
	// rewritten body, so what is stored is what the model actually saw.
	body = hookedBody
	dataplane.SetHookAnnotations(w, hookDecision)

	if err := dataplane.ApplyCredential(r.Context(), result.Headers, provider, h.services.Credentials); err != nil {
		logging.RequestError(log, "openai.credential_error", err, logging.Fields{Model: chatReq.Model})
		dataplane.RollbackQuota(r, &h.services, resv)
		errors.WriteErrorJSON(w, errors.ErrorProviderUnavailable, "upstream credential unavailable")
		dataplane.RecordUsage(r, h.services.Usage, provider, chatReq.Model, "error", "credential_unavailable", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, chatReq.Model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, chatReq.Model, "error", "credential_unavailable", chatReq.Stream, body, []byte("upstream credential unavailable"), capture.ResponseFormatText, timing.StartedAt(), &now)
		return
	}

	bodyReader := bytes.NewReader(body)

	if chatReq.Stream {
		h.handleStreaming(w, r, result, bodyReader, body, provider, chatReq.Model, log, timing, resv)
	} else {
		h.handleNonStreaming(w, r, result, bodyReader, body, provider, chatReq.Model, log, timing, resv)
	}
}

// denyModelGuard writes a model-guard violation (modelguard.Rules) and
// records the same audit/capture side effects as the model_denied path.
// provider is nil when the guard fires before provider resolution. No usage
// row is written, mirroring model_denied: the request never reached upstream.
func (h *ChatHandler) denyModelGuard(w http.ResponseWriter, r *http.Request, violation *modelguard.Violation, provider *providers.Provider, model string, stream bool, body []byte, timing logging.Timing, log zerolog.Logger) {
	logging.RequestEvent(log, "openai.model_guarded", logging.Fields{
		Model:      model,
		ErrorClass: "model_guarded",
	})
	dataplane.RecordAudit(r, h.services.Audit, provider, model, "denied")
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, "error", "model_guarded", stream, body, []byte(violation.Message), capture.ResponseFormatText, timing.StartedAt(), &now)
	errors.WriteErrorJSON(w, violation.ErrorType, violation.Message)
}

func (h *ChatHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, result *rewrite.Result, body io.Reader, requestBody []byte, provider *providers.Provider, model string, log zerolog.Logger, timing logging.Timing, resv *quotapkg.Reservation) {
	upstreamStart := time.Now()
	upstreamResp, err := h.upstream.DoForward(r.Context(), result.UpstreamMethod, result.UpstreamURL, result.Headers, body)
	upstreamDuration := time.Since(upstreamStart).Milliseconds()

	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "openai.client_closed", err, logging.Fields{
				Model:          model,
				UpstreamURL:    result.UpstreamURL,
				DurationMs:     timing.Elapsed(),
				UpstreamStatus: dataplane.StatusClientClosedRequest,
				ErrorClass:     dataplane.ErrorClassClientClosed,
			})
			dataplane.WriteClientClosed(w)
			dataplane.RollbackQuota(r, &h.services, resv)
			dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, 0, 0, false, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, false, requestBody, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
			return
		}
		logging.RequestError(log, "openai.upstream_error", err, logging.Fields{
			Model:          model,
			UpstreamURL:    result.UpstreamURL,
			DurationMs:     timing.Elapsed(),
			UpstreamStatus: 0,
		})
		errors.WriteErrorJSON(w, errors.ErrorUpstreamFailure, err.Error())
		dataplane.RollbackQuota(r, &h.services, resv)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "upstream_failure", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "upstream_failure", false, requestBody, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		return
	}
	defer upstreamResp.Body.Close()

	middleware.AddRequestLogFields(r.Context(), logging.Fields{
		Provider:       string(provider.Type),
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
	})
	logging.RequestEvent(log, "openai.upstream_response", logging.Fields{
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
		DurationMs:     upstreamDuration,
	})

	respBody, err := dataplane.ReadUpstreamBody(upstreamResp.Body, dataplane.DefaultMaxUpstreamResponseBytes)
	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "openai.client_closed", err, logging.Fields{
				Model:          model,
				UpstreamURL:    result.UpstreamURL,
				UpstreamStatus: dataplane.StatusClientClosedRequest,
				DurationMs:     timing.Elapsed(),
				ErrorClass:     dataplane.ErrorClassClientClosed,
			})
			dataplane.WriteClientClosed(w)
			dataplane.RollbackQuota(r, &h.services, resv)
			dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, 0, 0, false, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, false, requestBody, []byte("client closed request"), capture.ResponseFormatText, timing.StartedAt(), &now)
			return
		}
		logging.RequestError(log, "openai.read_upstream_body", err, logging.Fields{
			Model:          model,
			UpstreamURL:    result.UpstreamURL,
			UpstreamStatus: upstreamResp.StatusCode,
			DurationMs:     timing.Elapsed(),
		})
		errors.WriteErrorJSON(w, errors.ErrorUpstreamFailure, "failed to read upstream response")
		dataplane.RollbackQuota(r, &h.services, resv)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "upstream_failure", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "upstream_failure", false, requestBody, []byte("failed to read upstream response"), capture.ResponseFormatText, timing.StartedAt(), &now)
		return
	}

	if isOverflow, nrm := rewrite.ContextOverflowNormalized(provider.Type, respBody); isOverflow {
		logging.RequestEvent(log, "openai.context_overflow", logging.Fields{
			Model:      model,
			ErrorClass: nrm,
			DurationMs: timing.Elapsed(),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		dataplane.RollbackQuota(r, &h.services, resv)
		_, _ = w.Write([]byte(`{"error":{"type":"context_length_exceeded","message":"Context length exceeded"}}`))
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", nrm, 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", nrm, false, requestBody, respBody, capture.ResponseFormatJSON, timing.StartedAt(), &now)
		return
	}

	tokenUsage, _ := usagepkg.ExtractOpenAIUsageDetails(respBody)
	estimated := false
	if tokenUsage.TotalTokens == 0 {
		tokenUsage.InputTokens = usagepkg.EstimateRequestTokens(requestBody)
		tokenUsage.OutputTokens = usagepkg.EstimateTextTokens(string(respBody))
		tokenUsage.TotalTokens = tokenUsage.InputTokens + tokenUsage.OutputTokens
		estimated = true
	}
	logging.RequestEvent(log, "openai.complete", logging.Fields{
		Model:          model,
		UpstreamStatus: upstreamResp.StatusCode,
		Tokens:         tokenUsage.TotalTokens,
		DurationMs:     timing.Elapsed(),
		UpstreamURL:    result.UpstreamURL,
	})
	status := "success"
	resultStatus := "success"
	errorClass := ""
	if upstreamResp.StatusCode >= 400 {
		status = "error"
		resultStatus = "error"
		errorClass = "upstream_error"
	}
	dataplane.RecordUsageWithCache(r, h.services.Usage, provider, model, status, errorClass, tokenUsage, estimated, timing.StartedAt())
	dataplane.RecordAudit(r, h.services.Audit, provider, model, resultStatus)
	dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, status, errorClass, false, requestBody, respBody, capture.ResponseFormatJSON, timing.StartedAt(), &now)
	dataplane.EmitUserPostSendHook(r, &h.services, provider, model, status, errorClass, upstreamResp.StatusCode, false, requestBody, tokenUsage, estimated, timing.StartedAt(), now)

	forwardResponseHeaders(w.Header(), upstreamResp.Header)
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = w.Write(respBody)
}

func (h *ChatHandler) handleStreaming(w http.ResponseWriter, r *http.Request, result *rewrite.Result, body io.Reader, requestBody []byte, provider *providers.Provider, model string, log zerolog.Logger, timing logging.Timing, resv *quotapkg.Reservation) {
	streamCtx, cancel := stream.NewStreamTimeout(r.Context(), provider.Timeout)
	defer cancel()

	upstreamStart := time.Now()
	upstreamResp, err := h.upstream.DoForwardAndStream(streamCtx, result.UpstreamMethod, result.UpstreamURL, result.Headers, body)
	upstreamDuration := time.Since(upstreamStart).Milliseconds()

	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "openai.stream_client_closed", err, logging.Fields{
				Model:          model,
				UpstreamURL:    result.UpstreamURL,
				UpstreamStatus: dataplane.StatusClientClosedRequest,
				DurationMs:     timing.Elapsed(),
				ErrorClass:     dataplane.ErrorClassClientClosed,
			})
			dataplane.WriteClientClosed(w)
			dataplane.RollbackQuota(r, &h.services, resv)
			dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, 0, 0, false, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, true, requestBody, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
			return
		}
		logging.RequestError(log, "openai.stream_upstream_error", err, logging.Fields{
			Model:          model,
			UpstreamURL:    result.UpstreamURL,
			UpstreamStatus: 0,
			DurationMs:     timing.Elapsed(),
		})
		errors.WriteErrorJSON(w, errors.ErrorUpstreamFailure, err.Error())
		dataplane.RollbackQuota(r, &h.services, resv)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "upstream_failure", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "upstream_failure", true, requestBody, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		return
	}
	defer upstreamResp.Body.Close()

	middleware.AddRequestLogFields(r.Context(), logging.Fields{
		Provider:       string(provider.Type),
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
		Streaming:      true,
	})
	if upstreamResp.StatusCode != http.StatusOK {
		respBody, err := dataplane.ReadUpstreamBody(upstreamResp.Body, dataplane.DefaultMaxUpstreamResponseBytes)
		if err != nil {
			if dataplane.IsClientClosed(r, err) {
				logging.RequestWarn(log, "openai.stream_client_closed", err, logging.Fields{
					Model:          model,
					UpstreamURL:    result.UpstreamURL,
					UpstreamStatus: dataplane.StatusClientClosedRequest,
					DurationMs:     timing.Elapsed(),
					ErrorClass:     dataplane.ErrorClassClientClosed,
				})
				dataplane.WriteClientClosed(w)
				dataplane.RollbackQuota(r, &h.services, resv)
				dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, 0, 0, false, timing.StartedAt())
				dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
				now := time.Now()
				dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, true, requestBody, []byte("client closed request"), capture.ResponseFormatText, timing.StartedAt(), &now)
				return
			}
			logging.RequestError(log, "openai.read_stream_error_body", err, logging.Fields{
				Model:          model,
				UpstreamURL:    result.UpstreamURL,
				UpstreamStatus: upstreamResp.StatusCode,
				DurationMs:     timing.Elapsed(),
			})
			errors.WriteErrorJSON(w, errors.ErrorUpstreamFailure, "failed to read upstream error response")
			dataplane.RollbackQuota(r, &h.services, resv)
			dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "upstream_failure", 0, 0, false, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", "upstream_failure", true, requestBody, []byte("failed to read upstream error response"), capture.ResponseFormatText, timing.StartedAt(), &now)
			return
		}
		logging.RequestEvent(log, "openai.stream_error_response", logging.Fields{
			Model:          model,
			UpstreamStatus: upstreamResp.StatusCode,
			UpstreamURL:    result.UpstreamURL,
			DurationMs:     timing.Elapsed(),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = w.Write(respBody)
		dataplane.RollbackQuota(r, &h.services, resv)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "upstream_error", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "upstream_error", true, requestBody, respBody, capture.ResponseFormatJSON, timing.StartedAt(), &now)
		return
	}

	logging.RequestEvent(log, "openai.stream_start", logging.Fields{
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
		DurationMs:     upstreamDuration,
	})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var eventCount int
	var tokenUsage usagepkg.TokenUsage
	var outputChars int64
	var sawUsage bool
	var sawTerminal bool
	var streamCapture = dataplane.NewStreamCapture(h.services.CaptureConfig.MaxPayloadBytes)
	finalizeStreamUsage := func() bool {
		estimated := !sawUsage
		if estimated {
			tokenUsage.InputTokens = usagepkg.EstimateRequestTokens(requestBody)
			tokenUsage.OutputTokens = outputChars / 4
			tokenUsage.TotalTokens = tokenUsage.InputTokens + tokenUsage.OutputTokens
		} else {
			if tokenUsage.OutputTokens == 0 && outputChars > 0 {
				tokenUsage.OutputTokens = outputChars / 4
				estimated = true
			}
			if tokenUsage.TotalTokens != 0 && !estimated {
				return estimated
			}
			outputTokens := tokenUsage.OutputTokens
			if outputTokens < tokenUsage.ReasoningTokens {
				outputTokens = tokenUsage.ReasoningTokens
			}
			tokenUsage.TotalTokens = tokenUsage.InputTokens + outputTokens + tokenUsage.CacheReadTokens + tokenUsage.CacheWriteTokens
		}
		return estimated
	}
	if err := stream.Relay(streamCtx, w, upstreamResp, func(e *stream.Event) {
		eventCount++
		streamCapture.Append(e)
		// The OpenAI/DeepSeek SSE terminal sentinel carries no tokens, so note it
		// and fall through (do NOT return early) to the usage/output accounting.
		if e.Data == "[DONE]" {
			sawTerminal = true
		}
		if streamUsage, ok := usagepkg.ExtractStreamTokenUsage(e.Data); ok {
			tokenUsage = streamUsage
			sawUsage = true
			return
		}
		if e.Data != "" && e.Data != "[DONE]" {
			outputChars += int64(len(e.Data))
		}
	}); err != nil {
		estimated := finalizeStreamUsage()
		if dataplane.IsClientClosed(r, err) {
			logging.RequestEvent(log, "openai.stream_client_closed", logging.Fields{
				Model:      model,
				ErrorClass: dataplane.ErrorClassClientClosed,
				DurationMs: timing.Elapsed(),
			})
			dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
			dataplane.RecordUsageWithCache(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, tokenUsage, estimated, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
			return
		}
		logging.RequestEvent(log, "openai.stream_error", logging.Fields{
			Model:      model,
			ErrorClass: "stream_error",
			DurationMs: timing.Elapsed(),
		})
		dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
		dataplane.RecordUsageWithCache(r, h.services.Usage, provider, model, "error", "stream_error", tokenUsage, estimated, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "stream_error", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
		return
	}

	estimated := finalizeStreamUsage()

	// stream.Relay returns nil on a plain EOF, so a stream cut short (network
	// drop, upstream crash) lands here with no error. Only a stream that emitted
	// the [DONE] terminal sentinel is a genuine success; an EOF without it is
	// truncated and classified as an error. The partial tokens still consumed
	// upstream are settled against spend quota and recorded for usage/audit.
	if !sawTerminal {
		logging.RequestEvent(log, "openai.stream_truncated", logging.Fields{
			Model:      model,
			ErrorClass: "stream_truncated",
			DurationMs: timing.Elapsed(),
			Tokens:     tokenUsage.TotalTokens,
		})
		dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
		dataplane.RecordUsageWithCache(r, h.services.Usage, provider, model, "error", "stream_truncated", tokenUsage, estimated, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "stream_truncated", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
		dataplane.EmitUserPostSendHook(r, &h.services, provider, model, "error", "stream_truncated", http.StatusOK, true, requestBody, tokenUsage, estimated, timing.StartedAt(), now)
		_ = eventCount
		return
	}

	logging.RequestEvent(log, "openai.stream_complete", logging.Fields{
		Model:      model,
		DurationMs: timing.Elapsed(),
		Tokens:     tokenUsage.TotalTokens,
	})
	dataplane.RecordUsageWithCache(r, h.services.Usage, provider, model, "success", "", tokenUsage, estimated, timing.StartedAt())
	dataplane.RecordAudit(r, h.services.Audit, provider, model, "success")
	dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, "success", "", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
	dataplane.EmitUserPostSendHook(r, &h.services, provider, model, "success", "", http.StatusOK, true, requestBody, tokenUsage, estimated, timing.StartedAt(), now)
	_ = eventCount
}

// resolveProvider resolves the upstream provider for a request from configured
// model routes first, then by path for non-model-specific requests.
func (h *ChatHandler) resolveProvider(r *http.Request, model string) (*providers.Provider, string, bool) {
	if h.registry != nil {
		if providerID := strings.TrimSpace(r.Header.Get(providers.RouteHeader)); providerID != "" {
			tenantID := ""
			if authCtx := middleware.GetAuthContext(r.Context()); authCtx != nil {
				tenantID = authCtx.TenantID
			}
			if route, ok := h.registry.ResolveModelRouteForProviderAndTenant(providerID, tenantID, model); ok {
				logging.RequestEvent(middleware.GetRequestLogger(r.Context()), "openai.model_routed", logging.Fields{
					Model:    model,
					Provider: route.Provider.Name,
				})
				return route.Provider, route.UpstreamModel, true
			}
			logging.RequestEvent(middleware.GetRequestLogger(r.Context()), "openai.provider_route_invalid", logging.Fields{
				Model:      model,
				ErrorClass: "provider_route_invalid",
			})
			return nil, "", false
		}
		tenantID := ""
		if authCtx := middleware.GetAuthContext(r.Context()); authCtx != nil {
			tenantID = authCtx.TenantID
		}
		if route, ok := h.registry.ResolveModelRoute(tenantID, publicProviderForPath(r.URL.Path), r.URL.Path, model); ok {
			logging.RequestEvent(middleware.GetRequestLogger(r.Context()), "openai.model_routed", logging.Fields{
				Model:    model,
				Provider: route.Provider.Name,
			})
			return route.Provider, route.UpstreamModel, true
		}
		if provider, ok := h.registry.GetProviderByPath(r.URL.Path); ok {
			return provider, model, true
		}
	}

	// Fallback: try the defaults.
	for _, p := range providers.DefaultProviders() {
		if p.Type == providers.ProviderOpenAI {
			return p, model, true
		}
	}
	return nil, "", false
}

func publicProviderForPath(path string) string {
	if strings.HasPrefix(path, "/deepseek/") || path == "/deepseek" {
		return "deepseek"
	}
	return "openai"
}

// ChatCompletionRequest represents an incoming OpenAI chat completion request.
type ChatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
}

// EstimatedOutputTokens returns the caller-requested output cap when present.
func (r ChatCompletionRequest) EstimatedOutputTokens() int64 {
	if r.MaxCompletionTokens != nil && *r.MaxCompletionTokens > 0 {
		return int64(*r.MaxCompletionTokens)
	}
	if r.MaxTokens != nil && *r.MaxTokens > 0 {
		return int64(*r.MaxTokens)
	}
	return 0
}

// Usage represents token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// forwardableResponseHeaders is the whitelist of upstream response headers that
// are safe to forward verbatim to the client on a buffered (non-streaming)
// response. Everything else is dropped. In particular Set-Cookie (leaks upstream
// session state) and the framing/transport headers Content-Length,
// Content-Encoding and Transfer-Encoding are NOT forwarded: the body is
// re-buffered and Go sets Content-Length itself from the buffered bytes on
// Write, so forwarding a stale length or a compression header for an
// already-decoded body would desync framing. Hop-by-hop headers (Connection,
// Keep-Alive, Proxy-Authenticate, Proxy-Authorization, TE, Trailer, Upgrade)
// are likewise dropped.
var forwardableResponseHeaders = map[string]bool{
	"Content-Type": true,
	"Retry-After":  true,
}

// forwardResponseHeaders copies only the whitelisted upstream response headers
// into dst, preserving multi-value headers. See forwardableResponseHeaders for
// the rationale on what is dropped.
func forwardResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if !forwardableResponseHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
