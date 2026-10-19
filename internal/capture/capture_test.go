package capture_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/capture"
)

type failingStore struct{}

func (f failingStore) Put(context.Context, *capture.Payload) error {
	return errors.New("write failed")
}

func (f failingStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, errors.New("not implemented")
}

type noopStore struct{}

func (n noopStore) Put(context.Context, *capture.Payload) error {
	return nil
}

func (n noopStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, errors.New("not implemented")
}

type recordingStore struct {
	ch chan *capture.Payload
}

func (s recordingStore) Put(_ context.Context, payload *capture.Payload) error {
	s.ch <- payload
	return nil
}

func (s recordingStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, errors.New("not implemented")
}

func TestCompressRoundTrip(t *testing.T) {
	raw := bytes.Repeat([]byte(`{"message":"hello"}`), 20)
	compressed, err := capture.Compress(raw)
	if err != nil {
		t.Fatalf("compress failed: %v", err)
	}
	got, err := capture.Decompress(compressed)
	if err != nil {
		t.Fatalf("decompress failed: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("expected decompressed payload to match original")
	}
}

func TestRecorderCloseConcurrentSubmit(t *testing.T) {
	recorder := capture.NewRecorder(noopStore{}, nil, 8, 1, nil, zerolog.Nop())

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 2000; j++ {
				recorder.Submit(&capture.Payload{RequestID: "req_close_race"})
			}
		}()
	}

	close(start)
	recorder.Close(context.Background())
	wg.Wait()
}

func TestExtractConversationID(t *testing.T) {
	headers := http.Header{}
	headers.Set(capture.HeaderSessionID, "session-header")
	if got := capture.ExtractConversationID(headers, []byte(`{"conversation_id":"body"}`)); got != "session-header" {
		t.Fatalf("expected header to win, got %q", got)
	}

	headers = http.Header{}
	body := []byte(`{"metadata":{"thread_id":"thread-body"}}`)
	if got := capture.ExtractConversationID(headers, body); got != "thread-body" {
		t.Fatalf("expected metadata thread id, got %q", got)
	}
}

func TestExtractProjectIDFromHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set(capture.HeaderProjectID, "mprj_01K2PROJECTATTRIBUTION000000")
	if got := capture.ExtractProjectIDFromHeaders(headers); got != "mprj_01K2PROJECTATTRIBUTION000000" {
		t.Fatalf("project ID = %q", got)
	}
	headers.Set(capture.HeaderProjectID, "../../another-tenant")
	if got := capture.ExtractProjectIDFromHeaders(headers); got != "" {
		t.Fatalf("invalid project ID accepted: %q", got)
	}
}

func TestExtractExtensionVersionFromHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set(capture.HeaderExtensionVersion, "1.3.24")
	if got := capture.ExtractExtensionVersionFromHeaders(headers); got != "1.3.24" {
		t.Fatalf("extension version = %q", got)
	}
	headers.Set(capture.HeaderExtensionVersion, strings.Repeat("x", 65))
	if got := capture.ExtractExtensionVersionFromHeaders(headers); got != "" {
		t.Fatalf("oversized extension version accepted: %q", got)
	}
}

func TestExtractConversationIDNormalizesPiSessionPath(t *testing.T) {
	sessionPath := `/Users/example/.pi/agent/sessions/--Users-example-code-piproxy--/2026-06-11T04-13-27-011Z_019eb4e2-8e63-732d-a94c-2c11e57cc4cc.jsonl`
	want := "019eb4e2-8e63-732d-a94c-2c11e57cc4cc"

	headers := http.Header{}
	headers.Set(capture.HeaderSessionID, sessionPath)
	if got := capture.ExtractConversationID(headers, nil); got != want {
		t.Fatalf("expected normalized header session id %q, got %q", want, got)
	}

	body := []byte(`{"metadata":{"conversation_id":"` + sessionPath + `"}}`)
	if got := capture.ExtractConversationID(http.Header{}, body); got != want {
		t.Fatalf("expected normalized body conversation id %q, got %q", want, got)
	}
}

func TestNormalizeConversationIDPreservesOpaqueID(t *testing.T) {
	if got := capture.NormalizeConversationID("session-header"); got != "session-header" {
		t.Fatalf("expected opaque id to be preserved, got %q", got)
	}
}

func TestLimitBytes(t *testing.T) {
	got, truncated := capture.LimitBytes([]byte("abcdef"), 3)
	if !truncated || string(got) != "abc" {
		t.Fatalf("expected capped bytes, got %q truncated=%v", got, truncated)
	}
}

func TestRecorderPreparesPayloadBeforeStorage(t *testing.T) {
	store := recordingStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, nil, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	recorder.Submit(&capture.Payload{
		RequestID:       "req_prepare",
		RequestBody:     []byte(`{"metadata":{"thread_id":"thread-from-body"},"padding":"abcdef"}`),
		ResponseBody:    []byte("response-body"),
		MaxPayloadBytes: 8,
	})

	select {
	case payload := <-store.ch:
		if payload.ConversationID != "thread-from-body" {
			t.Fatalf("expected body-derived conversation id, got %q", payload.ConversationID)
		}
		if string(payload.RequestBody) != `{"metada` || !payload.RequestTruncated {
			t.Fatalf("expected truncated request body, got %q truncated=%v", payload.RequestBody, payload.RequestTruncated)
		}
		if string(payload.ResponseBody) != "response" || !payload.ResponseTruncated {
			t.Fatalf("expected truncated response body, got %q truncated=%v", payload.ResponseBody, payload.ResponseTruncated)
		}
		if payload.MaxPayloadBytes != 0 {
			t.Fatalf("worker-only max payload limit leaked to store: %d", payload.MaxPayloadBytes)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for captured payload")
	}
}

func TestRecorderFailOpenOnBackendError(t *testing.T) {
	recorder := capture.NewRecorder(failingStore{}, nil, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	if ok := recorder.Submit(&capture.Payload{RequestID: "req_1"}); !ok {
		t.Fatal("expected submit to accept payload even though backend will fail")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if recorder.Failed() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected failed write to be counted")
}
