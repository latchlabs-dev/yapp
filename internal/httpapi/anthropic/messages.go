// Package anthropic provides Anthropic-compatible API handlers.
package anthropic

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
	"github.com/earlgrey/piproxy/internal/providers"
	quotapkg "github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/stream"
	"github.com/earlgrey/piproxy/internal/upstream"
	usagepkg "github.com/earlgrey/piproxy/internal/usage"
)

// MessagesHandler handles POST /anthropic/v1/messages requests.
type MessagesHandler struct {
	upstream *upstream.Client
	rewriter *rewrite.RequestRewriter
	registry *providers.Registry
	services dataplane.Services
}

// NewMessagesHandler creates a new MessagesHandler.
func NewMessagesHandler(upstreamClient *upstream.Client, rewriter *rewrite.RequestRewriter, registry *providers.Registry, services dataplane.Services) *MessagesHandler {
	return &MessagesHandler{
		upstream: upstreamClient,
		rewriter: rewriter,
		registry: registry,
		services: services,
	}
}

// Handle processes an Anthropic messages request with structured logging.
func (h *MessagesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	log := middleware.GetRequestLogger(r.Context())
	timing := logging.StartTiming()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logging.RequestError(log, "anthropic.read_body", err, logging.Fields{})
		errors.WriteErrorJSON(w, errors.ErrorBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var msgReq MessagesRequest
	if err := json.Unmarshal(body, &msgReq); err != nil {
		logging.RequestEvent(log, "anthropic.parse_error", logging.Fields{ErrorClass: "bad_request"})
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, "", "error", "bad_request", false, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorBadRequest, fmt.Sprintf("invalid request: %s", err.Error()))
		return
	}

	// Strip the corp- model namespace before routing/forwarding (see
	// dataplane.NormalizeModel).
	body, msgReq.Model = dataplane.NormalizeModel(body, msgReq.Model)

	logging.RequestEvent(log, "anthropic.request", logging.Fields{
		Model:     msgReq.Model,
		Streaming: msgReq.Stream,
	})

	// Model guards run before routing and quota: a guarded model is denied
	// with the operator's message and never reserves spend.
	if violation := dataplane.CheckModelGuard(r, &h.services, msgReq.Model); violation != nil {
		logging.RequestEvent(log, "anthropic.model_guarded", logging.Fields{
			Model:      msgReq.Model,
			ErrorClass: "model_guarded",
		})
		dataplane.RecordAudit(r, h.services.Audit, nil, msgReq.Model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, msgReq.Model, "error", "model_guarded", msgReq.Stream, body, []byte(violation.Message), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, violation.ErrorType, violation.Message)
		return
	}

	provider, ok := h.getProvider(r)
	if !ok {
		logging.RequestEvent(log, "anthropic.provider_unavailable", logging.Fields{Model: msgReq.Model})
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, msgReq.Model, "error", "provider_unavailable", msgReq.Stream, body, []byte("Anthropic provider not configured"), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorProviderUnavailable, "Anthropic provider not configured")
		return
	}
	if err := dataplane.CheckModelAccess(r, &h.services, msgReq.Model); err != nil {
		logging.RequestEvent(log, "anthropic.model_denied", logging.Fields{
			Model:      msgReq.Model,
			ErrorClass: "model_denied",
		})
		dataplane.RecordAudit(r, h.services.Audit, provider, msgReq.Model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, msgReq.Model, "error", "model_denied", msgReq.Stream, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorForbidden, err.Error())
		return
	}

	// Estimate input tokens for quota check.
	estimatedTokens := usagepkg.EstimateRequestTokens(body)
	resv, err := dataplane.CheckQuota(r, &h.services, provider, msgReq.Model, estimatedTokens, msgReq.EstimatedOutputTokens())
	if err != nil {
		logging.RequestEvent(log, "anthropic.quota_denied", logging.Fields{
			Model:      msgReq.Model,
			ErrorClass: "quota_exceeded",
		})
		dataplane.RecordAudit(r, h.services.Audit, provider, msgReq.Model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, msgReq.Model, "error", "quota_exceeded", msgReq.Stream, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
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
		logging.RequestEvent(log, "anthropic.rewrite_error", logging.Fields{
			Model:      msgReq.Model,
			ErrorClass: "bad_request",
		})
		dataplane.RollbackQuota(r, &h.services, resv)
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, msgReq.Model, "error", "bad_request", msgReq.Stream, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorUnprocessable, err.Error())
		dataplane.RecordAudit(r, h.services.Audit, provider, msgReq.Model, "error")
		return
	}

	hookDecision, hookedBody, hookErr := dataplane.ApplyUserPreSendHook(r.Context(), r, &h.services, provider, msgReq.Model, msgReq.Stream, body, timing.StartedAt())
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
		dataplane.RecordUsage(r, h.services.Usage, provider, msgReq.Model, "error", errorClass, 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, msgReq.Model, "denied")
		dataplane.CapturePayload(r, &h.services, provider, msgReq.Model, "error", errorClass, msgReq.Stream, body, []byte(message), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errorType, message)
		return
	}
	// A hook with mutation rights may have rewritten the request. Everything
	// downstream — the upstream call and the captured payload — uses the
	// rewritten body, so what is stored is what the model actually saw.
	body = hookedBody
	dataplane.SetHookAnnotations(w, hookDecision)

	if err := dataplane.ApplyCredential(r.Context(), result.Headers, provider, h.services.Credentials); err != nil {
		logging.RequestError(log, "anthropic.credential_error", err, logging.Fields{Model: msgReq.Model})
		dataplane.RollbackQuota(r, &h.services, resv)
		errors.WriteErrorJSON(w, errors.ErrorProviderUnavailable, "upstream credential unavailable")
		dataplane.RecordUsage(r, h.services.Usage, provider, msgReq.Model, "error", "credential_unavailable", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, msgReq.Model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, msgReq.Model, "error", "credential_unavailable", msgReq.Stream, body, []byte("upstream credential unavailable"), capture.ResponseFormatText, timing.StartedAt(), &now)
		return
	}

	bodyReader := bytes.NewReader(body)

	if msgReq.Stream {
		h.handleStreaming(w, r, result, bodyReader, body, provider, msgReq.Model, log, timing, resv)
	} else {
		h.handleNonStreaming(w, r, result, bodyReader, body, provider, msgReq.Model, log, timing, resv)
	}
}

func (h *MessagesHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, result *rewrite.Result, body io.Reader, requestBody []byte, provider *providers.Provider, model string, log zerolog.Logger, timing logging.Timing, resv *quotapkg.Reservation) {
	upstreamStart := time.Now()
	upstreamResp, err := h.upstream.DoForward(r.Context(), result.UpstreamMethod, result.UpstreamURL, result.Headers, body)
	upstreamDuration := time.Since(upstreamStart).Milliseconds()

	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "anthropic.client_closed", err, logging.Fields{
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
		logging.RequestError(log, "anthropic.upstream_error", err, logging.Fields{
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
	logging.RequestEvent(log, "anthropic.upstream_response", logging.Fields{
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
		DurationMs:     upstreamDuration,
	})

	respBody, err := dataplane.ReadUpstreamBody(upstreamResp.Body, dataplane.DefaultMaxUpstreamResponseBytes)
	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "anthropic.client_closed", err, logging.Fields{
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
		logging.RequestError(log, "anthropic.read_upstream_body", err, logging.Fields{
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
		logging.RequestEvent(log, "anthropic.context_overflow", logging.Fields{
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

	inputTokens, outputTokens, totalTokens, _ := usagepkg.ExtractAnthropicUsage(respBody)
	estimated := false
	if totalTokens == 0 {
		inputTokens = usagepkg.EstimateRequestTokens(requestBody)
		outputTokens = usagepkg.EstimateTextTokens(string(respBody))
		totalTokens = inputTokens + outputTokens
		estimated = true
	}
	logging.RequestEvent(log, "anthropic.complete", logging.Fields{
		Model:          model,
		UpstreamStatus: upstreamResp.StatusCode,
		Tokens:         totalTokens,
		DurationMs:     timing.Elapsed(),
		UpstreamURL:    result.UpstreamURL,
	})
	_ = inputTokens
	_ = outputTokens
	status := "success"
	resultStatus := "success"
	errorClass := ""
	if upstreamResp.StatusCode >= 400 {
		status = "error"
		resultStatus = "error"
		errorClass = "upstream_error"
	}
	dataplane.RecordUsage(r, h.services.Usage, provider, model, status, errorClass, inputTokens, outputTokens, estimated, timing.StartedAt())
	dataplane.RecordAudit(r, h.services.Audit, provider, model, resultStatus)
	dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, status, errorClass, false, requestBody, respBody, capture.ResponseFormatJSON, timing.StartedAt(), &now)
	dataplane.EmitUserPostSendHook(r, &h.services, provider, model, status, errorClass, upstreamResp.StatusCode, false, requestBody, usagepkg.TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: totalTokens}, estimated, timing.StartedAt(), now)

	forwardResponseHeaders(w.Header(), upstreamResp.Header)
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = w.Write(respBody)
}

func (h *MessagesHandler) handleStreaming(w http.ResponseWriter, r *http.Request, result *rewrite.Result, body io.Reader, requestBody []byte, provider *providers.Provider, model string, log zerolog.Logger, timing logging.Timing, resv *quotapkg.Reservation) {
	streamCtx, cancel := stream.NewStreamTimeout(r.Context(), provider.Timeout)
	defer cancel()

	upstreamStart := time.Now()
	upstreamResp, err := h.upstream.DoForwardAndStream(streamCtx, result.UpstreamMethod, result.UpstreamURL, result.Headers, body)
	upstreamDuration := time.Since(upstreamStart).Milliseconds()

	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "anthropic.stream_client_closed", err, logging.Fields{
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
		logging.RequestError(log, "anthropic.stream_upstream_error", err, logging.Fields{
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
				logging.RequestWarn(log, "anthropic.stream_client_closed", err, logging.Fields{
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
			logging.RequestError(log, "anthropic.read_stream_error_body", err, logging.Fields{
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
		logging.RequestEvent(log, "anthropic.stream_error_response", logging.Fields{
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

	logging.RequestEvent(log, "anthropic.stream_start", logging.Fields{
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
	var inputTokens int64
	var outputTokens int64
	var outputChars int64
	var sawUsage bool
	var sawTerminal bool
	var streamCapture = dataplane.NewStreamCapture(h.services.CaptureConfig.MaxPayloadBytes)
	finalizeStreamUsage := func() (usagepkg.TokenUsage, bool) {
		estimated := !sawUsage
		if estimated {
			inputTokens = usagepkg.EstimateRequestTokens(requestBody)
			outputTokens = outputChars / 4
		} else if outputTokens == 0 && outputChars > 0 {
			outputTokens = outputChars / 4
			estimated = true
		}
		return usagepkg.TokenUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		}, estimated
	}
	if err := stream.Relay(streamCtx, w, upstreamResp, func(e *stream.Event) {
		eventCount++
		streamCapture.Append(e)
		// Note the Anthropic terminal event independently of token extraction: the
		// SSE Reader splits "event: message_stop" into e.Event and its JSON into
		// e.Data, so an upstream that omits the event: line is still covered by the
		// data-payload substring check. Done before the token branch so its early
		// return cannot skip it.
		if e.Event == "message_stop" || strings.Contains(e.Data, `"type":"message_stop"`) {
			sawTerminal = true
		}
		if in, out, ok := usagepkg.ExtractAnthropicStreamTokens(e.Event, e.Data); ok {
			if in > 0 {
				inputTokens = in
			}
			if out > 0 {
				outputTokens = out
			}
			sawUsage = true
			return
		}
		if e.Data != "" && e.Data != "[DONE]" {
			outputChars += int64(len(e.Data))
		}
	}); err != nil {
		tokenUsage, estimated := finalizeStreamUsage()
		if dataplane.IsClientClosed(r, err) {
			logging.RequestEvent(log, "anthropic.stream_client_closed", logging.Fields{
				Model:      model,
				ErrorClass: dataplane.ErrorClassClientClosed,
				DurationMs: timing.Elapsed(),
			})
			dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
			dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, inputTokens, outputTokens, estimated, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
			return
		}
		logging.RequestEvent(log, "anthropic.stream_error", logging.Fields{
			Model:      model,
			ErrorClass: "stream_error",
			DurationMs: timing.Elapsed(),
		})
		dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "stream_error", inputTokens, outputTokens, estimated, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "stream_error", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
		return
	}

	tokenUsage, estimated := finalizeStreamUsage()

	// stream.Relay returns nil on a plain EOF, so a stream cut short (network
	// drop, upstream crash) lands here with no error. Only a stream that emitted
	// the message_stop terminal event is a genuine success; an EOF without it is
	// truncated and classified as an error. The partial tokens still consumed
	// upstream are settled against spend quota and recorded for usage/audit.
	if !sawTerminal {
		logging.RequestEvent(log, "anthropic.stream_truncated", logging.Fields{
			Model:      model,
			ErrorClass: "stream_truncated",
			DurationMs: timing.Elapsed(),
			Tokens:     inputTokens + outputTokens,
		})
		dataplane.CompleteQuotaWithTokenUsage(r, &h.services, resv, provider, model, tokenUsage)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "stream_truncated", inputTokens, outputTokens, estimated, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "stream_truncated", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
		dataplane.EmitUserPostSendHook(r, &h.services, provider, model, "error", "stream_truncated", http.StatusOK, true, requestBody, usagepkg.TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}, estimated, timing.StartedAt(), now)
		_ = eventCount
		return
	}

	logging.RequestEvent(log, "anthropic.stream_complete", logging.Fields{
		Model:      model,
		DurationMs: timing.Elapsed(),
		Tokens:     inputTokens + outputTokens,
	})
	dataplane.RecordUsage(r, h.services.Usage, provider, model, "success", "", inputTokens, outputTokens, estimated, timing.StartedAt())
	dataplane.RecordAudit(r, h.services.Audit, provider, model, "success")
	dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, "success", "", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
	dataplane.EmitUserPostSendHook(r, &h.services, provider, model, "success", "", http.StatusOK, true, requestBody, usagepkg.TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}, estimated, timing.StartedAt(), now)
	_ = eventCount
}

// MessagesRequest represents an incoming Anthropic messages request.
type MessagesRequest struct {
	Model         string          `json:"model"`
	Messages      json.RawMessage `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences json.RawMessage `json:"stop_sequences,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

// EstimatedOutputTokens returns Anthropic's required max_tokens value when set.
func (r MessagesRequest) EstimatedOutputTokens() int64 {
	if r.MaxTokens > 0 {
		return int64(r.MaxTokens)
	}
	return 0
}

func (h *MessagesHandler) getProvider(r *http.Request) (*providers.Provider, bool) {
	if h.registry != nil {
		if provider, ok := h.registry.GetProviderByPath(r.URL.Path); ok && provider.Type == providers.ProviderAnthropic {
			return provider, true
		}
	}

	for _, provider := range providers.DefaultProviders() {
		if provider.Type == providers.ProviderAnthropic {
			return provider, true
		}
	}
	return nil, false
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
