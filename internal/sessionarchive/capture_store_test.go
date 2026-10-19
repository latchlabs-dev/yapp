package sessionarchive

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
)

type recordingPayloadStore struct {
	mu   sync.Mutex
	puts []*capture.Payload
	err  error
}

func (r *recordingPayloadStore) Put(_ context.Context, p *capture.Payload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.puts = append(r.puts, p)
	return nil
}

func (r *recordingPayloadStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, errors.New("not used")
}

type countingMetrics struct {
	applied map[string]int
	failed  int
}

func (m *countingMetrics) TranscriptApplied(kind string) { m.applied[kind]++ }
func (m *countingMetrics) TranscriptApplyFailed()        { m.failed++ }

func TestCaptureStoreAppliesAndNeverFailsOnTranscriptErrors(t *testing.T) {
	ctx := context.Background()
	c := &conversation{}
	c.user("hi")

	raw := &recordingPayloadStore{}
	states := NewMemoryStateStore()
	metrics := &countingMetrics{applied: map[string]int{}}
	good := NewCaptureStore(raw, NewBuilder(blob.NewMemory(), states, nil, BuilderOptions{}), zerolog.Nop(), metrics)
	if err := good.Put(ctx, c.request("a", false)); err != nil {
		t.Fatal(err)
	}
	if len(raw.puts) != 1 || metrics.applied[string(KindExtend)] != 1 {
		t.Fatalf("puts=%d applied=%v", len(raw.puts), metrics.applied)
	}

	broken := &failingObjects{Memory: blob.NewMemory(), fail: true}
	failingStates := NewMemoryStateStore()
	bad := NewCaptureStore(raw, NewBuilder(broken, failingStates, nil, BuilderOptions{}), zerolog.Nop(), metrics)
	p := c.request("b", false)
	if err := bad.Put(ctx, p); err != nil {
		t.Fatalf("transcript failure must not fail the capture: %v", err)
	}
	st, _ := failingStates.Get(ctx, testTenant, testConv)
	if metrics.failed != 1 || st == nil || !st.NeedsRebuild || st.LastError == "" {
		t.Fatalf("failure not flagged: failed=%d state=%+v", metrics.failed, st)
	}

	raw.err = errors.New("db down")
	if err := good.Put(ctx, c.request("c", false)); err == nil {
		t.Fatal("raw capture failures must fail the write")
	}
	if metrics.applied[string(KindExtend)] != 1 {
		t.Fatal("a capture that was not stored must not be applied")
	}

	rawOnly := NewCaptureStore(&recordingPayloadStore{}, nil, zerolog.Nop(), nil)
	if err := rawOnly.Put(ctx, c.request("d", false)); err != nil {
		t.Fatal(err)
	}
}
