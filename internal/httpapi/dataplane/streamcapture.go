// StreamCapture accumulates the raw upstream SSE transcript on streaming
// requests so the captured payload mirrors what the model actually emitted.
//
// It replaces the per-handler appendCapturedSSEEvent helpers with one shared,
// truncation-aware implementation. The limit resolution matters: an unset
// (0) PROXY_CAPTURE_MAX_PAYLOAD_BYTES means "do not truncate", but the relay
// buffer still gets a hard ceiling matching the non-streaming response cap
// (DefaultMaxUpstreamResponseBytes) so a pathological upstream stream cannot
// grow capture memory without bound. Hitting any limit is surfaced through
// Truncated() so the payload row and a capture_gaps marker both record the
// loss.
package dataplane

import (
	"bytes"
	"fmt"

	"github.com/earlgrey/piproxy/internal/stream"
)

// StreamCaptureCeiling is the hard cap applied to SSE transcript buffers when
// no explicit payload limit is configured. It deliberately matches
// DefaultMaxUpstreamResponseBytes: capture never buffers more for a streaming
// response than the non-streaming path would accept before erroring.
const StreamCaptureCeiling int64 = DefaultMaxUpstreamResponseBytes

// StreamCapture is a bounded buffer of raw SSE events. It is not safe for
// concurrent use; like the bytes.Buffer it replaces, it belongs to the single
// relay goroutine of one request.
type StreamCapture struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

// NewStreamCapture resolves the effective transcript limit. configured <= 0
// means unlimited configuration, which maps to the hard ceiling.
func NewStreamCapture(configured int64) *StreamCapture {
	limit := configured
	if limit <= 0 {
		limit = StreamCaptureCeiling
	}
	return &StreamCapture{limit: limit}
}

// Append writes one upstream event to the transcript. Raw framing is kept
// verbatim when the reader provides it; otherwise the event is re-serialized
// in SSE wire format. Events or trailing bytes past the limit are dropped and
// flagged via Truncated.
func (s *StreamCapture) Append(e *stream.Event) {
	if s == nil || e == nil {
		return
	}
	if int64(s.buf.Len()) >= s.limit {
		s.truncated = true
		return
	}
	if len(e.Raw) > 0 {
		remaining := len(e.Raw)
		room := int(s.limit) - s.buf.Len()
		if room <= 0 {
			s.truncated = true
			return
		}
		if remaining > room {
			remaining = room
			s.truncated = true
		}
		_, _ = s.buf.Write(e.Raw[:remaining])
		return
	}
	write := func(format string, args ...interface{}) {
		if int64(s.buf.Len()) >= s.limit {
			s.truncated = true
			return
		}
		_, _ = fmt.Fprintf(&s.buf, format, args...)
	}
	if e.ID != "" {
		write("id: %s\n", e.ID)
	}
	if e.Event != "" {
		write("event: %s\n", e.Event)
	}
	if e.Data != "" {
		write("data: %s\n", e.Data)
	}
	write("\n")
}

// Bytes returns the captured transcript so far.
func (s *StreamCapture) Bytes() []byte {
	if s == nil {
		return nil
	}
	return s.buf.Bytes()
}

// Truncated reports whether any event bytes were dropped because the limit
// was reached.
func (s *StreamCapture) Truncated() bool {
	return s != nil && s.truncated
}

// Limit returns the effective transcript limit in bytes.
func (s *StreamCapture) Limit() int64 {
	if s == nil {
		return 0
	}
	return s.limit
}
