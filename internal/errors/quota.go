package errors

import (
	"net/http"
)

// QuotaExceeded creates a quota exceeded error response.
func QuotaExceeded(scope, quota, message, requestID string, retryAfter int) *AppError {
	return &AppError{
		Type:      ErrorQuotaExceeded,
		Message:   message,
		RequestID: requestID,
		RetrySec:  retryAfter,
		Scope:     scope,
		Quota:     quota,
	}
}

// WriteQuotaExceeded writes a standardized quota exceeded response.
func WriteQuotaExceeded(w http.ResponseWriter, scope, quota, message, requestID string, retryAfter int) {
	WriteJSON(w, QuotaExceeded(scope, quota, message, requestID, retryAfter))
}

// IsQuotaError checks if an error is a quota exceeded error.
func IsQuotaError(err error) bool {
	if appErr, ok := err.(*AppError); ok {
		return appErr.Type == ErrorQuotaExceeded || appErr.Type == ErrorProxyQuotaExceeded || appErr.Type == ErrorRateLimited
	}
	return false
}
