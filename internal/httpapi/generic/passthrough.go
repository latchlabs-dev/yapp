// Package generic provides a protocol-agnostic reverse-proxy handler for
// upstream LLM providers whose API protocols the proxy does not have a
// dedicated handler for (Mistral, Google Gemini, Google Vertex, Azure
// OpenAI Responses, OpenAI Codex, Amazon Bedrock, etc.).
//
// The handler extracts the model name from JSON bodies generically, uses
// the shared rewriter for path/URL transforms + SSRF protection, injects
// credentials, forwards the request verbatim, and records best-effort usage
// (token estimates when protocol-specific extraction is unavailable).
package generic

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
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

// PassthroughHandler proxies requests for any provider type without
// protocol-specific parsing. It is the catch-all handler for providers
// whose API family does not have a dedicated handler in the proxy.
type PassthroughHandler struct {
	upstream *upstream.Client
	rewriter *rewrite.RequestRewriter
	registry *providers.Registry
	services dataplane.Services
}

// NewPassthroughHandler creates a new PassthroughHandler.
func NewPassthroughHandler(
	upstreamClient *upstream.Client,
	rewriter *rewrite.RequestRewriter,
	registry *providers.Registry,
	services dataplane.Services,
) *PassthroughHandler {
	return &PassthroughHandler{
		upstream: upstreamClient,
		rewriter: rewriter,
		registry: registry,
		services: services,
	}
}

// Handle processes a request generically, forwarding it to the upstream
// provider after credential injection. It tries to extract a model name
// from the JSON body for usage tracking; on failure it records "unknown".
func (h *PassthroughHandler) Handle(w http.ResponseWriter, r *http.Request) {
	log := middleware.GetRequestLogger(r.Context())
	timing := logging.StartTiming()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logging.RequestError(log, "generic.read_body", err, logging.Fields{})
		errors.WriteErrorJSON(w, errors.ErrorBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	inspectionBody := body
	if decoded, decodeErr := decodeContentEncodedBody(body, r.Header.Get("Content-Encoding")); decodeErr == nil {
		inspectionBody = decoded
	}

	model, streaming := extractModelAndStream(inspectionBody)
	streaming = streaming || pathIndicatesStreaming(r.URL.Path)
	logging.RequestEvent(log, "generic.request", logging.Fields{
		Model:     model,
		Streaming: streaming,
	})

	// Model guards run before routing and quota: a guarded model is denied
	// with the operator's message and never reserves spend.
	if violation := dataplane.CheckModelGuard(r, &h.services, model); violation != nil {
		logging.RequestEvent(log, "generic.model_guarded", logging.Fields{
			Model:      model,
			ErrorClass: "model_guarded",
		})
		dataplane.RecordAudit(r, h.services.Audit, nil, model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, model, "error", "model_guarded", streaming, body, []byte(violation.Message), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, violation.ErrorType, violation.Message)
		return
	}

	provider, ok := h.resolveProvider(r)
	if !ok {
		logging.RequestEvent(log, "generic.provider_unavailable", logging.Fields{Model: model})
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, nil, model, "error", "provider_unavailable", streaming, body, []byte("provider not configured"), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorProviderUnavailable, "upstream provider not configured")
		return
	}
	if err := dataplane.CheckModelAccess(r, &h.services, model); err != nil {
		logging.RequestEvent(log, "generic.model_denied", logging.Fields{
			Model:      model,
			ErrorClass: "model_denied",
		})
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "model_denied", streaming, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorForbidden, err.Error())
		return
	}

	// Estimate input tokens for quota.
	estimatedTokens := usagepkg.EstimateRequestTokens(inspectionBody)
	resv, err := dataplane.CheckQuota(r, &h.services, provider, model, estimatedTokens, 0)
	if err != nil {
		logging.RequestEvent(log, "generic.quota_denied", logging.Fields{
			Model:      model,
			ErrorClass: "quota_exceeded",
		})
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "denied")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "quota_exceeded", streaming, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		writeQuotaError(w, provider, err)
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
		logging.RequestEvent(log, "generic.rewrite_error", logging.Fields{
			Model:      model,
			ErrorClass: "bad_request",
		})
		dataplane.RollbackQuota(r, &h.services, resv)
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "bad_request", streaming, body, []byte(err.Error()), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errors.ErrorUnprocessable, err.Error())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		return
	}

	if decision, err := dataplane.EmitUserPreSendHook(r.Context(), r, &h.services, provider, model, streaming, inspectionBody, timing.StartedAt()); err != nil || (decision != nil && decision.Abort) {
		dataplane.RollbackQuota(r, &h.services, resv)
		message := "request blocked by hook"
		errorType := errors.ErrorForbidden
		errorClass := "hook_blocked"
		if decision != nil {
			if decision.Message != "" {
				message = decision.Message
			}
			if decision.ErrorType != "" {
				errorClass = decision.ErrorType
			}
		} else if err != nil {
			message = "pre-send hook failed"
			errorType = errors.ErrorUpstreamFailure
			errorClass = "hook_failed"
		}
		now := time.Now()
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", errorClass, 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "denied")
		dataplane.CapturePayload(r, &h.services, provider, model, "error", errorClass, streaming, body, []byte(message), capture.ResponseFormatText, timing.StartedAt(), &now)
		errors.WriteErrorJSON(w, errorType, message)
		return
	}

	if err := dataplane.ApplyCredential(r.Context(), result.Headers, provider, h.services.Credentials); err != nil {
		logging.RequestError(log, "generic.credential_error", err, logging.Fields{Model: model})
		dataplane.RollbackQuota(r, &h.services, resv)
		errors.WriteErrorJSON(w, errors.ErrorProviderUnavailable, "upstream credential unavailable")
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "credential_unavailable", 0, 0, false, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "credential_unavailable", streaming, body, []byte("upstream credential unavailable"), capture.ResponseFormatText, timing.StartedAt(), &now)
		return
	}

	bodyReader := bytes.NewReader(body)

	if streaming {
		h.handleStreaming(w, r, result, bodyReader, inspectionBody, provider, model, log, timing, resv)
	} else {
		h.handleNonStreaming(w, r, result, bodyReader, inspectionBody, provider, model, log, timing, resv)
	}
}

// writeQuotaError keeps PiProxy-local quota failures distinguishable from an
// upstream ChatGPT account limit. The OpenAI Codex client replaces every HTTP
// 429 response with "You have hit your ChatGPT usage limit" before surfacing
// it, even when error.message says that PiProxy's concurrency guard fired.
// A proxy-specific 409 makes that client retain the precise local message.
func writeQuotaError(w http.ResponseWriter, provider *providers.Provider, err error) {
	if provider != nil && provider.Type == providers.ProviderOpenAICodexResponses {
		errors.WriteErrorJSON(w, errors.ErrorProxyQuotaExceeded, codexProxyQuotaMessage(err))
		return
	}
	errors.WriteErrorJSON(w, errors.ErrorQuotaExceeded, err.Error())
}

func codexProxyQuotaMessage(err error) string {
	var exceeded *quotapkg.ExceededError
	if !stderrors.As(err, &exceeded) {
		return "PiProxy usage limit reached. This is a local proxy limit, not a ChatGPT subscription or billing limit."
	}

	detail := strings.TrimSuffix(exceeded.Message, ".")
	guidance := " Contact your PiProxy administrator to adjust the quota or wait for it to reset."
	if exceeded.Limit == quotapkg.LimitConcurrentRequests {
		guidance = " Retry after an active request finishes or an interrupted slot expires."
	}

	return fmt.Sprintf(
		"PiProxy usage limit reached (%s/%s): %s. This is a local proxy limit, not a ChatGPT subscription or billing limit.%s",
		exceeded.Scope,
		exceeded.Limit,
		detail,
		guidance,
	)
}

// resolveProvider looks up the provider by the request path.
func (h *PassthroughHandler) resolveProvider(r *http.Request) (*providers.Provider, bool) {
	if h.registry != nil {
		if p, ok := h.registry.GetProviderByPath(r.URL.Path); ok {
			return p, true
		}
	}

	// Fallback: try defaults for the known provider types that match the path.
	for _, p := range providers.DefaultProviders() {
		if p.Enabled && pathPrefixMatches(r.URL.Path, p.PublicBasePath) {
			return p, true
		}
	}
	return nil, false
}

func pathPrefixMatches(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	return len(path) >= len(prefix) && path[:len(prefix)] == prefix &&
		(len(path) == len(prefix) || path[len(prefix)] == '/')
}

func (h *PassthroughHandler) handleNonStreaming(w http.ResponseWriter, r *http.Request, result *rewrite.Result, body io.Reader, requestBody []byte, provider *providers.Provider, model string, log zerolog.Logger, timing logging.Timing, resv *quotapkg.Reservation) {
	upstreamStart := time.Now()
	upstreamResp, err := h.upstream.DoForward(r.Context(), result.UpstreamMethod, result.UpstreamURL, result.Headers, body)
	upstreamDuration := time.Since(upstreamStart).Milliseconds()

	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "generic.client_closed", err, logging.Fields{
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
		logging.RequestError(log, "generic.upstream_error", err, logging.Fields{
			Model:       model,
			UpstreamURL: result.UpstreamURL,
			DurationMs:  timing.Elapsed(),
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
	logging.RequestEvent(log, "generic.upstream_response", logging.Fields{
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
		DurationMs:     upstreamDuration,
	})

	respBody, err := dataplane.ReadUpstreamBody(upstreamResp.Body, dataplane.DefaultMaxUpstreamResponseBytes)
	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "generic.client_closed", err, logging.Fields{
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
		logging.RequestError(log, "generic.read_upstream_body", err, logging.Fields{
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

	// Best-effort usage: estimate from body sizes since we can't parse
	// protocol-specific usage fields.
	inputTokens := usagepkg.EstimateRequestTokens(requestBody)
	outputTokens := usagepkg.EstimateTextTokens(string(respBody))
	estimated := true

	status := "success"
	resultStatus := "success"
	errorClass := ""
	if upstreamResp.StatusCode >= 400 {
		status = "error"
		resultStatus = "error"
		errorClass = "upstream_error"
	}

	logging.RequestEvent(log, "generic.complete", logging.Fields{
		Model:          model,
		UpstreamStatus: upstreamResp.StatusCode,
		Tokens:         inputTokens + outputTokens,
		DurationMs:     timing.Elapsed(),
		UpstreamURL:    result.UpstreamURL,
	})

	dataplane.RecordUsage(r, h.services.Usage, provider, model, status, errorClass, inputTokens, outputTokens, estimated, timing.StartedAt())
	dataplane.RecordAudit(r, h.services.Audit, provider, model, resultStatus)
	dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, status, errorClass, false, requestBody, respBody, capture.ResponseFormatJSON, timing.StartedAt(), &now)
	dataplane.EmitUserPostSendHook(r, &h.services, provider, model, status, errorClass, upstreamResp.StatusCode, false, requestBody, usagepkg.TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}, estimated, timing.StartedAt(), now)

	forwardResponseHeaders(w.Header(), upstreamResp.Header)
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = w.Write(respBody)
}

func (h *PassthroughHandler) handleStreaming(w http.ResponseWriter, r *http.Request, result *rewrite.Result, body io.Reader, requestBody []byte, provider *providers.Provider, model string, log zerolog.Logger, timing logging.Timing, resv *quotapkg.Reservation) {
	streamCtx, cancel := stream.NewStreamTimeout(r.Context(), provider.Timeout)
	defer cancel()

	upstreamStart := time.Now()
	upstreamResp, err := h.upstream.DoForwardAndStream(streamCtx, result.UpstreamMethod, result.UpstreamURL, result.Headers, body)
	upstreamDuration := time.Since(upstreamStart).Milliseconds()

	if err != nil {
		if dataplane.IsClientClosed(r, err) {
			logging.RequestWarn(log, "generic.stream_client_closed", err, logging.Fields{
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
		logging.RequestError(log, "generic.stream_upstream_error", err, logging.Fields{
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
				logging.RequestWarn(log, "generic.stream_client_closed", err, logging.Fields{
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
			logging.RequestError(log, "generic.read_stream_error_body", err, logging.Fields{
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
		logging.RequestEvent(log, "generic.stream_error_response", logging.Fields{
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

	logging.RequestEvent(log, "generic.stream_start", logging.Fields{
		Model:          model,
		UpstreamURL:    result.UpstreamURL,
		UpstreamStatus: upstreamResp.StatusCode,
		DurationMs:     upstreamDuration,
	})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var outputChars int64
	var streamCapture = dataplane.NewStreamCapture(h.services.CaptureConfig.MaxPayloadBytes)
	upstreamStreamFailed := false
	if err := stream.Relay(streamCtx, w, upstreamResp, func(e *stream.Event) {
		streamCapture.Append(e)
		if isUpstreamSSEFailure(e) {
			upstreamStreamFailed = true
			return
		}
		if e.Data != "" && e.Data != "[DONE]" {
			outputChars += int64(len(e.Data))
		}
	}); err != nil {
		inputTokens := usagepkg.EstimateRequestTokens(requestBody)
		outputTokens := outputChars / 4
		if dataplane.IsClientClosed(r, err) {
			logging.RequestEvent(log, "generic.stream_client_closed", logging.Fields{
				Model:      model,
				ErrorClass: dataplane.ErrorClassClientClosed,
				DurationMs: timing.Elapsed(),
			})
			dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
			dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", dataplane.ErrorClassClientClosed, inputTokens, outputTokens, true, timing.StartedAt())
			dataplane.RecordAudit(r, h.services.Audit, provider, model, dataplane.ErrorClassClientClosed)
			now := time.Now()
			dataplane.CapturePayload(r, &h.services, provider, model, "error", dataplane.ErrorClassClientClosed, true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
			return
		}
		logging.RequestEvent(log, "generic.stream_error", logging.Fields{
			Model:      model,
			ErrorClass: "stream_error",
			DurationMs: timing.Elapsed(),
		})
		dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", "stream_error", inputTokens, outputTokens, true, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", "stream_error", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
		return
	}

	inputTokens := usagepkg.EstimateRequestTokens(requestBody)
	outputTokens := outputChars / 4
	if upstreamStreamFailed {
		const errorClass = "upstream_error"
		logging.RequestEvent(log, "generic.stream_upstream_error_event", logging.Fields{
			Model:          model,
			UpstreamStatus: upstreamResp.StatusCode,
			ErrorClass:     errorClass,
			DurationMs:     timing.Elapsed(),
		})
		dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
		dataplane.RecordUsage(r, h.services.Usage, provider, model, "error", errorClass, inputTokens, outputTokens, true, timing.StartedAt())
		dataplane.RecordAudit(r, h.services.Audit, provider, model, "error")
		now := time.Now()
		dataplane.CapturePayload(r, &h.services, provider, model, "error", errorClass, true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
		dataplane.EmitUserPostSendHook(r, &h.services, provider, model, "error", errorClass, upstreamResp.StatusCode, true, requestBody, usagepkg.TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}, true, timing.StartedAt(), now)
		return
	}

	logging.RequestEvent(log, "generic.stream_complete", logging.Fields{
		Model:      model,
		DurationMs: timing.Elapsed(),
		Tokens:     inputTokens + outputTokens,
	})

	dataplane.RecordUsage(r, h.services.Usage, provider, model, "success", "", inputTokens, outputTokens, true, timing.StartedAt())
	dataplane.RecordAudit(r, h.services.Audit, provider, model, "success")
	dataplane.CompleteQuotaWithUsage(r, &h.services, resv, provider, model, inputTokens, outputTokens)
	now := time.Now()
	dataplane.CapturePayload(r, &h.services, provider, model, "success", "", true, requestBody, streamCapture.Bytes(), capture.ResponseFormatSSE, timing.StartedAt(), &now, dataplane.WithStreamCapture(streamCapture))
	dataplane.EmitUserPostSendHook(r, &h.services, provider, model, "success", "", http.StatusOK, true, requestBody, usagepkg.TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}, true, timing.StartedAt(), now)
}

// isUpstreamSSEFailure recognizes semantic failures carried inside an HTTP 200
// SSE response. OpenAI Responses/Codex uses both `type:error` and
// `type:response.failed`; the SSE transport itself still reaches a clean EOF,
// so Relay correctly returns nil and the handler must classify the event.
func isUpstreamSSEFailure(event *stream.Event) bool {
	if event == nil {
		return false
	}
	if isFailureEventType(event.Event) {
		return true
	}

	var payload struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		return false
	}
	if isFailureEventType(payload.Type) || strings.EqualFold(payload.Response.Status, "failed") {
		return true
	}
	return hasJSONValue(payload.Error) || hasJSONValue(payload.Response.Error)
}

func isFailureEventType(eventType string) bool {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "error", "response.failed":
		return true
	default:
		return false
	}
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("{}"))
}

// extractModelAndStream tries to pull "model" and "stream" fields from a JSON
// body. Returns "unknown" for model and false for stream on any parse failure.
func extractModelAndStream(body []byte) (model string, streaming bool) {
	var obj struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return "unknown", false
	}
	if obj.Model == "" {
		return "unknown", obj.Stream
	}
	return obj.Model, obj.Stream
}

const maxDecodedInspectionBytes = 64 << 20

// decodeContentEncodedBody decodes a request body for metadata extraction,
// quota estimation, hooks, and capture. The original bytes are still forwarded
// upstream unchanged. Content codings are decoded in reverse application order
// as required by RFC 9110.
func decodeContentEncodedBody(body []byte, contentEncoding string) ([]byte, error) {
	decoded := body
	encodings := strings.Split(contentEncoding, ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		encoding := strings.ToLower(strings.TrimSpace(encodings[i]))
		if encoding == "" || encoding == "identity" {
			continue
		}

		var reader io.ReadCloser
		switch encoding {
		case "gzip", "x-gzip":
			gzipReader, err := gzip.NewReader(bytes.NewReader(decoded))
			if err != nil {
				return nil, fmt.Errorf("decode gzip request body: %w", err)
			}
			reader = gzipReader
		case "zstd":
			zstdReader, err := zstd.NewReader(
				bytes.NewReader(decoded),
				zstd.WithDecoderMaxMemory(maxDecodedInspectionBytes),
			)
			if err != nil {
				return nil, fmt.Errorf("decode zstd request body: %w", err)
			}
			reader = zstdReader.IOReadCloser()
		default:
			return nil, fmt.Errorf("unsupported request content encoding %q", encoding)
		}

		next, err := io.ReadAll(io.LimitReader(reader, maxDecodedInspectionBytes+1))
		closeErr := reader.Close()
		if err != nil {
			return nil, fmt.Errorf("decode %s request body: %w", encoding, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s request decoder: %w", encoding, closeErr)
		}
		if len(next) > maxDecodedInspectionBytes {
			return nil, fmt.Errorf("decoded request body exceeds %d bytes", maxDecodedInspectionBytes)
		}
		decoded = next
	}
	return decoded, nil
}

func pathIndicatesStreaming(path string) bool {
	path = strings.ToLower(path)
	return strings.Contains(path, "streamgeneratecontent") ||
		strings.Contains(path, "stream-generate-content") ||
		strings.Contains(path, "converse-stream") ||
		strings.Contains(path, "/stream")
}

// forwardableResponseHeaders is the whitelist of upstream response headers that
// are safe to forward verbatim to the client on a buffered (non-streaming)
// response. Everything else is dropped. This is especially important for the
// generic passthrough since it proxies arbitrary providers (Gemini, Bedrock,
// Azure) whose responses may carry Set-Cookie or compression headers. In
// particular Set-Cookie (leaks upstream session state) and the framing/transport
// headers Content-Length, Content-Encoding and Transfer-Encoding are NOT
// forwarded: the body is re-buffered and Go sets Content-Length itself from the
// buffered bytes on Write, so forwarding a stale length or a compression header
// for an already-decoded body would desync framing. Hop-by-hop headers
// (Connection, Keep-Alive, Proxy-Authenticate, Proxy-Authorization, TE, Trailer,
// Upgrade) are likewise dropped.
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
