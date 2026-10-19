package dataplane_test

import (
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/stream"
)

func TestStreamCaptureKeepsRawBytesVerbatim(t *testing.T) {
	raw := []byte(": keep-alive\r\nretry: 1000\r\nfoo: bar\r\ndata:  spaced \r\n\r\n")
	sc := dataplane.NewStreamCapture(0)

	sc.Append(&stream.Event{Event: "message", Data: "spaced", Raw: raw})

	if got := string(sc.Bytes()); got != string(raw) {
		t.Fatalf("captured SSE = %q, want raw %q", got, string(raw))
	}
	if sc.Truncated() {
		t.Fatal("unexpected truncation flag")
	}
}

func TestStreamCaptureReserializesWhenNoRaw(t *testing.T) {
	sc := dataplane.NewStreamCapture(0)

	sc.Append(&stream.Event{ID: "1", Event: "content_block_delta", Data: `{"type":"x"}`})

	want := "id: 1\nevent: content_block_delta\ndata: {\"type\":\"x\"}\n\n"
	if got := string(sc.Bytes()); got != want {
		t.Fatalf("captured SSE = %q, want %q", got, want)
	}
}

func TestStreamCaptureUnlimitedResolvesToHardCeiling(t *testing.T) {
	if got := dataplane.NewStreamCapture(0).Limit(); got != dataplane.StreamCaptureCeiling {
		t.Fatalf("expected unlimited config to map to the %d ceiling, got %d", dataplane.StreamCaptureCeiling, got)
	}
	if got := dataplane.NewStreamCapture(1024).Limit(); got != 1024 {
		t.Fatalf("expected explicit limit preserved, got %d", got)
	}
	if dataplane.StreamCaptureCeiling != dataplane.DefaultMaxUpstreamResponseBytes {
		t.Fatal("stream capture ceiling must track the non-streaming response cap")
	}
}

func TestStreamCaptureFlagsTruncationAtLimit(t *testing.T) {
	sc := dataplane.NewStreamCapture(16)

	sc.Append(&stream.Event{Data: strings.Repeat("a", 12), Raw: []byte(strings.Repeat("a", 12))})
	if sc.Truncated() {
		t.Fatal("event within the limit must not flag truncation")
	}

	// Next event only partially fits; the tail is dropped and flagged.
	sc.Append(&stream.Event{Data: strings.Repeat("b", 10), Raw: []byte(strings.Repeat("b", 10))})
	if !sc.Truncated() {
		t.Fatal("expected truncation flag once bytes are dropped")
	}
	if got := len(sc.Bytes()); got != 16 {
		t.Fatalf("expected buffer capped at 16 bytes, got %d", got)
	}

	// Everything after the limit is dropped but still flagged.
	sc.Append(&stream.Event{Data: "more", Raw: []byte("more")})
	if got := len(sc.Bytes()); got != 16 {
		t.Fatalf("expected buffer to stay capped, got %d", got)
	}
}
