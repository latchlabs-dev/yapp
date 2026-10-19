package openai_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

type repeatByte byte

func (r repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestChatHandlerRejectsBodyOverMiddlewareCap(t *testing.T) {
	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(providers.NewRegistry()),
		providers.NewRegistry(),
		dataplane.Services{},
	)

	validJSON := `{"model":"gpt-test","messages":[]}`
	padding := middleware.DefaultMaxBodyBytes - int64(len(validJSON))
	body := io.MultiReader(
		strings.NewReader(validJSON),
		io.LimitReader(repeatByte(' '), padding),
		strings.NewReader("x"),
	)
	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	wrapped := middleware.BodySize(middleware.DefaultMaxBodyBytes)(http.HandlerFunc(handler.Handle))
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversize body, got %d", rec.Code)
	}
}

func TestChatHandlerAcceptsBodyUnderCap(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderOpenAI,
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}
	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{},
	)

	// A valid body comfortably under the cap reads and forwards successfully.
	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("under-cap valid body should succeed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatHandlerContextOverflowReturnsBadRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"context_length_exceeded","message":"too long"}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderOpenAI,
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{},
	)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Fatalf("expected context overflow response, got %s", rec.Body.String())
	}
}

func TestChatHandlerDropsUnsafeUpstreamHeaders(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=leak")
		// NB: don't fake Content-Encoding/Transfer-Encoding here — labelling a
		// plain body as gzip makes Go's client transport fail to decode it
		// (502 before headers are even forwarded). The whitelist's handling of
		// those hop-by-hop/encoding headers is covered directly in
		// headers_test.go (TestForwardResponseHeaders). This end-to-end test
		// proves Set-Cookie and arbitrary upstream-internal headers are stripped.
		w.Header().Set("X-Upstream-Internal", "leak-me")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderOpenAI,
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{},
	)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	for _, blocked := range []string{"Set-Cookie", "Content-Encoding", "Transfer-Encoding", "X-Upstream-Internal"} {
		if v := rec.Header().Get(blocked); v != "" {
			t.Errorf("header %q leaked to client: %q", blocked, v)
		}
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("body not forwarded intact: %s", rec.Body.String())
	}
}

func TestChatHandlerRoutesDeepSeekProviderModelAndRewritesUpstreamModel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	received := make(chan string, 1)
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string          `json:"model"`
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if !strings.Contains(string(body.Messages), `"type":"image_url"`) || !strings.Contains(string(body.Messages), "https://example.test/image.png") {
			t.Error("vision input was lost during upstream model rewrite")
		}
		received <- body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	deepseek := providers.ApplyCatalogDefaults(&providers.Provider{
		Type:               providers.ProviderDeepSeek,
		PublicBasePath:     "/deepseek",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}, providers.CatalogDeepSeek)
	if err := registry.RegisterProvider(deepseek); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{},
	)

	for _, publicModel := range []string{"corp-deepseek-flash", "corp-deepseek-v4.1-flash", "corp-deepseek-v4-flash", "corp-deepseek-v4-flash-vision-exp"} {
		t.Run(publicModel, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/deepseek/v1/chat/completions", strings.NewReader(`{"model":"`+publicModel+`","messages":[{"role":"user","content":[{"type":"text","text":"Describe this picture"},{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.Handle(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			select {
			case model := <-received:
				if model != "deepseek-flash" {
					t.Fatalf("upstream model = %q, want deepseek-flash", model)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream did not receive request")
			}
		})
	}
}

func TestChatHandlerRouteHeaderPinsDuplicateModelToSelectedAccount(t *testing.T) {
	newServer := func(label string, hits chan<- string) *httptest.Server {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("local listener unavailable: %v", err)
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s request: %v", label, err)
			}
			hits <- label + ":" + body.Model
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		server.Listener = listener
		server.Start()
		return server
	}

	hits := make(chan string, 2)
	zaiServer := newServer("zai", hits)
	defer zaiServer.Close()
	qwenServer := newServer("qwen", hits)
	defer qwenServer.Close()

	registry := providers.NewRegistry()
	for _, p := range []*providers.Provider{
		{
			ID: "zai", Type: providers.ProviderOpenAI, Name: "ZAI",
			PublicBasePath: "/openai", UpstreamBaseURL: zaiServer.URL,
			AllowedPaths: []string{"/v1/chat/completions"}, AllowPrivateEgress: true,
			Models: []providers.Model{{PublicID: "corp-glm-5.2", UpstreamID: "glm-5.2"}}, Enabled: true,
		},
		{
			ID: "qwen", Type: providers.ProviderOpenAI, Name: "Qwen Token Plan",
			PublicBasePath: "/openai", UpstreamBaseURL: qwenServer.URL,
			AllowedPaths: []string{"/v1/chat/completions"}, AllowPrivateEgress: true,
			Models: []providers.Model{{PublicID: "corp-glm-5.2", UpstreamID: "glm-5.2"}}, Enabled: true,
		},
	} {
		if err := registry.RegisterProvider(p); err != nil {
			t.Fatalf("RegisterProvider %s failed: %v", p.ID, err)
		}
	}

	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{},
	)
	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"model":"corp-glm-5.2","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(providers.RouteHeader, "qwen")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-hits:
		if got != "qwen:glm-5.2" {
			t.Fatalf("upstream hit = %q, want qwen:glm-5.2", got)
		}
	case <-time.After(time.Second):
		t.Fatal("selected upstream was not called")
	}
}

func TestChatHandlerRouteHeaderResolvesSyntheticModel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	received := make(chan string, 1)
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received <- body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		ID: "zai", Type: providers.ProviderOpenAI, Name: "ZAI",
		PublicBasePath: "/openai", UpstreamBaseURL: upstreamServer.URL,
		AllowedPaths: []string{"/v1/chat/completions"}, AllowPrivateEgress: true,
		Models: []providers.Model{{PublicID: "corp-glm-5.3-flash", UpstreamID: "glm-5.3-flash"}}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAlias(&providers.ModelAlias{
		ID: "synthetic", PublicProvider: "openai", PublicModel: "standard-flash",
		ProviderAccountID: "zai", UpstreamModel: "glm-5.3-flash", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry), registry, dataplane.Services{},
	)
	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"model":"standard-flash","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(providers.RouteHeader, "zai")
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case model := <-received:
		if model != "glm-5.3-flash" {
			t.Fatalf("upstream model = %q, want glm-5.3-flash", model)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
	}
}

func TestChatHandlerRoutesXiaomiWithoutDoubleV1(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	receivedPath := make(chan string, 1)
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	xiaomi := providers.ApplyCatalogDefaults(&providers.Provider{
		ID:                 "xiaomi-sgp",
		Name:               "Xiaomi MiMo Token Plan (SGP)",
		Type:               providers.ProviderOpenAI,
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}, providers.CatalogXiaomiTokenSGP)
	if err := registry.RegisterProvider(xiaomi); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}
	handler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{},
	)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(`{"model":"corp-mimo-v2.5-pro","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case path := <-receivedPath:
		if path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", path)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive request")
	}
}
