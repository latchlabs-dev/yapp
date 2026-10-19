package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
)

// The console speaks one event vocabulary to the browser regardless of which
// provider protocol served the turn. Normalising here rather than in the SPA
// keeps OpenAI's and Anthropic's very different stream shapes out of the UI.
const (
	eventStart     = "start"
	eventDelta     = "delta"
	eventReasoning = "reasoning"
	eventUsage     = "usage"
	eventNotice    = "notice"
	eventBlocked   = "blocked"
	eventError     = "error"
	eventDone      = "done"
)

// A single event should contain a small delta. Bound an unfinished frame too,
// so a missing delimiter cannot retain the entire upstream response.
const maxSSEFrameBytes = 1 << 20

var errSSEFrameTooLarge = errors.New("upstream event exceeds the frame limit")

// protocol selects the upstream stream dialect to parse.
type protocol int

const (
	protocolOpenAI protocol = iota
	protocolAnthropic
)

// sseWriter emits normalised events to the browser.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	s := &sseWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeats proxy buffering, which would otherwise hold the whole stream
	// until completion and make the turn look frozen.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s.flush()
	return s
}

// Event writes one named event. Errors are ignored: a write failure means the
// browser is gone, which the surrounding turn detects by context cancellation.
func (s *sseWriter) Event(name string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = s.w.Write([]byte("event: " + name + "\ndata: "))
	_, _ = s.w.Write(encoded)
	_, _ = s.w.Write([]byte("\n\n"))
	s.flush()
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// tokenUsage is the per-turn accounting the console persists on the assistant
// message so a conversation can show what it cost.
type tokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// turnRecorder sits between the protocol handler and the browser. It satisfies
// http.ResponseWriter so the existing handlers can write into it unmodified,
// parses whatever they produce, and re-emits it in the console's vocabulary
// while accumulating the assistant text for persistence.
//
// The handler's status code is captured but never forwarded: the browser
// already has a 200 with an open event stream by the time the handler runs, so
// a failure has to arrive as an `error` event, not as a status.
type turnRecorder struct {
	sse      *sseWriter
	protocol protocol
	canWrite func() bool

	header      http.Header
	status      int
	wroteHeader bool
	streaming   bool

	frames     bytes.Buffer    // partial SSE frames awaiting a blank-line terminator
	nonSSE     bytes.Buffer    // a JSON error body, when the handler did not stream
	text       strings.Builder // assembled assistant text
	usage      tokenUsage
	stopped    bool
	errType    string
	errMessage string
	terminal   bool

	// buffered suppresses incremental delta events. It is set when a mutating
	// post-send hook is registered for this turn: a token already flushed to
	// the browser cannot be recalled, so the reply must be withheld until the
	// hook has had its say.
	buffered bool
}

func newTurnRecorder(sse *sseWriter, p protocol, buffered bool) *turnRecorder {
	return &turnRecorder{sse: sse, protocol: p, header: http.Header{}, status: http.StatusOK, buffered: buffered}
}

// emitDelta forwards a chunk of the reply, unless the turn is buffering or has
// already ended in an error. A token that reaches the browser after an error
// renders as trailing text under a failed turn the UI will not persist.
func (t *turnRecorder) emitDelta(text string) {
	if t.stopped || !t.canEmitContent() {
		return
	}
	t.text.WriteString(text)
	if !t.buffered {
		t.sse.Event(eventDelta, map[string]string{"text": text})
	}
}

func (t *turnRecorder) canEmitContent() bool {
	return t.canWrite == nil || t.canWrite()
}

func (t *turnRecorder) emitReasoning(text string) {
	if !t.buffered && !t.stopped && t.canEmitContent() {
		t.sse.Event(eventReasoning, map[string]string{"text": text})
	}
}

// Annotations returns any hook annotations the delegated handler published on
// the response, so the console can show that a guardrail acted.
func (t *turnRecorder) Annotations() map[string]string {
	raw := t.header.Get(dataplane.HookAnnotationsHeader)
	if raw == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (t *turnRecorder) Header() http.Header { return t.header }

func (t *turnRecorder) WriteHeader(status int) {
	if t.wroteHeader {
		return
	}
	t.wroteHeader = true
	t.status = status
	t.streaming = status == http.StatusOK &&
		strings.Contains(t.header.Get("Content-Type"), "text/event-stream")
}

func (t *turnRecorder) Write(p []byte) (int, error) {
	if t.canWrite != nil && !t.canWrite() {
		return 0, errors.New("conversation turn is no longer active")
	}
	if !t.wroteHeader {
		t.WriteHeader(http.StatusOK)
	}
	if !t.streaming {
		// Cap what we retain: an upstream failure body is a short JSON error,
		// and anything larger is not worth buffering to relay.
		if remaining := (64 << 10) - t.nonSSE.Len(); remaining > 0 {
			t.nonSSE.Write(p[:min(len(p), remaining)])
		}
		return len(p), nil
	}
	if t.stopped {
		return len(p), nil
	}
	written := 0
	for written < len(p) {
		remaining := maxSSEFrameBytes - t.frames.Len()
		if remaining == 0 {
			t.frames.Reset()
			t.emitError("stream_frame_too_large", "the model stream contained an oversized event")
			return written, errSSEFrameTooLarge
		}
		// Incremental appends keep even a single huge Write bounded. Drain each
		// chunk, since one write can also contain many individually small frames.
		n := min(len(p)-written, remaining, 8<<10)
		t.frames.Write(p[written : written+n])
		written += n
		t.drain()
		if t.stopped {
			t.frames.Reset()
			return len(p), nil
		}
	}
	return written, nil
}

// Flush satisfies http.Flusher so the protocol handlers' own flush calls reach
// the browser rather than being dropped.
func (t *turnRecorder) Flush() {
	t.sse.flush()
}

// drain consumes every complete SSE frame currently buffered. A frame ends at a
// blank line; a partial frame stays buffered until the rest arrives.
func (t *turnRecorder) drain() {
	for {
		data := t.frames.Bytes()
		idx, delimiterLen := bytes.Index(data, []byte("\n\n")), 2
		// CRLF CRLF ends in LF CRLF; preserving CRLF in the forwarded
		// stream must not prevent us recognizing its blank line.
		if crlf := bytes.Index(data, []byte("\n\r\n")); crlf >= 0 && (idx < 0 || crlf < idx) {
			idx, delimiterLen = crlf, 3
		}
		if idx < 0 {
			return
		}
		frame := make([]byte, idx)
		copy(frame, data[:idx])
		t.frames.Next(idx + delimiterLen)
		t.handleFrame(string(frame))
	}
}

func (t *turnRecorder) handleFrame(frame string) {
	var eventName string
	var payload strings.Builder
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if payload.Len() > 0 {
				payload.WriteString("\n")
			}
			payload.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	data := payload.String()
	if data == "[DONE]" {
		t.terminal = true
		return
	}
	if data == "" || t.stopped {
		return
	}
	if t.protocol == protocolAnthropic {
		t.handleAnthropic(eventName, data)
		return
	}
	t.handleOpenAI(data)
}

func (t *turnRecorder) handleOpenAI(data string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Error != nil {
		t.emitError(chunk.Error.Type, chunk.Error.Message)
		return
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.ReasoningContent != "" {
			t.emitReasoning(choice.Delta.ReasoningContent)
		}
		if choice.Delta.Content != "" {
			t.emitDelta(choice.Delta.Content)
		}
	}
	if chunk.Usage != nil {
		t.usage = tokenUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
		t.sse.Event(eventUsage, t.usage)
	}
}

func (t *turnRecorder) handleAnthropic(eventName, data string) {
	switch eventName {
	case "message_start":
		var start struct {
			Message struct {
				Usage struct {
					InputTokens              int64 `json:"input_tokens"`
					CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &start) == nil {
			u := start.Message.Usage
			t.usage.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		}
	case "content_block_delta":
		if t.stopped {
			return
		}
		var delta struct {
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &delta) != nil {
			return
		}
		switch delta.Delta.Type {
		case "text_delta":
			if delta.Delta.Text != "" {
				t.emitDelta(delta.Delta.Text)
			}
		case "thinking_delta":
			if delta.Delta.Thinking != "" {
				t.emitReasoning(delta.Delta.Thinking)
			}
		}
	case "message_delta":
		var md struct {
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &md) == nil && md.Usage.OutputTokens > 0 {
			t.usage.OutputTokens = md.Usage.OutputTokens
			t.usage.TotalTokens = t.usage.InputTokens + t.usage.OutputTokens
			t.sse.Event(eventUsage, t.usage)
		}
	case "message_stop":
		t.terminal = true
	case "error":
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &e) == nil {
			t.emitError(e.Error.Type, e.Error.Message)
		}
	}
}

func (t *turnRecorder) emitError(errType, message string) {
	if t.stopped {
		return
	}
	t.stopped = true
	if message == "" {
		message = "the model request failed"
	}
	t.errType = firstNonEmpty(errType, "upstream_error")
	t.errMessage = message
	// A hook refusal is not a fault; the UI shows it differently from an
	// upstream error, so it gets its own event name.
	name := eventError
	if errType == "hook_blocked" {
		name = eventBlocked
	}
	t.sse.Event(name, map[string]string{"type": errType, "message": message})
}

// Finish verifies that the delegated stream reached its protocol's terminal
// event. A relay can return on EOF or a transport failure without writing an
// error body after headers have been sent; neither is a completed answer.
func (t *turnRecorder) Finish() {
	if t.streaming && !t.stopped && (!t.terminal || t.frames.Len() > 0) {
		t.frames.Reset()
		t.emitError("stream_truncated", "the model stream ended before the reply was complete")
	}
}

// Failed reports both HTTP failures and terminal streaming errors, returning
// the normalized reason rather than letting a started stream imply success.
func (t *turnRecorder) Failed() (errType, message string, failed bool) {
	if t.stopped {
		return t.errType, t.errMessage, true
	}
	if t.streaming || t.status < 400 {
		return "", "", false
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(t.nonSSE.Bytes(), &body) == nil && body.Error.Message != "" {
		return body.Error.Type, body.Error.Message, true
	}
	return "upstream_error", http.StatusText(t.status), true
}

// Text returns the assistant text assembled from the stream.
func (t *turnRecorder) Text() string { return t.text.String() }

// Usage returns the token accounting reported by the provider, which may be
// zero if the stream ended before a usage block arrived.
func (t *turnRecorder) Usage() tokenUsage { return t.usage }
