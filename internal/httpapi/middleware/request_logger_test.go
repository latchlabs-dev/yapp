package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/logging"
)

func TestRequestLoggingSuppressesStartAtInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.InfoLevel)
	handler := RequestLogging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if strings.Contains(buf.String(), "request.start") {
		t.Fatalf("info logs contain request.start: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "request.complete") {
		t.Fatalf("info logs missing request.complete: %s", buf.String())
	}
}

func TestRequestLoggingIncludesAccumulatedFields(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.InfoLevel)
	handler := RequestLogging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddRequestLogFields(r.Context(), logging.Fields{
			Provider:       "openai-completions",
			Model:          "gpt-4.1-mini",
			UpstreamStatus: http.StatusOK,
			Tokens:         123,
		})
		w.WriteHeader(http.StatusNoContent)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v\n%s", err, buf.String())
	}
	if got["message"] != "request.complete" {
		t.Fatalf("message = %v, want request.complete; log=%s", got["message"], buf.String())
	}
	if got["provider"] != "openai-completions" {
		t.Fatalf("provider = %v, want openai-completions; log=%s", got["provider"], buf.String())
	}
	if got["model"] != "gpt-4.1-mini" {
		t.Fatalf("model = %v, want gpt-4.1-mini; log=%s", got["model"], buf.String())
	}
	if got["upstream_status"].(float64) != http.StatusOK {
		t.Fatalf("upstream_status = %v, want 200; log=%s", got["upstream_status"], buf.String())
	}
	if got["tokens"].(float64) != 123 {
		t.Fatalf("tokens = %v, want 123; log=%s", got["tokens"], buf.String())
	}
}

func TestRequestLoggingUsesErrorOutcomeForInBandFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.InfoLevel)
	handler := RequestLogging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddRequestLogFields(r.Context(), logging.Fields{
			Provider:       "openai-codex-responses",
			Model:          "gpt-5.6-sol",
			UpstreamStatus: http.StatusOK,
			ErrorClass:     "upstream_error",
		})
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/codex/responses", nil))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log: %v\n%s", err, buf.String())
	}
	if got["message"] != "request.error" {
		t.Fatalf("message = %v, want request.error; log=%s", got["message"], buf.String())
	}
	if got["status"].(float64) != http.StatusOK {
		t.Fatalf("status = %v, want wire status 200; log=%s", got["status"], buf.String())
	}
	if got["error_class"] != "upstream_error" {
		t.Fatalf("error_class = %v, want upstream_error; log=%s", got["error_class"], buf.String())
	}
}
