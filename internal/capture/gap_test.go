package capture_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/capture"
)

// gapSink collects recorded gap markers for assertions.
type gapSink struct {
	mu   sync.Mutex
	gaps []capture.Gap
}

func (g *gapSink) Record(gap capture.Gap) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gaps = append(g.gaps, gap)
	return true
}

func (g *gapSink) byReason(reason string) []capture.Gap {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []capture.Gap
	for _, gap := range g.gaps {
		if gap.Reason == reason {
			out = append(out, gap)
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func TestRecorderRecordsQueueOverflowGap(t *testing.T) {
	sink := &gapSink{}
	// Queue size 1 with a store that never completes writes until we let it,
	// so the queue stays full. Use a gated store.
	gate := make(chan struct{})
	store := &gatedStore{gate: gate, arrived: make(chan struct{})}
	recorder := capture.NewRecorder(store, sink, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	first := &capture.Payload{RequestID: "req_overflow_1", RequestBody: []byte("aaaaaaaaaa"), ResponseBody: []byte("bbbbbbbbbb")}
	if !recorder.Submit(first) {
		t.Fatal("expected first submit to be accepted")
	}
	// Wait until the worker has dequeued the first payload so the queue is
	// empty again but the worker is blocked, then fill it and overflow.
	<-store.arrived
	second := &capture.Payload{RequestID: "req_overflow_2", ConversationID: "conv_overflow"}
	if !recorder.Submit(second) {
		t.Fatal("expected second submit to fill the queue")
	}
	third := &capture.Payload{RequestID: "req_overflow_3", ConversationID: "conv_overflow"}
	if recorder.Submit(third) {
		t.Fatal("expected third submit to overflow")
	}
	close(gate)

	waitFor(t, func() bool { return len(sink.byReason(capture.GapReasonQueueOverflow)) > 0 })
	gap := sink.byReason(capture.GapReasonQueueOverflow)[0]
	if gap.RequestID != "req_overflow_3" {
		t.Fatalf("expected gap for dropped request, got %q", gap.RequestID)
	}
	if gap.ConversationID != "conv_overflow" {
		t.Fatalf("expected gap to carry conversation id, got %q", gap.ConversationID)
	}
	if !strings.Contains(gap.Detail, "request_bytes=") || !strings.Contains(gap.Detail, "response_bytes=") {
		t.Fatalf("expected gap detail to carry sizes, got %q", gap.Detail)
	}
}

func TestRecorderRecordsStoreFailureGap(t *testing.T) {
	sink := &gapSink{}
	recorder := capture.NewRecorder(failingStore{}, sink, 8, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	recorder.Submit(&capture.Payload{
		RequestID:       "req_store_fail",
		ConversationID:  "conv_store_fail",
		RequestBody:     []byte(`{"messages":[]}`),
		ResponseBody:    []byte("partial"),
		MaxPayloadBytes: 0, // no truncation: only the store failure should mark
	})

	waitFor(t, func() bool { return len(sink.byReason(capture.GapReasonStoreFailure)) > 0 })
	gap := sink.byReason(capture.GapReasonStoreFailure)[0]
	if gap.RequestID != "req_store_fail" {
		t.Fatalf("expected gap for failed request, got %q", gap.RequestID)
	}
	if !strings.Contains(gap.Detail, "error=write failed") {
		t.Fatalf("expected gap detail to carry the store error, got %q", gap.Detail)
	}
	if len(sink.byReason(capture.GapReasonRequestTruncated)) != 0 || len(sink.byReason(capture.GapReasonResponseTruncated)) != 0 {
		t.Fatal("expected no truncation gaps for an untruncated payload")
	}
}

func TestRecorderRecordsTruncationGapsWithSizes(t *testing.T) {
	sink := &gapSink{}
	store := recordingStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, sink, 8, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	recorder.Submit(&capture.Payload{
		RequestID:       "req_trunc",
		RequestBody:     []byte(strings.Repeat("a", 100)),
		ResponseBody:    []byte(strings.Repeat("b", 50)),
		MaxPayloadBytes: 40,
	})

	select {
	case payload := <-store.ch:
		if !payload.RequestTruncated || !payload.ResponseTruncated {
			t.Fatal("expected both truncation flags on the stored payload")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for captured payload")
	}
	waitFor(t, func() bool {
		return len(sink.byReason(capture.GapReasonRequestTruncated)) > 0 &&
			len(sink.byReason(capture.GapReasonResponseTruncated)) > 0
	})
	reqGap := sink.byReason(capture.GapReasonRequestTruncated)[0]
	respGap := sink.byReason(capture.GapReasonResponseTruncated)[0]
	if !strings.Contains(reqGap.Detail, "original_bytes=100") || !strings.Contains(reqGap.Detail, "limit=40") {
		t.Fatalf("expected request gap detail with sizes, got %q", reqGap.Detail)
	}
	if !strings.Contains(respGap.Detail, "original_bytes=50") {
		t.Fatalf("expected response gap detail with sizes, got %q", respGap.Detail)
	}
}

func TestRecorderStoresFullBodiesWhenUnlimited(t *testing.T) {
	sink := &gapSink{}
	store := recordingStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, sink, 8, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	big := strings.Repeat("x", 11*1024*1024) // larger than the old 10 MiB default
	recorder.Submit(&capture.Payload{
		RequestID:       "req_full",
		RequestBody:     []byte(big),
		MaxPayloadBytes: 0, // unlimited — the new default
	})

	select {
	case payload := <-store.ch:
		if len(payload.RequestBody) != len(big) {
			t.Fatalf("expected full request body (%d bytes), got %d", len(big), len(payload.RequestBody))
		}
		if payload.RequestTruncated {
			t.Fatal("expected no truncation flag with unlimited capture")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for captured payload")
	}
	if n := len(sink.gaps); n != 0 {
		t.Fatalf("expected no gap markers for unlimited capture, got %d", n)
	}
}

func TestRecorderRecordGapForwardsToSink(t *testing.T) {
	sink := &gapSink{}
	recorder := capture.NewRecorder(noopStore{}, sink, 8, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	if !recorder.RecordGap(capture.Gap{Reason: capture.GapReasonResponseTruncated}) {
		t.Fatal("expected RecordGap to accept the marker")
	}
	waitFor(t, func() bool { return len(sink.gaps) == 1 })
	if sink.gaps[0].Reason != capture.GapReasonResponseTruncated {
		t.Fatalf("unexpected marker: %+v", sink.gaps[0])
	}
}

func TestRecorderRecordGapNilSafe(t *testing.T) {
	recorder := capture.NewRecorder(noopStore{}, nil, 8, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())
	if recorder.RecordGap(capture.Gap{Reason: "x"}) {
		t.Fatal("expected RecordGap to be a no-op without a sink")
	}

	// Nil recorder must also be safe (dataplane calls it through Services).
	var nilRecorder *capture.Recorder
	if nilRecorder.RecordGap(capture.Gap{Reason: "x"}) {
		t.Fatal("expected nil recorder RecordGap to fail closed")
	}
}

// recordingGapStore collects persisted batches.
type recordingGapStore struct {
	mu    sync.Mutex
	gaps  []*capture.Gap
	calls int
}

func (s *recordingGapStore) PutGaps(_ context.Context, gaps []*capture.Gap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.gaps = append(s.gaps, gaps...)
	return nil
}

func TestGapRecorderFillsIDsAndCapsDetail(t *testing.T) {
	store := &recordingGapStore{}
	rec := capture.NewGapRecorder(store, 8, nil, zerolog.Nop())

	if !rec.Record(capture.Gap{Reason: "x", Detail: strings.Repeat("d", 10_000)}) {
		t.Fatal("expected Record to accept the marker")
	}
	rec.Close(context.Background())

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.gaps) != 1 {
		t.Fatalf("expected one persisted gap, got %d", len(store.gaps))
	}
	gap := store.gaps[0]
	if gap.ID == "" || gap.CreatedAt.IsZero() {
		t.Fatal("expected gap id and created_at to be filled by the recorder")
	}
	if got := len([]rune(gap.Detail)); got > 513 {
		t.Fatalf("expected detail capped to ~512 runes, got %d", got)
	}
}

func TestGapRecorderPersistsBatchesAndDrops(t *testing.T) {
	store := &recordingGapStore{}
	// Queue large enough for the burst: markers are best-effort and a full
	// queue legitimately drops, which the overflow-recorder test covers.
	rec := capture.NewGapRecorder(store, 512, nil, zerolog.Nop())

	for i := 0; i < 300; i++ {
		rec.Record(capture.Gap{Reason: capture.GapReasonQueueOverflow, RequestID: fmt.Sprintf("req_%d", i)})
	}
	rec.Close(context.Background())

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.gaps) != 300 {
		t.Fatalf("expected all 300 markers persisted through Close, got %d", len(store.gaps))
	}
	if rec.Written() != 300 {
		t.Fatalf("expected written counter 300, got %d", rec.Written())
	}
	if rec.Dropped() != 0 {
		t.Fatalf("expected no drops with a large queue, got %d", rec.Dropped())
	}
}

func TestGapRecorderNilSafe(t *testing.T) {
	var rec *capture.GapRecorder
	if rec.Record(capture.Gap{Reason: "x"}) {
		t.Fatal("expected nil recorder to fail closed")
	}
	rec.Close(context.Background()) // must not panic

	// A recorder without a store accepts nothing rather than pretending.
	noStoreRec := capture.NewGapRecorder(nil, 8, nil, zerolog.Nop())
	if noStoreRec.Record(capture.Gap{Reason: "x"}) {
		t.Fatal("expected recorder without store to reject markers")
	}
	noStoreRec.Close(context.Background())
}

// gatedStore blocks each Put until its gate channel is closed, and signals
// arrival (once) so tests can deterministically fill the recorder queue.
type gatedStore struct {
	gate    chan struct{}
	arrived chan struct{}
	once    sync.Once
}

func (g *gatedStore) Put(_ context.Context, _ *capture.Payload) error {
	g.once.Do(func() { close(g.arrived) })
	<-g.gate
	return nil
}

func (g *gatedStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, nil
}
