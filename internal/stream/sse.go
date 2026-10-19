// Package stream provides Server-Sent Events (SSE) streaming relay functionality.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var ErrClientClosed = errors.New("client closed")

// Event represents a single SSE event.
type Event struct {
	ID    string
	Event string
	Data  string
	Raw   []byte
}

// Reader reads SSE events from a stream.
type Reader struct {
	reader *bufio.Reader
}

// NewReader creates a new SSE reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{reader: bufio.NewReader(r)}
}

// ReadEvent reads the next SSE event from the stream and returns the parsed
// view. Returns io.EOF when the stream ends. It is a thin wrapper around
// ReadRawEvent that discards the verbatim bytes.
func (r *Reader) ReadEvent() (*Event, error) {
	_, event, err := r.ReadRawEvent()
	return event, err
}

// ReadRawEvent reads the next SSE event block from the stream, returning both
// the verbatim bytes of the block (raw) and the parsed view (event).
//
// raw contains every line exactly as read from the underlying reader —
// including original line terminators (\n or \r\n) and the trailing blank line
// that delimits the event — so the block is self-delimiting and can be
// forwarded byte-for-byte. The parsed event is provided only for capture/usage
// extraction; it is NOT used to reconstruct raw, so retry:/comment/unknown
// fields and significant whitespace survive in raw even though they are dropped
// by the parser.
//
// Returns io.EOF (with raw == nil, event == nil) when the stream ends. A
// buffered-but-unterminated trailing block is intentionally dropped, matching
// the historical ReadEvent behavior.
func (r *Reader) ReadRawEvent() (raw []byte, event *Event, err error) {
	event = &Event{}
	var acc bytes.Buffer

	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			return nil, nil, err
		}
		// Accumulate the line verbatim, including its line terminator, for
		// byte-faithful passthrough. Do not normalize \r\n to \n.
		acc.WriteString(line)

		trimmed := strings.TrimRight(line, "\r\n")

		if trimmed == "" {
			// Empty line signals end of an event. Forward every block,
			// including comment-only/retry-only/unknown-field blocks, so the
			// client receives the upstream stream verbatim.
			raw := acc.Bytes()
			event.Raw = append([]byte(nil), raw...)
			return raw, event, nil
		}

		if strings.HasPrefix(trimmed, "id:") {
			event.ID = sseFieldValue(trimmed[3:])
		} else if strings.HasPrefix(trimmed, "event:") {
			event.Event = sseFieldValue(trimmed[6:])
		} else if strings.HasPrefix(trimmed, "data:") {
			if event.Data != "" {
				event.Data += "\n"
			}
			event.Data += sseFieldValue(trimmed[5:])
		}
	}
}

func sseFieldValue(value string) string {
	if strings.HasPrefix(value, " ") {
		return value[1:]
	}
	return value
}

func IsClientClosed(err error) bool {
	return errors.Is(err, ErrClientClosed)
}

func isDownstreamClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"broken pipe",
		"connection reset by peer",
		"client disconnected",
		"client closed",
		"http2: stream closed",
		"use of closed network connection",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// Relay copies an SSE stream from an upstream response to a downstream writer.
// It handles client disconnection via context cancellation.
func Relay(ctx context.Context, w io.Writer, upstreamResp *http.Response, onEvent func(*Event)) error {
	reader := NewReader(upstreamResp.Body)
	defer upstreamResp.Body.Close()

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return fmt.Errorf("%w: %v", ErrClientClosed, ctx.Err())
			}
			return ctx.Err()
		default:
		}

		raw, event, err := reader.ReadRawEvent()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read sse event: %w", err)
		}

		// onEvent receives the parsed view (a copy) for capture/usage
		// extraction; it does not affect what is forwarded.
		if onEvent != nil {
			onEvent(event)
		}

		// Forward the event block byte-for-byte. raw is self-delimiting (it
		// includes the trailing blank line and original line terminators), so
		// retry:/comment/unknown fields and significant whitespace are
		// preserved verbatim.
		if _, err := w.Write(raw); err != nil {
			if ctx.Err() == context.Canceled || isDownstreamClosed(err) {
				return fmt.Errorf("%w: write sse event: %v", ErrClientClosed, err)
			}
			return fmt.Errorf("write sse event: %w", err)
		}

		// Flush after each event.
		flush(w)
	}
}

// flush forces buffered bytes out to the client. It uses
// http.NewResponseController, which unwraps middleware ResponseWriter wrappers
// (via their Unwrap method) to find the real flusher — a direct
// w.(http.Flusher) assertion fails on wrappers like the request-logging
// statusWriter and silently disables streaming.
func flush(w io.Writer) {
	if rw, ok := w.(http.ResponseWriter); ok {
		_ = http.NewResponseController(rw).Flush()
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// RelayJSONStream relays a JSON stream (NDJSON / chunked JSON) from upstream to downstream.
// Used for providers that don't use SSE format but still stream JSON objects.
func RelayJSONStream(ctx context.Context, w io.Writer, upstreamResp *http.Response) error {
	defer upstreamResp.Body.Close()

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return fmt.Errorf("%w: %v", ErrClientClosed, ctx.Err())
			}
			return ctx.Err()
		default:
		}

		n, err := upstreamResp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				if ctx.Err() == context.Canceled || isDownstreamClosed(writeErr) {
					return fmt.Errorf("%w: write json chunk: %v", ErrClientClosed, writeErr)
				}
				return fmt.Errorf("write json chunk: %w", writeErr)
			}
			flush(w)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read json stream: %w", err)
		}
	}
}
