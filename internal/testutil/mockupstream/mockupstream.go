// Package mockupstream provides in-memory mock LLM upstream servers for
// end-to-end proxy tests.
//
// It offers two flavors of upstream:
//
//   - NewOpenAI:   an OpenAI-compatible chat-completions upstream.
//   - NewDeepSeek: a DeepSeek-style upstream that additionally returns
//     reasoning_content and DeepSeek's prompt_cache_hit_tokens /
//     prompt_cache_miss_tokens usage accounting.
//
// Both flavors capture the exact request the proxy forwards — most importantly
// the upstream Authorization token — so tests can assert the proxy injects the
// credential it is expected to, and that the calling client's own token is
// never leaked upstream. Both support streaming (SSE) and non-streaming
// responses, selected by the "stream" field of the incoming request body.
package mockupstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Style identifies the response dialect a mock server speaks.
type Style int

const (
	// StyleOpenAI emits standard OpenAI chat-completion responses.
	StyleOpenAI Style = iota
	// StyleDeepSeek emits DeepSeek-style responses (reasoning_content plus
	// prompt_cache_hit_tokens / prompt_cache_miss_tokens usage).
	StyleDeepSeek
)

// Usage is the token accounting a mock upstream reports back to the proxy. The
// cache fields are only emitted by DeepSeek-style servers.
type Usage struct {
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
}

// CapturedRequest records what the proxy actually forwarded upstream.
type CapturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
	// Authorization is the raw Authorization header the proxy sent. For
	// OpenAI/DeepSeek providers the proxy injects "Bearer <upstream-token>".
	Authorization string
	// BearerToken is Authorization with the "Bearer " prefix stripped, or ""
	// when the header is absent or not a bearer token.
	BearerToken string
	// APIKey is the x-api-key header, populated for Anthropic-style providers.
	APIKey string
	// Model is the "model" field parsed from the JSON request body. After the
	// proxy rewrites a corp-* model to its upstream ID, this is the upstream ID.
	Model string
	// Stream is the "stream" field parsed from the JSON request body.
	Stream bool
}

// Server is a mock upstream. The zero value is not usable; construct one with
// NewOpenAI or NewDeepSeek. Response fields (Usage, Content, ReasoningContent)
// may be mutated before sending traffic to customize the reply.
type Server struct {
	*httptest.Server

	Style            Style
	Content          string
	ReasoningContent string

	mu       sync.Mutex
	usage    Usage
	requests []CapturedRequest
}

// NewOpenAI starts an OpenAI-compatible mock upstream with sensible default
// usage counts. Call Close (or rely on httptest cleanup) when done.
func NewOpenAI() *Server {
	s := &Server{
		Style:   StyleOpenAI,
		Content: "Hello from the mock OpenAI upstream.",
		usage: Usage{
			PromptTokens:     11,
			CompletionTokens: 7,
			TotalTokens:      18,
		},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// NewDeepSeek starts a DeepSeek-style mock upstream. Its default usage exercises
// the cache-hit/cache-miss accounting the proxy maps onto input and cache-read
// tokens: 8 cache-hit + 12 cache-miss = 20 prompt tokens.
func NewDeepSeek() *Server {
	s := &Server{
		Style:            StyleDeepSeek,
		Content:          "Hello from the mock DeepSeek upstream.",
		ReasoningContent: "Let me reason about this step by step.",
		usage: Usage{
			PromptTokens:          20,
			CompletionTokens:      9,
			TotalTokens:           29,
			PromptCacheHitTokens:  8,
			PromptCacheMissTokens: 12,
		},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// SetUsage overrides the token usage the server reports. Safe to call between
// requests.
func (s *Server) SetUsage(u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = u
}

// Requests returns a copy of every request captured so far.
func (s *Server) Requests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CapturedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// LastRequest returns the most recently captured request, or ok=false if the
// server has not been called.
func (s *Server) LastRequest() (CapturedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return CapturedRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

// Count returns the number of requests the server has received.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}

	var parsed struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &parsed)

	captured := CapturedRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		Header:        r.Header.Clone(),
		Body:          body,
		Authorization: r.Header.Get("Authorization"),
		APIKey:        r.Header.Get("x-api-key"),
		Model:         parsed.Model,
		Stream:        parsed.Stream,
	}
	if after, ok := strings.CutPrefix(captured.Authorization, "Bearer "); ok {
		captured.BearerToken = after
	}

	s.mu.Lock()
	s.requests = append(s.requests, captured)
	usage := s.usage
	s.mu.Unlock()

	if parsed.Stream {
		s.writeStream(w, parsed.Model, usage)
		return
	}
	s.writeJSON(w, parsed.Model, usage)
}

func (s *Server) writeJSON(w http.ResponseWriter, model string, usage Usage) {
	message := map[string]any{
		"role":    "assistant",
		"content": s.Content,
	}
	if s.Style == StyleDeepSeek {
		message["reasoning_content"] = s.ReasoningContent
	}
	resp := map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		"usage":   s.usageMap(usage),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) writeStream(w http.ResponseWriter, model string, usage Usage) {
	flusher, ok := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(payload map[string]any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if ok {
			flusher.Flush()
		}
	}

	chunk := func(delta map[string]any) map[string]any {
		return map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}
	}

	writeEvent(chunk(map[string]any{"role": "assistant", "content": s.Content}))
	if s.Style == StyleDeepSeek && s.ReasoningContent != "" {
		writeEvent(chunk(map[string]any{"reasoning_content": s.ReasoningContent}))
	}
	// Final chunk carries usage (as with OpenAI stream_options.include_usage).
	writeEvent(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": []any{},
		"usage":   s.usageMap(usage),
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if ok {
		flusher.Flush()
	}
}

func (s *Server) usageMap(usage Usage) map[string]any {
	m := map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
	if s.Style == StyleDeepSeek {
		m["prompt_cache_hit_tokens"] = usage.PromptCacheHitTokens
		m["prompt_cache_miss_tokens"] = usage.PromptCacheMissTokens
	}
	return m
}
