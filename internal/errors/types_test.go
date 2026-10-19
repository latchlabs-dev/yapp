package errors_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/earlgrey/piproxy/internal/errors"
)

func TestErrorTypes(t *testing.T) {
	tests := []struct {
		errType    errors.ErrorType
		expStatus  int
		expMessage string
	}{
		{errors.ErrorBadRequest, http.StatusBadRequest, "bad request"},
		{errors.ErrorUnauthorized, http.StatusUnauthorized, "unauthorized"},
		{errors.ErrorForbidden, http.StatusForbidden, "forbidden"},
		{errors.ErrorNotFound, http.StatusNotFound, "not found"},
		{errors.ErrorConflict, http.StatusConflict, "conflict"},
		{errors.ErrorBodyTooLarge, http.StatusRequestEntityTooLarge, "body too large"},
		{errors.ErrorQuotaExceeded, http.StatusTooManyRequests, "quota exceeded"},
		{errors.ErrorProxyQuotaExceeded, http.StatusConflict, "proxy quota exceeded"},
		{errors.ErrorUpstreamFailure, http.StatusBadGateway, "upstream failure"},
		{errors.ErrorInternal, http.StatusInternalServerError, "internal error"},
	}

	for _, tt := range tests {
		t.Run(string(tt.errType), func(t *testing.T) {
			err := errors.New(tt.errType, tt.expMessage)
			if err.Type != tt.errType {
				t.Errorf("expected type %q, got %q", tt.errType, err.Type)
			}
			if err.Message != tt.expMessage {
				t.Errorf("expected message %q, got %q", tt.expMessage, err.Message)
			}
			if err.StatusCode() != tt.expStatus {
				t.Errorf("expected status %d, got %d", tt.expStatus, err.StatusCode())
			}
		})
	}
}

func TestNewWithRequestID(t *testing.T) {
	err := errors.NewWithRequestID(errors.ErrorUnauthorized, "invalid key", "req_123")
	if err.RequestID != "req_123" {
		t.Errorf("expected request_id 'req_123', got %q", err.RequestID)
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	err := errors.New(errors.ErrorNotFound, "resource not found")
	errors.WriteJSON(w, err)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}

	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Error.Type != "not_found" {
		t.Errorf("expected type 'not_found', got %q", resp.Error.Type)
	}
}

func TestWriteErrorJSON(t *testing.T) {
	w := httptest.NewRecorder()
	errors.WriteErrorJSON(w, errors.ErrorInternal, "something went wrong")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
}

func TestNewWithError(t *testing.T) {
	inner := errors.New(errors.ErrorBadRequest, "inner error")
	err := errors.NewWithError(errors.ErrorUpstreamFailure, "upstream failed", inner)
	if err.Err == nil {
		t.Error("expected wrapped error, got nil")
	}
	if err.Unwrap() == nil {
		t.Error("expected Unwrap to return inner error")
	}
}

func TestStatusCodeDefault(t *testing.T) {
	// Unknown error type should default to 500.
	err := errors.New("unknown_type", "test")
	if err.StatusCode() != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, err.StatusCode())
	}
}
