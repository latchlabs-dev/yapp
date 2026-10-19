package generic

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/stream"
	"github.com/earlgrey/piproxy/internal/upstream"
	"github.com/earlgrey/piproxy/internal/usage"
)

func TestExtractModelAndStream(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantModel     string
		wantStreaming bool
	}{
		{
			name:          "standard OpenAI-style body",
			body:          `{"model": "gpt-4", "stream": true, "messages": []}`,
			wantModel:     "gpt-4",
			wantStreaming: true,
		},
		{
			name:          "non-streaming",
			body:          `{"model": "claude-sonnet", "stream": false}`,
			wantModel:     "claude-sonnet",
			wantStreaming: false,
		},
		{
			name:          "no stream field defaults to false",
			body:          `{"model": "gemini-pro"}`,
			wantModel:     "gemini-pro",
			wantStreaming: false,
		},
		{
			name:          "no model field returns unknown",
			body:          `{"prompt": "hello"}`,
			wantModel:     "unknown",
			wantStreaming: false,
		},
		{
			name:          "non-JSON returns unknown",
			body:          `not json`,
			wantModel:     "unknown",
			wantStreaming: false,
		},
		{
			name:          "empty object",
			body:          `{}`,
			wantModel:     "unknown",
			wantStreaming: false,
		},
		{
			name:          "model in nested object (not extracted)",
			body:          `{"config": {"model": "deep"}}`,
			wantModel:     "unknown",
			wantStreaming: false,
		},
		{
			name:          "empty body",
			body:          ``,
			wantModel:     "unknown",
			wantStreaming: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, streaming := extractModelAndStream([]byte(tt.body))
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if streaming != tt.wantStreaming {
				t.Errorf("streaming = %v, want %v", streaming, tt.wantStreaming)
			}
		})
	}
}

func TestExtractModelAndStreamGoogleFormat(t *testing.T) {
	// Google Gemini API uses a different JSON shape:
	// {"contents": [...], "generationConfig": {"model": "..."}}
	// Our generic extractor only looks at the top-level "model" field,
	// so for Google-format requests it will return "unknown".
	body := `{"contents":[{"parts":[{"text":"hi"}]}]}`
	model, streaming := extractModelAndStream([]byte(body))
	if model != "unknown" {
		t.Errorf("expected unknown for Google format, got %q", model)
	}
	if streaming {
		t.Error("expected streaming=false for Google format")
	}
}

func TestExtractModelPreservesBody(t *testing.T) {
	// Verify the body is not mutated by extraction.
	original := `{"model": "gpt-4", "stream": true, "extra": 42}`
	body := []byte(original)
	model, streaming := extractModelAndStream(body)

	if model != "gpt-4" {
		t.Errorf("model = %q, want gpt-4", model)
	}
	if !streaming {
		t.Error("expected streaming=true")
	}

	// Verify body unchanged (we use json.Unmarshal, not Decoder).
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("body should still be valid JSON: %v", err)
	}
}

func TestDecodeContentEncodedBodyForModelExtraction(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4-mini","stream":true,"input":"hello"}`)

	tests := []struct {
		name            string
		contentEncoding string
		encode          func(*testing.T, []byte) []byte
	}{
		{
			name:            "zstd",
			contentEncoding: "zstd",
			encode: func(t *testing.T, body []byte) []byte {
				t.Helper()
				encoder, err := zstd.NewWriter(nil)
				if err != nil {
					t.Fatalf("create zstd encoder: %v", err)
				}
				defer encoder.Close()
				return encoder.EncodeAll(body, nil)
			},
		},
		{
			name:            "gzip",
			contentEncoding: "gzip",
			encode: func(t *testing.T, body []byte) []byte {
				t.Helper()
				var encoded bytes.Buffer
				writer := gzip.NewWriter(&encoded)
				if _, err := writer.Write(body); err != nil {
					t.Fatalf("write gzip body: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("close gzip writer: %v", err)
				}
				return encoded.Bytes()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := tt.encode(t, body)
			decoded, err := decodeContentEncodedBody(encoded, tt.contentEncoding)
			if err != nil {
				t.Fatalf("decodeContentEncodedBody failed: %v", err)
			}
			if !bytes.Equal(decoded, body) {
				t.Fatalf("decoded body = %q, want %q", decoded, body)
			}
			model, streaming := extractModelAndStream(decoded)
			if model != "gpt-5.4-mini" {
				t.Fatalf("model = %q, want gpt-5.4-mini", model)
			}
			if !streaming {
				t.Fatal("expected streaming=true")
			}
			if bytes.Equal(encoded, body) {
				t.Fatal("test encoding did not alter the body")
			}
		})
	}
}

func TestDecodeContentEncodedBodyRejectsInvalidEncoding(t *testing.T) {
	if _, err := decodeContentEncodedBody([]byte("not zstd"), "zstd"); err == nil {
		t.Fatal("expected invalid zstd body to fail")
	}
	if _, err := decodeContentEncodedBody([]byte(`{"model":"gpt-5.4-mini"}`), "br"); err == nil {
		t.Fatal("expected unsupported encoding to fail")
	}
}

func TestPathIndicatesStreaming(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/bedrock/model/anthropic.claude-3/converse-stream", true},
		{"/google/v1beta/models/gemini-2.5-pro:streamGenerateContent", true},
		{"/google/v1beta/models/gemini-2.5-pro:stream-generate-content", true},
		{"/mistral/v1/chat/completions", false},
		{"/bedrock/model/anthropic.claude-3/converse", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := pathIndicatesStreaming(tt.path); got != tt.want {
				t.Fatalf("pathIndicatesStreaming(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsUpstreamSSEFailure(t *testing.T) {
	tests := []struct {
		name  string
		event *stream.Event
		want  bool
	}{
		{
			name:  "codex top-level error",
			event: &stream.Event{Data: `{"type":"error","message":"An error occurred while processing the request."}`},
			want:  true,
		},
		{
			name:  "responses failed event",
			event: &stream.Event{Data: `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded"}}}`},
			want:  true,
		},
		{
			name:  "named SSE error event",
			event: &stream.Event{Event: "error", Data: `not-json`},
			want:  true,
		},
		{
			name:  "nested error without type",
			event: &stream.Event{Data: `{"response":{"error":{"code":"server_error"}}}`},
			want:  true,
		},
		{
			name:  "normal response event",
			event: &stream.Event{Data: `{"type":"response.output_text.delta","delta":"hello"}`},
			want:  false,
		},
		{
			name:  "empty error fields",
			event: &stream.Event{Data: `{"type":"response.completed","error":null,"response":{"status":"completed","error":{}}}`},
			want:  false,
		},
		{name: "nil event", event: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUpstreamSSEFailure(tt.event); got != tt.want {
				t.Fatalf("isUpstreamSSEFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPassthroughRecordsInBandSSEErrorAsFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	const upstreamEvent = "data: {\"type\":\"error\",\"message\":\"An error occurred while processing the request.\"}\n\n"
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamEvent))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderOpenAICodexResponses,
		PublicBasePath:     "/codex",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/responses"},
		AllowPrivateEgress: true,
		Timeout:            2 * time.Second,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	captureStore := passthroughCaptureStore{ch: make(chan *capture.Payload, 1)}
	captureRecorder := capture.NewRecorder(captureStore, nil, 1, 1, nil, zerolog.Nop())
	defer captureRecorder.Close(context.Background())
	usageRecorder := usage.NewRecorder(usage.NewCostTable(), nil, nil, 1, zerolog.Nop())
	defer usageRecorder.Close()
	auditLogger := audit.NewLogger(1)
	defer auditLogger.Close()
	quotaStore := newPassthroughQuotaStore()
	quotaEnforcer := quota.NewEnforcer(quota.NewManager(), quotaStore, nil)

	handler := NewPassthroughHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{
			Usage:   usageRecorder,
			Audit:   auditLogger,
			Quota:   quotaEnforcer,
			Capture: captureRecorder,
			CaptureConfig: dataplane.CaptureSettings{
				MaxPayloadBytes: 1000,
			},
		},
	)

	var logs bytes.Buffer
	wrapped := middleware.RequestLogging(zerolog.New(&logs))(http.HandlerFunc(handler.Handle))
	req := httptest.NewRequest("POST", "/codex/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"input":"hello"}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{KeyID: "key_stream_error"}))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamEvent {
		t.Fatalf("SSE body = %q, want byte-faithful %q", rec.Body.String(), upstreamEvent)
	}
	if !strings.Contains(logs.String(), `"message":"generic.stream_upstream_error_event"`) {
		t.Fatalf("logs missing semantic upstream error: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"error_class":"upstream_error"`) || !strings.Contains(logs.String(), `"message":"request.error"`) {
		t.Fatalf("final request log did not record an error: %s", logs.String())
	}
	if strings.Contains(logs.String(), `"message":"request.complete"`) {
		t.Fatalf("semantic SSE error was logged as complete: %s", logs.String())
	}

	select {
	case payload := <-captureStore.ch:
		if payload.Status != "error" || payload.ErrorClass != "upstream_error" {
			t.Fatalf("capture outcome = %s/%s, want error/upstream_error", payload.Status, payload.ErrorClass)
		}
		if string(payload.ResponseBody) != upstreamEvent {
			t.Fatalf("captured SSE body = %q, want %q", payload.ResponseBody, upstreamEvent)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}

	select {
	case event := <-usageRecorder.Events():
		if event.Status != "error" || event.ErrorClass != "upstream_error" {
			t.Fatalf("usage outcome = %s/%s, want error/upstream_error", event.Status, event.ErrorClass)
		}
		if event.OutputTokens != 0 {
			t.Fatalf("error event counted as %d output tokens, want 0", event.OutputTokens)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage event")
	}

	select {
	case event := <-auditLogger.Events():
		if event.Result != "error" {
			t.Fatalf("audit result = %q, want error", event.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audit event")
	}

	if quotaStore.decrementCalls != 1 {
		t.Fatalf("quota concurrency releases = %d, want one settlement release", quotaStore.decrementCalls)
	}
	for key, count := range quotaStore.concurrency {
		if count != 0 {
			t.Fatalf("quota concurrency counter %s = %d after SSE error, want 0", key, count)
		}
	}
	for key, count := range quotaStore.daily {
		if strings.Contains(key, ":spend_millicents:") && count <= 0 {
			t.Fatalf("daily spend %s = %d after SSE error, want partial charge", key, count)
		}
	}
	for key, count := range quotaStore.monthly {
		if strings.Contains(key, ":spend_millicents:") && count <= 0 {
			t.Fatalf("monthly spend %s = %d after SSE error, want partial charge", key, count)
		}
	}
}

func TestPassthroughTreatsStreamingPathAsStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/anthropic.claude-3/converse-stream" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderBedrockConverse,
		PublicBasePath:     "/bedrock",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/"},
		AllowPrivateEgress: true,
		Timeout:            2 * time.Second,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	store := passthroughCaptureStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, nil, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	handler := NewPassthroughHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{
			Capture: recorder,
			CaptureConfig: dataplane.CaptureSettings{
				MaxPayloadBytes: 1000,
			},
		},
	)

	req := httptest.NewRequest(
		"POST",
		"/bedrock/model/anthropic.claude-3/converse-stream",
		strings.NewReader(`{"model":"anthropic.claude-3-sonnet"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE response, got Content-Type %q", ct)
	}

	select {
	case payload := <-store.ch:
		if !payload.Streaming {
			t.Fatalf("expected captured payload to be marked streaming")
		}
		if payload.ResponseFormat != capture.ResponseFormatSSE {
			t.Fatalf("expected SSE capture format, got %q", payload.ResponseFormat)
		}
		if !strings.Contains(string(payload.ResponseBody), `data: {"delta":"hello"}`) {
			t.Fatalf("expected captured SSE transcript, got %q", payload.ResponseBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}
}

func TestPassthroughDecodesZstdForMetadataAndForwardsOriginalBody(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}

	requestBody := []byte(`{"model":"gpt-5.4-mini","stream":false,"input":"hello"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("create zstd encoder: %v", err)
	}
	encodedBody := encoder.EncodeAll(requestBody, nil)
	encoder.Close()

	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read upstream request body: %v", readErr)
		}
		if !bytes.Equal(gotBody, encodedBody) {
			t.Errorf("upstream body was changed during inspection")
		}
		if got := r.Header.Get("Content-Encoding"); got != "zstd" {
			t.Errorf("upstream Content-Encoding = %q, want zstd", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderOpenAICodexResponses,
		PublicBasePath:     "/codex",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/"},
		AllowPrivateEgress: true,
		Timeout:            2 * time.Second,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	store := passthroughCaptureStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, nil, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	handler := NewPassthroughHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{
			Capture: recorder,
			CaptureConfig: dataplane.CaptureSettings{
				MaxPayloadBytes: 1000,
			},
		},
	)

	req := httptest.NewRequest("POST", "/codex/responses", bytes.NewReader(encodedBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case payload := <-store.ch:
		if payload.Model != "gpt-5.4-mini" {
			t.Fatalf("captured model = %q, want gpt-5.4-mini", payload.Model)
		}
		if !bytes.Equal(payload.RequestBody, requestBody) {
			t.Fatalf("captured request body = %q, want decoded JSON", payload.RequestBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}
}

func TestCodexLocalQuotaPreservesProxyWording(t *testing.T) {
	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:            providers.ProviderOpenAICodexResponses,
		PublicBasePath:  "/codex",
		UpstreamBaseURL: "https://chatgpt.com/backend-api",
		AllowedPaths:    []string{"/responses"},
		Enabled:         true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "key-concurrency",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Limits:    map[quota.LimitType]int64{quota.LimitConcurrentRequests: 16},
		Enabled:   true,
	}})
	handler := NewPassthroughHandler(
		nil,
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{Quota: quota.NewEnforcer(manager, saturatedConcurrencyStore{}, nil)},
	)
	req := httptest.NewRequest("POST", "/codex/responses", strings.NewReader(`{"model":"gpt-5.6-luna","stream":true}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{KeyID: "key_test"}))
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var response struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error.Type != "proxy_quota_exceeded" {
		t.Fatalf("error type = %q, want proxy_quota_exceeded", response.Error.Type)
	}
	for _, want := range []string{
		"PiProxy usage limit reached",
		"concurrent requests limit 16 exceeded (16 active)",
		"not a ChatGPT subscription or billing limit",
		"Retry after an active request finishes or an interrupted slot expires",
	} {
		if !strings.Contains(response.Error.Message, want) {
			t.Errorf("message %q does not contain %q", response.Error.Message, want)
		}
	}
}

type saturatedConcurrencyStore struct{}

func TestCodexQuotaDisabledStillForwardsAndRecordsUsage(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	const event = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(event))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		Type: providers.ProviderOpenAICodexResponses, PublicBasePath: "/codex",
		UpstreamBaseURL: server.URL, AllowedPaths: []string{"/responses"},
		AllowPrivateEgress: true, Enabled: true, Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := usage.NewRecorder(usage.NewCostTable(), nil, nil, 1, zerolog.Nop())
	defer recorder.Close()
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID: "saturated-key", ScopeType: quota.ScopeKey, ScopeID: "key_test", Enabled: true,
		Limits: map[quota.LimitType]int64{quota.LimitConcurrentRequests: 1},
	}})
	handler := NewPassthroughHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second}),
		rewrite.NewRequestRewriter(registry), registry,
		dataplane.Services{
			Quota:              quota.NewEnforcer(manager, saturatedConcurrencyStore{}, nil),
			CodexQuotaDisabled: true, Usage: recorder,
		},
	)
	req := httptest.NewRequest("POST", "/codex/responses", strings.NewReader(`{"model":"gpt-test","stream":true}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{KeyID: "key_test"}))
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != event {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	select {
	case usageEvent := <-recorder.Events():
		if usageEvent.Status != "success" || usageEvent.InputTokens <= 0 || usageEvent.OutputTokens <= 0 {
			t.Fatalf("usage = %+v", usageEvent)
		}
	case <-time.After(time.Second):
		t.Fatal("usage event missing for exempt request")
	}
}

func (saturatedConcurrencyStore) IsConnected() bool { return true }
func (saturatedConcurrencyStore) ConcurrencyIncr(context.Context, string) (int64, error) {
	return 17, nil
}
func (saturatedConcurrencyStore) ConcurrencyDecr(context.Context, string) (int64, error) {
	return 16, nil
}
func (saturatedConcurrencyStore) SlidingWindowAdd(context.Context, string, time.Duration, int64) (int64, error) {
	return 0, nil
}
func (saturatedConcurrencyStore) DailyAdd(context.Context, string, int64) (int64, error) {
	return 0, nil
}
func (saturatedConcurrencyStore) MonthlyAdd(context.Context, string, int64) (int64, error) {
	return 0, nil
}

type passthroughQuotaStore struct {
	daily          map[string]int64
	monthly        map[string]int64
	concurrency    map[string]int64
	decrementCalls int
}

func newPassthroughQuotaStore() *passthroughQuotaStore {
	return &passthroughQuotaStore{
		daily:       map[string]int64{},
		monthly:     map[string]int64{},
		concurrency: map[string]int64{},
	}
}

func (s *passthroughQuotaStore) IsConnected() bool { return true }

func (s *passthroughQuotaStore) ConcurrencyIncr(_ context.Context, key string) (int64, error) {
	s.concurrency[key]++
	return s.concurrency[key], nil
}

func (s *passthroughQuotaStore) ConcurrencyDecr(_ context.Context, key string) (int64, error) {
	s.decrementCalls++
	s.concurrency[key]--
	return s.concurrency[key], nil
}

func (*passthroughQuotaStore) SlidingWindowAdd(_ context.Context, _ string, _ time.Duration, amount int64) (int64, error) {
	return amount, nil
}

func (s *passthroughQuotaStore) DailyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.daily[key] += amount
	return s.daily[key], nil
}

func (s *passthroughQuotaStore) MonthlyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.monthly[key] += amount
	return s.monthly[key], nil
}

type passthroughCaptureStore struct {
	ch chan *capture.Payload
}

func (s passthroughCaptureStore) Put(_ context.Context, payload *capture.Payload) error {
	s.ch <- payload
	return nil
}

func (s passthroughCaptureStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, nil
}
