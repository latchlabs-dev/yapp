package stream_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/stream"
)

func TestSSEReader(t *testing.T) {
	input := "data: hello\n\n"
	reader := stream.NewReader(strings.NewReader(input))
	event, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ReadEvent failed: %v", err)
	}
	if event.Data != "hello" {
		t.Errorf("expected 'hello', got %q", event.Data)
	}
}

func TestSSEReaderMultipleEvents(t *testing.T) {
	input := "data: first\n\nid: 2\nevent: update\ndata: second\n\n"
	reader := stream.NewReader(strings.NewReader(input))

	event1, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ReadEvent 1 failed: %v", err)
	}
	if event1.Data != "first" {
		t.Errorf("expected 'first', got %q", event1.Data)
	}

	event2, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ReadEvent 2 failed: %v", err)
	}
	if event2.Data != "second" {
		t.Errorf("expected 'second', got %q", event2.Data)
	}
	if event2.Event != "update" {
		t.Errorf("expected 'update', got %q", event2.Event)
	}
	if event2.ID != "2" {
		t.Errorf("expected '2', got %q", event2.ID)
	}
}

func TestSSEReaderEOFSingle(t *testing.T) {
	reader := stream.NewReader(strings.NewReader(""))
	_, err := reader.ReadEvent()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestSSERelay(t *testing.T) {
	input := "data: hello\n\n"
	upstreamResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(input)),
		Header:     make(http.Header),
	}

	var output bytes.Buffer
	err := stream.Relay(context.Background(), &output, upstreamResp, nil)
	if err != nil {
		t.Fatalf("Relay failed: %v", err)
	}

	if !strings.Contains(output.String(), "hello") {
		t.Errorf("expected 'hello' in output, got: %s", output.String())
	}
}

func TestSSERelayWithCallback(t *testing.T) {
	input := "data: test-event\n\ndata: second\n\n"
	upstreamResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(input)),
		Header:     make(http.Header),
	}

	var events []string
	var output bytes.Buffer
	err := stream.Relay(context.Background(), &output, upstreamResp, func(e *stream.Event) {
		events = append(events, e.Data)
	})
	if err != nil {
		t.Fatalf("Relay failed: %v", err)
	}

	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
}

// relayString is a helper that runs Relay over input and returns the
// downstream output as a string.
func relayString(t *testing.T, input string) string {
	t.Helper()
	upstreamResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(input)),
		Header:     make(http.Header),
	}
	var output bytes.Buffer
	if err := stream.Relay(context.Background(), &output, upstreamResp, nil); err != nil {
		t.Fatalf("Relay failed: %v", err)
	}
	return output.String()
}

// TestSSERelayByteFaithfulFields guards M1: retry:, comment, and unknown-field
// lines must reach the client verbatim rather than being dropped by the parser.
func TestSSERelayByteFaithfulFields(t *testing.T) {
	input := ": keep-alive\nretry: 1000\nfoo: bar\nevent: update\ndata: payload\n\n"
	got := relayString(t, input)
	if got != input {
		t.Errorf("expected byte-faithful passthrough.\n got: %q\nwant: %q", got, input)
	}
}

// TestSSERelayByteFaithfulWhitespace guards M1: significant whitespace in a
// data: value must round-trip intact. The old TrimSpace path would corrupt it.
func TestSSERelayByteFaithfulWhitespace(t *testing.T) {
	input := "data:  two  spaces \n\n"
	got := relayString(t, input)
	if got != input {
		t.Errorf("expected whitespace preserved.\n got: %q\nwant: %q", got, input)
	}
}

// TestSSERelayByteFaithfulMultiLineData guards M1: multi-line data: blocks must
// keep each data: prefix and exact content.
func TestSSERelayByteFaithfulMultiLineData(t *testing.T) {
	input := "data: line one\ndata: line two\ndata: line three\n\n"
	got := relayString(t, input)
	if got != input {
		t.Errorf("expected multi-line data preserved.\n got: %q\nwant: %q", got, input)
	}
}

// TestSSERelayByteFaithfulCRLF guards M1: \r\n line terminators must not be
// normalized to \n.
func TestSSERelayByteFaithfulCRLF(t *testing.T) {
	input := "event: update\r\ndata: payload\r\n\r\n"
	got := relayString(t, input)
	if got != input {
		t.Errorf("expected CRLF preserved.\n got: %q\nwant: %q", got, input)
	}
}

// TestSSEReadRawEvent verifies ReadRawEvent returns both verbatim bytes and a
// usable parsed view.
func TestSSEReadRawEvent(t *testing.T) {
	input := "id: 7\nevent: update\ndata:  spaced \n\n"
	reader := stream.NewReader(strings.NewReader(input))
	raw, event, err := reader.ReadRawEvent()
	if err != nil {
		t.Fatalf("ReadRawEvent failed: %v", err)
	}
	if string(raw) != input {
		t.Errorf("raw not byte-faithful: got %q want %q", string(raw), input)
	}
	if event.ID != "7" || event.Event != "update" {
		t.Errorf("unexpected parsed view: %+v", event)
	}
	// Parsed Data follows SSE field-value rules: a single separator space after
	// "data:" is removed, but significant payload whitespace is preserved.
	if event.Data != " spaced " {
		t.Errorf("expected parsed data %q, got %q", " spaced ", event.Data)
	}
}

func TestSSERelayCallbackIncludesRawEvent(t *testing.T) {
	input := ": keep-alive\r\nretry: 1000\r\nfoo: bar\r\ndata:  spaced \r\n\r\n"
	upstreamResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(input)),
		Header:     make(http.Header),
	}

	var output bytes.Buffer
	var raw []byte
	err := stream.Relay(context.Background(), &output, upstreamResp, func(e *stream.Event) {
		raw = append([]byte(nil), e.Raw...)
	})
	if err != nil {
		t.Fatalf("Relay failed: %v", err)
	}
	if output.String() != input {
		t.Fatalf("relay output = %q, want %q", output.String(), input)
	}
	if string(raw) != input {
		t.Fatalf("callback raw = %q, want %q", string(raw), input)
	}
}

func TestSSERelayContextCancelled(t *testing.T) {
	input := "data: hello\n\ndata: world\n\n"
	upstreamResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(input)),
		Header:     make(http.Header),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var output bytes.Buffer
	err := stream.Relay(ctx, &output, upstreamResp, nil)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
	if !stream.IsClientClosed(err) {
		t.Fatalf("expected client-closed classification, got %v", err)
	}
}

type cancelOnWrite struct {
	cancel context.CancelFunc
}

func (w cancelOnWrite) Write([]byte) (int, error) {
	w.cancel()
	return 0, context.Canceled
}

func TestSSERelayWriteContextCancelledIsClientClosed(t *testing.T) {
	upstreamResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("data: hello\n\n")),
		Header:     make(http.Header),
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := stream.Relay(ctx, cancelOnWrite{cancel: cancel}, upstreamResp, nil)
	if !stream.IsClientClosed(err) {
		t.Fatalf("expected client-closed classification, got %v", err)
	}
}

func TestRelayJSONStream(t *testing.T) {
	input := `{"id":"1"}{"id":"2"}`
	upstreamResp := &http.Response{
		Body: io.NopCloser(strings.NewReader(input)),
	}

	var output bytes.Buffer
	err := stream.RelayJSONStream(context.Background(), &output, upstreamResp)
	if err != nil {
		t.Fatalf("RelayJSONStream failed: %v", err)
	}

	if output.String() != input {
		t.Errorf("expected %q, got %q", input, output.String())
	}
}

func TestNewStreamTimeout(t *testing.T) {
	ctx, cancel := stream.NewStreamTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("stream context timed out too early")
	default:
	}

	select {
	case <-ctx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected stream context timeout")
	}
}
