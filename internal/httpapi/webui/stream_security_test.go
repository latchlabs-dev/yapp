package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecorderCRLFAtEverySplitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		proto       protocol
	}{
		{"openai", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n", protocolOpenAI},
		{"anthropic", "event: content_block_delta\r\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\r\n\r\nevent: message_stop\r\ndata: {}\r\n\r\n", protocolAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for split := 0; split <= len(tc.frame); split++ {
				recorder, _ := recordStream(t, tc.proto, false, tc.frame[:split], tc.frame[split:])
				recorder.Finish()
				if recorder.Text() != "hello" || recorder.frames.Len() != 0 {
					t.Fatalf("split %d: text=%q pending=%d", split, recorder.Text(), recorder.frames.Len())
				}
				if errType, message, failed := recorder.Failed(); failed {
					t.Fatalf("split %d: unexpected failure %s: %s", split, errType, message)
				}
			}
		})
	}
}

func TestBufferedRecorderWithholdsReasoning(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		proto       protocol
	}{
		{"openai", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private reasoning\",\"content\":\"draft\"}}]}\n\n", protocolOpenAI},
		{"anthropic", "event: content_block_delta\ndata: {\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private reasoning\"}}\n\nevent: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"draft\"}}\n\n", protocolAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, out := recordStream(t, tc.proto, true, tc.frame)
			if recorder.Text() != "draft" {
				t.Fatalf("draft not retained for review: %q", recorder.Text())
			}
			if strings.Contains(out, "private reasoning") || strings.Contains(out, "draft") || strings.Contains(out, "event: reasoning") {
				t.Fatalf("unreviewed output leaked: %s", out)
			}
		})
	}
}

func TestRecorderStreamingFailuresStayFailed(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		proto       protocol
	}{
		{"openai", "data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"boom\"}}\n\n", protocolOpenAI},
		{"anthropic", "event: error\ndata: {\"error\":{\"type\":\"upstream_error\",\"message\":\"boom\"}}\n\n", protocolAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, out := recordStream(t, tc.proto, false, tc.frame)
			recorder.Finish()
			errType, message, failed := recorder.Failed()
			if !failed || errType != "upstream_error" || message != "boom" {
				t.Fatalf("failure = %q/%q/%v", errType, message, failed)
			}
			if strings.Count(out, "event: error") != 1 {
				t.Fatalf("error must be emitted once: %s", out)
			}
		})
	}
}

func TestRecorderRejectsUnfinishedOversizedFrame(t *testing.T) {
	r := newTurnRecorder(newSSEWriter(httptest.NewRecorder()), protocolOpenAI, false)
	r.Header().Set("Content-Type", "text/event-stream")
	r.WriteHeader(http.StatusOK)
	// A huge single write must not evade the limit by being appended before
	// the size check, and its error must stop the upstream relay.
	_, err := r.Write([]byte(strings.Repeat("x", 3*maxSSEFrameBytes)))
	if !errors.Is(err, errSSEFrameTooLarge) {
		t.Fatalf("write error = %v", err)
	}
	if r.frames.Len() > maxSSEFrameBytes {
		t.Fatalf("unbounded pending frame: %d bytes", r.frames.Len())
	}
	if errType, _, failed := r.Failed(); !failed || errType != "stream_frame_too_large" {
		t.Fatalf("oversized frame reported as success: %s/%v", errType, failed)
	}
}

func TestRecorderFinishRejectsMissingTerminal(t *testing.T) {
	r, _ := recordStream(t, protocolOpenAI, false, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	r.Finish()
	if errType, _, failed := r.Failed(); !failed || errType != "stream_truncated" {
		t.Fatalf("EOF without terminal reported as success: %s/%v", errType, failed)
	}
}

func TestRecorderIgnoresContentAfterFailure(t *testing.T) {
	r, out := recordStream(t, protocolOpenAI, false,
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"boom\"}}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"late\",\"reasoning_content\":\"secret\"}}]}\n\n",
	)
	if r.Text() != "partial" || strings.Contains(out, "late") || strings.Contains(out, "secret") {
		t.Fatalf("content after failure was retained or emitted: %q / %s", r.Text(), out)
	}
}

// Expiry can occur while a previous Event blocks on browser Flush. The next
// frame from the same Write must re-check the lease instead of relying on the
// admission check made before that stalled write started.
type leaseExpiringWriter struct {
	*httptest.ResponseRecorder
	active *bool
}

func (w *leaseExpiringWriter) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), "event: delta") {
		*w.active = false
	}
}

func TestRecorderRechecksLeaseWithinOneWrite(t *testing.T) {
	active := true
	w := &leaseExpiringWriter{ResponseRecorder: httptest.NewRecorder(), active: &active}
	r := newTurnRecorder(newSSEWriter(w), protocolOpenAI, false)
	r.canWrite = func() bool { return active }
	r.Header().Set("Content-Type", "text/event-stream")
	r.WriteHeader(http.StatusOK)
	_, _ = r.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"late content\",\"reasoning_content\":\"late reasoning\"}}]}\n\n"))
	if strings.Contains(w.Body.String(), "late content") || strings.Contains(w.Body.String(), "late reasoning") || r.Text() != "first" {
		t.Fatalf("stale buffered frames were emitted: %q / %s", r.Text(), w.Body.String())
	}
}
