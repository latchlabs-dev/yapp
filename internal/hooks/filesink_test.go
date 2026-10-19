package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestJSONLFileSinkPersistsCompatibleRecord(t *testing.T) {
	sink, eventsFile := newTestFileSink(t)
	ev := &Event{
		Type:      EventUserMessagePreSend,
		RequestID: "req_123",
		Route:     "/openai/v1/chat/completions",
	}
	if err := sink.Write(context.Background(), ev); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	var got fileRecord
	line := readSingleLine(t, eventsFile)
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if got.ReceivedAt == "" {
		t.Fatal("received_at is empty")
	}
	if got.LatencyMS < 0 {
		t.Fatalf("latency_ms = %f, want non-negative", got.LatencyMS)
	}
	if got.Headers["X-Piproxy-Event-ID"] != ev.ID {
		t.Fatalf("event id header = %q, want %q", got.Headers["X-Piproxy-Event-ID"], ev.ID)
	}
	if got.Headers["X-Piproxy-Event-Type"] != string(EventUserMessagePreSend) {
		t.Fatalf("event type header = %q", got.Headers["X-Piproxy-Event-Type"])
	}
	var stored Event
	if err := json.Unmarshal(got.Event, &stored); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if stored.RequestID != "req_123" || stored.Route != "/openai/v1/chat/completions" {
		t.Fatalf("stored event = %+v", stored)
	}
	if stored.ID == "" || stored.EmittedAt.IsZero() || stored.Actor.Kind != ActorUser {
		t.Fatalf("stored event was not normalized: %+v", stored)
	}
}

func TestJSONLFileSinkConcurrentWritesRemainJSONLines(t *testing.T) {
	sink, eventsFile := newTestFileSink(t)
	const count = 50
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sink.Write(context.Background(), &Event{Type: EventUserMessagePostSend}); err != nil {
				t.Errorf("Write() error = %v", err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != count {
		t.Fatalf("line count = %d, want %d", len(lines), count)
	}
	for i, line := range lines {
		var got fileRecord
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is invalid JSON: %v", i, err)
		}
	}
}

func TestManagerDrainsAndClosesJSONLFileSink(t *testing.T) {
	eventsFile := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := NewJSONLFileSink(eventsFile)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{QueueSize: 8, Timeout: time.Second, Sinks: []Sink{sink}}, zerolog.Nop())
	if !m.HasSinks() {
		t.Fatal("configured sink was not detected")
	}
	m.EmitAsync(&Event{Type: EventSessionUploaded, ConversationID: "session_123"})
	m.Close()

	line := readSingleLine(t, eventsFile)
	if !strings.Contains(line, `"conversation_id":"session_123"`) {
		t.Fatalf("persisted line = %s", line)
	}
	if err := sink.Write(context.Background(), &Event{Type: EventSessionUploaded}); err == nil {
		t.Fatal("Write() after manager close succeeded")
	}
	// Manager and sink shutdown are both idempotent.
	m.Close()
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestNewJSONLFileSinkValidatesPathAndCreatesParent(t *testing.T) {
	if _, err := NewJSONLFileSink(""); err == nil {
		t.Fatal("empty path was accepted")
	}
	path := filepath.Join(t.TempDir(), "nested", "events.jsonl")
	sink, err := NewJSONLFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("events file mode = %o, must not be group/world writable by default", info.Mode().Perm())
	}
}

func newTestFileSink(t *testing.T) (*JSONLFileSink, string) {
	t.Helper()
	eventsFile := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := NewJSONLFileSink(eventsFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sink.Close(); err != nil {
			t.Errorf("close sink: %v", err)
		}
	})
	return sink, eventsFile
}

func readSingleLine(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}
