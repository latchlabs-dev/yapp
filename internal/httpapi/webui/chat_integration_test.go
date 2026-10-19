package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/upstream"
)

type webChatRig struct {
	pool    *pgstore.Pool
	deps    []*Dependencies
	servers []*httptest.Server
	conv    *pgstore.WebConversation
}

func chatTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local HTTP listener unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func newWebChatRig(t *testing.T, providerHandler http.Handler, manager *hooks.Manager) *webChatRig {
	t.Helper()
	dsn := os.Getenv("PIPROXY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIPROXY_TEST_POSTGRES_DSN is not set")
	}
	providerServer := chatTestServer(t, providerHandler)
	ctx := context.Background()
	pool, err := pgstore.New(ctx, pgstore.Config{DSN: dsn, MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tenantID, userID := id.New("tn_webchat"), id.New("usr_webchat")
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'Web chat test')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, tenant_id, email, display_name) VALUES ($1, $2, $3, '')`, userID, tenantID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	providerID := id.New("prv_webchat")
	conv, err := pgstore.NewWebConversationStore(pool).Create(ctx, &pgstore.WebConversation{TenantID: tenantID, UserID: userID, Model: "test-model", ProviderID: providerID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM web_conversations WHERE tenant_id=$1`, tenantID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID)
	})
	rig := &webChatRig{pool: pool, conv: conv}
	for replica := 0; replica < 2; replica++ {
		appPool, err := pgstore.New(ctx, pgstore.Config{DSN: dsn, MaxOpenConns: 2})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(appPool.Close)
		registry := providers.NewRegistry()
		if err := registry.RegisterProvider(&providers.Provider{
			ID: providerID, Type: providers.ProviderOpenAI, PublicBasePath: "/openai", UpstreamBaseURL: providerServer.URL,
			AllowedPaths: []string{"/v1/chat/completions"}, AllowPrivateEgress: true, Enabled: true,
			Models: []providers.Model{{PublicID: "test-model", UpstreamID: "test-model", Input: []string{"text"}}},
		}); err != nil {
			t.Fatal(err)
		}
		client := upstream.NewClient(upstream.Config{Timeout: 10 * time.Second, MaxConnsPerHost: 4, MaxIdleConns: 4})
		t.Cleanup(client.CloseIdleConnections)
		deps := &Dependencies{Config: Config{MaxHistoryMessages: 100}, Logger: zerolog.Nop(),
			Conversations: pgstore.NewWebConversationStore(appPool), Attachments: pgstore.NewWebAttachmentStore(appPool),
			Registry: registry, Hooks: manager,
			ChatHandler: openai.NewChatHandler(client, rewrite.NewRequestRewriter(registry), registry, dataplane.Services{Hooks: manager}),
		}
		router := chi.NewRouter()
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := middleware.WithAuthContext(r.Context(), &auth.Context{TenantID: tenantID, UserID: userID, Scopes: []string{auth.ScopeLLMInvoke}})
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		})
		router.Post("/conversations/{conversationID}/messages", sendMessageHandler(deps))
		rig.deps = append(rig.deps, deps)
		rig.servers = append(rig.servers, chatTestServer(t, router))
	}
	return rig
}

func (r *webChatRig) request(t *testing.T, ctx context.Context, replica int, content string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(sendMessageRequest{Content: content})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.servers[replica].URL+"/conversations/"+r.conv.ID+"/messages", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestWebChatConcurrentHandlersAndDisconnectRecovery(t *testing.T) {
	var calls atomic.Int64
	providerReady := make(chan struct{}, 1)
	rig := newWebChatRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"paid partial\"}}]}\r\n\r\n")
			w.(http.Flusher).Flush()
			providerReady <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n")
	}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := rig.request(t, ctx, 0, "first prompt")
	defer first.Body.Close()
	reader := bufio.NewReader(first.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("first stream: %v", err)
		}
		if strings.Contains(line, "paid partial") {
			break
		}
	}
	select {
	case <-providerReady:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	secondCtx, release := context.WithTimeout(context.Background(), 5*time.Second)
	defer release()
	second := rig.request(t, secondCtx, 1, "must not enter history")
	denied, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusConflict || !strings.Contains(string(denied), "conversation_busy") || calls.Load() != 1 {
		t.Fatalf("second replica entered a concurrent turn: %d %s (upstream calls %d)", second.StatusCode, denied, calls.Load())
	}
	var prompts int
	if err := rig.pool.QueryRow(context.Background(), `SELECT count(*) FROM web_messages WHERE conversation_id=$1`, rig.conv.ID).Scan(&prompts); err != nil || prompts != 1 {
		t.Fatalf("busy request persisted a prompt: count=%d err=%v", prompts, err)
	}
	cancel()
	first.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var released bool
		if err := rig.pool.QueryRow(context.Background(), `SELECT turn_token IS NULL FROM web_conversations WHERE id=$1`, rig.conv.ID).Scan(&released); err != nil {
			t.Fatal(err)
		}
		if released {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect did not release the turn reservation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	history, err := rig.deps[1].Conversations.ListMessages(context.Background(), rig.conv.ID, 100)
	if err != nil || len(history) != 2 || !strings.Contains(string(history[1].Content), "paid partial") || !strings.Contains(string(history[1].Annotations), "stopped") {
		t.Fatalf("stopped paid reply lost: %+v %v", history, err)
	}
	retry := rig.request(t, secondCtx, 1, "retry after stop")
	finished, _ := io.ReadAll(retry.Body)
	retry.Body.Close()
	if retry.StatusCode != 200 || !strings.Contains(string(finished), `"status":"ok"`) || calls.Load() != 2 {
		t.Fatalf("new replica could not recover the stopped turn: %d %s", retry.StatusCode, finished)
	}
	history, err = rig.deps[1].Conversations.ListMessages(context.Background(), rig.conv.ID, 100)
	if err != nil || len(history) != 4 {
		t.Fatalf("history after retry: %+v %v", history, err)
	}
	for i, message := range history {
		wantRole := []string{"user", "assistant"}[i%2]
		if message.Seq != i || message.Role != wantRole {
			t.Fatalf("turns interleaved: message %+v at index %d", message, i)
		}
	}
}

func TestWebChatReviewRefusalNeverReachesBrowserOrHistory(t *testing.T) {
	sink := &reviewedEventSink{events: make(chan hooks.Event, 2)}
	manager := hooks.NewManager(hooks.Config{Sinks: []hooks.Sink{sink}}, zerolog.Nop())
	defer manager.Close()
	manager.Append(hooks.HandlerFunc{HandlerName: "review", EventTypes: []hooks.EventType{hooks.EventUserMessagePostSend},
		Fn: func(context.Context, *hooks.Event) (*hooks.Decision, error) {
			return &hooks.Decision{Abort: true, Message: "blocked by policy"}, nil
		}})
	rig := newWebChatRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"DRAFT SECRET\",\"reasoning_content\":\"PRIVATE REASONING\"}}]}\n\ndata: [DONE]\n\n")
	}), manager)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response := rig.request(t, ctx, 0, "please answer")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if strings.Contains(string(body), "DRAFT SECRET") || strings.Contains(string(body), "PRIVATE REASONING") || strings.Contains(string(body), `"status":"ok"`) || !strings.Contains(string(body), "event: blocked") {
		t.Fatalf("review refusal released unreviewed content: %s", body)
	}
	history, err := rig.deps[0].Conversations.ListMessages(context.Background(), rig.conv.ID, 100)
	if err != nil || len(history) != 1 || !strings.Contains(string(history[0].Annotations), "blocked by policy") {
		t.Fatalf("draft stored or refusal not recorded: %+v %v", history, err)
	}
	manager.Close()
	if len(sink.events) != 1 {
		t.Fatalf("observer got %d events, want one", len(sink.events))
	}
	if ev := <-sink.events; ev.Content.Output != "" || ev.Result.Status != "blocked" {
		t.Fatalf("observer got raw draft: %+v", ev)
	}
}

func TestWebChatStreamErrorPersistsDeliveredPartialAndReportsError(t *testing.T) {
	rig := newWebChatRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial reply\"}}]}\n\n"+
			"data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"provider failed\"}}\n\ndata: [DONE]\n\n")
	}), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response := rig.request(t, ctx, 0, "please answer")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "event: error") || !strings.Contains(string(body), `"status":"error"`) || strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("stream failure reported success: %s", body)
	}
	history, err := rig.deps[0].Conversations.ListMessages(context.Background(), rig.conv.ID, 100)
	if err != nil || len(history) != 2 || !strings.Contains(string(history[1].Content), "partial reply") || !strings.Contains(string(history[1].Annotations), "interrupted") {
		t.Fatalf("delivered partial lost or unlabelled: %+v %v", history, err)
	}
}
