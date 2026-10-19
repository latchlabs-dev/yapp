// Package errors provides structured error types and JSON error response formatting.
package errors

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ErrorType categorizes API errors.
type ErrorType string

const (
	ErrorBadRequest          ErrorType = "bad_request"
	ErrorModelBlocked        ErrorType = "model_blocked"
	ErrorUnauthorized        ErrorType = "unauthorized"
	ErrorForbidden           ErrorType = "forbidden"
	ErrorNotFound            ErrorType = "not_found"
	ErrorConflict            ErrorType = "conflict"
	ErrorBodyTooLarge        ErrorType = "body_too_large"
	ErrorUnprocessable       ErrorType = "unprocessable"
	ErrorQuotaExceeded       ErrorType = "quota_exceeded"
	ErrorProxyQuotaExceeded  ErrorType = "proxy_quota_exceeded"
	ErrorRateLimited         ErrorType = "rate_limited"
	ErrorUpstreamFailure     ErrorType = "upstream_failure"
	ErrorUpstreamUnavailable ErrorType = "upstream_unavailable"
	ErrorUpstreamTimeout     ErrorType = "upstream_timeout"
	ErrorInternal            ErrorType = "internal_error"
	ErrorSSRFDenied          ErrorType = "ssrf_denied"
	ErrorProviderUnavailable ErrorType = "provider_unavailable"
)

// ErrorCode maps error types to HTTP status codes.
var ErrorCode = map[ErrorType]int{
	ErrorBadRequest: http.StatusBadRequest,
	// Guarded models are a permanent client-side condition: the caller must
	// switch models, so 400 (not 403/404) keeps the custom message visible in
	// every client without implying a permission or routing problem.
	ErrorModelBlocked:  http.StatusBadRequest,
	ErrorUnauthorized:  http.StatusUnauthorized,
	ErrorForbidden:     http.StatusForbidden,
	ErrorNotFound:      http.StatusNotFound,
	ErrorConflict:      http.StatusConflict,
	ErrorBodyTooLarge:  http.StatusRequestEntityTooLarge,
	ErrorUnprocessable: http.StatusUnprocessableEntity,
	ErrorQuotaExceeded: http.StatusTooManyRequests,
	// The ChatGPT-backed Codex client rewrites every HTTP 429 into a ChatGPT
	// account-limit message, even when the JSON body identifies a PiProxy-local
	// quota. Use 409 for that local conflict so the client preserves our exact
	// error message instead of blaming the user's ChatGPT subscription.
	ErrorProxyQuotaExceeded:  http.StatusConflict,
	ErrorRateLimited:         http.StatusTooManyRequests,
	ErrorUpstreamFailure:     http.StatusBadGateway,
	ErrorUpstreamUnavailable: http.StatusServiceUnavailable,
	ErrorUpstreamTimeout:     http.StatusGatewayTimeout,
	ErrorInternal:            http.StatusInternalServerError,
	ErrorSSRFDenied:          http.StatusUnprocessableEntity,
	ErrorProviderUnavailable: http.StatusServiceUnavailable,
}

// AppError represents a structured API error.
type AppError struct {
	Type      ErrorType `json:"type"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id,omitempty"`
	RetrySec  int       `json:"retry_after_seconds,omitempty"`
	Scope     string    `json:"scope,omitempty"`
	Quota     string    `json:"quota,omitempty"`
	Err       error     `json:"-"`
}

func (e *AppError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

func (e *AppError) Unwrap() error {
	return e.Err
}

// StatusCode returns the HTTP status code for this error type.
func (e *AppError) StatusCode() int {
	if code, ok := ErrorCode[e.Type]; ok {
		return code
	}
	return http.StatusInternalServerError
}

// New creates a new AppError.
func New(errType ErrorType, message string) *AppError {
	return &AppError{Type: errType, Message: message}
}

// NewWithError wraps an existing error.
func NewWithError(errType ErrorType, message string, err error) *AppError {
	return &AppError{Type: errType, Message: message, Err: err}
}

// NewWithRequestID creates an error with a request ID.
func NewWithRequestID(errType ErrorType, message, requestID string) *AppError {
	return &AppError{Type: errType, Message: message, RequestID: requestID}
}

// ErrorResponse is the JSON envelope sent to clients.
type ErrorResponse struct {
	Error *AppError `json:"error"`
}

// WriteJSON writes the error as a JSON response.
func WriteJSON(w http.ResponseWriter, appErr *AppError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(appErr.StatusCode())
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: appErr})
}

// WriteErrorJSON is a convenience for creating and writing an error in one call.
func WriteErrorJSON(w http.ResponseWriter, errType ErrorType, message string) {
	WriteJSON(w, New(errType, message))
}
