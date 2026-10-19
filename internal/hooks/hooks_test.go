package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestHandlerFunc(t *testing.T) {
	h := HandlerFunc{
		HandlerName: "test-handler",
		EventTypes:  []EventType{EventUserMessagePreSend},
		Fn: func(ctx context.Context, ev *Event) (*Decision, error) {
			return &Decision{Metadata: map[string]string{"seen": string(ev.Type)}}, nil
		},
	}

	if h.Name() != "test-handler" {
		t.Fatalf("Name() = %q", h.Name())
	}
	if !handlerHandles(h, EventUserMessagePreSend) {
		t.Fatal("handler should handle configured event")
	}
	if handlerHandles(h, EventSessionUploaded) {
		t.Fatal("handler should not handle unconfigured event")
	}
	decision, err := h.Handle(context.Background(), &Event{Type: EventUserMessagePreSend})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if decision.Metadata["seen"] != string(EventUserMessagePreSend) {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestManagerCloseConcurrentEmitAsync(t *testing.T) {
	m := NewManager(Config{QueueSize: 8, Timeout: time.Millisecond}, zerolog.Nop())

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 2000; j++ {
				m.EmitAsync(&Event{Type: EventSessionUploaded})
			}
		}()
	}

	close(start)
	m.Close()
	wg.Wait()
}

func TestNormalizeFillsDefaults(t *testing.T) {
	ev := &Event{}
	normalize(ev)
	if ev.ID == "" {
		t.Fatal("expected generated event id")
	}
	if ev.EmittedAt.IsZero() {
		t.Fatal("expected emitted timestamp")
	}
	if ev.Actor.Kind != ActorUser {
		t.Fatalf("actor kind = %q", ev.Actor.Kind)
	}
}

func TestEmitPreSendRunsHandlersAndWebhook(t *testing.T) {
	const secret = "hook-secret"
	var got Event
	var gotSignature string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q", r.Header.Get("Content-Type"))
		}
		gotSignature = r.Header.Get("X-Piproxy-Signature")
		if r.Header.Get("X-Piproxy-Event-Type") != string(EventUserMessagePreSend) {
			t.Fatalf("event type header = %q", r.Header.Get("X-Piproxy-Event-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	m := NewManager(Config{WebhookURL: server.URL, WebhookSecret: secret, Timeout: time.Second}, zerolog.Nop())
	defer m.Close()
	var handlerRuns atomic.Int32
	m.Append(
		HandlerFunc{
			HandlerName: "ignored",
			EventTypes:  []EventType{EventSessionUploaded},
			Fn: func(ctx context.Context, ev *Event) (*Decision, error) {
				t.Fatal("handler for unrelated event should not run")
				return nil, nil
			},
		},
		HandlerFunc{
			HandlerName: "pre-send",
			EventTypes:  []EventType{EventUserMessagePreSend},
			Fn: func(ctx context.Context, ev *Event) (*Decision, error) {
				handlerRuns.Add(1)
				return &Decision{Metadata: map[string]string{"route": ev.Route}}, nil
			},
		},
	)

	decision, err := m.EmitPreSend(context.Background(), &Event{
		Type:  EventUserMessagePreSend,
		Route: "/openai/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("EmitPreSend() error = %v", err)
	}
	if decision == nil || decision.Metadata["route"] != "/openai/v1/chat/completions" {
		t.Fatalf("decision = %+v", decision)
	}
	m.Close()
	if handlerRuns.Load() != 1 {
		t.Fatalf("pre-send handler runs = %d, want 1", handlerRuns.Load())
	}
	if got.ID == "" || got.Actor.Kind != ActorUser {
		t.Fatalf("delivered event was not normalized: %+v", got)
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSignature != wantSignature {
		t.Fatalf("signature = %q, want %q", gotSignature, wantSignature)
	}
}

func TestEmitPreSendHandlerAbortSkipsLaterHandlers(t *testing.T) {
	m := NewManager(Config{}, zerolog.Nop())
	ranLater := false
	m.Append(
		HandlerFunc{
			HandlerName: "abort",
			EventTypes:  []EventType{EventUserMessagePreSend},
			Fn: func(ctx context.Context, ev *Event) (*Decision, error) {
				return &Decision{Abort: true, StatusCode: http.StatusForbidden, ErrorType: "blocked"}, nil
			},
		},
		HandlerFunc{
			HandlerName: "later",
			EventTypes:  []EventType{EventUserMessagePreSend},
			Fn: func(ctx context.Context, ev *Event) (*Decision, error) {
				ranLater = true
				return nil, nil
			},
		},
	)

	decision, err := m.EmitPreSend(context.Background(), &Event{Type: EventUserMessagePreSend})
	if err != nil {
		t.Fatalf("EmitPreSend() error = %v", err)
	}
	if ranLater {
		t.Fatal("abort decision should skip later handlers")
	}
	if decision == nil || !decision.Abort || decision.StatusCode != http.StatusForbidden {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestEmitPreSendWebhookFailureIsAsyncAndFailOpen(t *testing.T) {
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(received)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	m := NewManager(Config{WebhookURL: server.URL, Timeout: time.Second}, zerolog.Nop())
	defer m.Close()
	decision, err := m.EmitPreSend(context.Background(), &Event{Type: EventUserMessagePreSend})
	if err != nil {
		t.Fatalf("EmitPreSend() returned async webhook failure: %v", err)
	}
	if decision != nil {
		t.Fatalf("decision = %+v, want nil", decision)
	}
	m.Close()
	select {
	case <-received:
	default:
		t.Fatal("async webhook was not attempted")
	}
}

func TestEmitPreSendDoesNotWaitForWebhook(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseServer := func() {
		releaseOnce.Do(func() { close(release) })
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(received)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	m := NewManager(Config{WebhookURL: server.URL, Timeout: 2 * time.Second}, zerolog.Nop())
	defer func() {
		releaseServer()
		m.Close()
	}()

	started := time.Now()
	decision, err := m.EmitPreSend(context.Background(), &Event{Type: EventUserMessagePreSend})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("EmitPreSend() error = %v", err)
	}
	if decision != nil {
		t.Fatalf("decision = %+v, want nil", decision)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("EmitPreSend() waited %s for the webhook", elapsed)
	}

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("async webhook delivery did not start")
	}
	releaseServer()
	m.Close()
}

func TestManagerNilSafeAndWebhookDetection(t *testing.T) {
	var m *Manager
	m.Append(HandlerFunc{})
	m.EmitAsync(&Event{Type: EventSessionUploaded})
	if m.HasWebhook() {
		t.Fatal("nil manager must not report webhook")
	}
	if m.HasSinks() {
		t.Fatal("nil manager must not report sinks")
	}
	decision, err := m.EmitPreSend(context.Background(), &Event{Type: EventUserMessagePreSend})
	if err != nil || decision != nil {
		t.Fatalf("nil EmitPreSend() = (%+v, %v)", decision, err)
	}

	withoutWebhook := NewManager(Config{}, zerolog.Nop())
	defer withoutWebhook.Close()
	if withoutWebhook.HasWebhook() {
		t.Fatal("empty webhook URL should be false")
	}
	withWebhook := NewManager(Config{WebhookURL: "http://example.invalid"}, zerolog.Nop())
	defer withWebhook.Close()
	if !withWebhook.HasWebhook() {
		t.Fatal("configured webhook URL should be true")
	}
}
