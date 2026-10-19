package capture

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/metrics"
)

// Gap reasons. They describe why conversation data is missing or partial for
// a point in time, so a session can be audited for replayability.
const (
	// GapReasonQueueOverflow marks a payload dropped because the capture
	// queue was full at Submit time. The payload never reached storage.
	GapReasonQueueOverflow = "queue_overflow"
	// GapReasonStoreFailure marks a payload that was dequeued but whose
	// storage write failed.
	GapReasonStoreFailure = "store_failure"
	// GapReasonRequestTruncated marks a stored payload whose request body
	// was cut at the configured ceiling.
	GapReasonRequestTruncated = "request_truncated"
	// GapReasonResponseTruncated marks a stored payload whose response body
	// (or SSE transcript) was cut at the configured ceiling.
	GapReasonResponseTruncated = "response_truncated"
)

// gapPipelineName labels gap-recorder metrics in the shared recorder set.
const gapPipelineName = "capture_gaps"

// Gap is a durable marker that conversation data was dropped or truncated at
// a point in time. Gaps are tiny (no bodies) and are kept forever: they are
// the audit trail that says "a capture that existed here is missing", which
// is exactly the information retention sweeps must not delete.
type Gap struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id,omitempty"`
	TenantID       string    `json:"tenant_id,omitempty"`
	UserID         string    `json:"user_id,omitempty"`
	RequestID      string    `json:"request_id,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Model          string    `json:"model,omitempty"`
	Streaming      bool      `json:"streaming"`
	Reason         string    `json:"reason"`
	Detail         string    `json:"detail,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// GapSink accepts gap markers without blocking. Implementations must be
// safe for concurrent use and nil-safe.
type GapSink interface {
	Record(gap Gap) bool
}

// GapStore is the pluggable backend for gap markers.
type GapStore interface {
	PutGaps(ctx context.Context, gaps []*Gap) error
}

const (
	gapDefaultQueueSize = 4096
	gapMaxBatch         = 128
	gapFlushInterval    = 500 * time.Millisecond
	gapDetailMaxRunes   = 512
)

// GapRecorder persists gap markers asynchronously through its own tiny queue,
// deliberately independent of the payload recorder: when the payload queue
// overflows, the marker for that drop must not compete with payload writes
// (or be lost to the same full queue). Batches are flushed on size or a
// ticker so a slow trickle of gaps does not hold rows in memory indefinitely.
type GapRecorder struct {
	store   GapStore
	logger  zerolog.Logger
	metrics *metrics.RecorderMetrics
	queue   chan Gap
	wg      sync.WaitGroup

	// sendMu serializes Record's non-blocking send with Close's channel
	// close, mirroring Recorder's shutdown discipline.
	sendMu    sync.RWMutex
	stopped   atomic.Bool
	closeOnce sync.Once

	drainCtx atomic.Pointer[context.Context]

	dropped atomic.Int64
	failed  atomic.Int64
	written atomic.Int64
}

// NewGapRecorder creates and starts the async gap recorder. queueSize falls
// back to a sane default when non-positive. metrics may be nil.
func NewGapRecorder(store GapStore, queueSize int, recorderMetrics *metrics.RecorderMetrics, logger zerolog.Logger) *GapRecorder {
	if queueSize <= 0 {
		queueSize = gapDefaultQueueSize
	}
	g := &GapRecorder{
		store:   store,
		logger:  logger,
		metrics: recorderMetrics,
		queue:   make(chan Gap, queueSize),
	}
	g.wg.Add(1)
	go g.run()
	return g
}

// Record enqueues a gap marker without blocking. It returns false if the
// marker was dropped (recorder stopped or marker queue full). ID and
// CreatedAt are filled when empty.
func (g *GapRecorder) Record(gap Gap) bool {
	if g == nil || g.store == nil {
		return false
	}
	if gap.ID == "" {
		gap.ID = id.New("gap")
	}
	if gap.CreatedAt.IsZero() {
		gap.CreatedAt = time.Now().UTC()
	}
	if gap.Detail != "" {
		gap.Detail = truncateRunes(gap.Detail, gapDetailMaxRunes)
	}
	if g.stopped.Load() {
		return false
	}
	g.sendMu.RLock()
	defer g.sendMu.RUnlock()
	if g.stopped.Load() {
		return false
	}
	select {
	case g.queue <- gap:
		g.metrics.SetQueueDepth(gapPipelineName, len(g.queue))
		return true
	default:
		dropped := g.dropped.Add(1)
		g.metrics.IncDropped(gapPipelineName)
		g.logger.Warn().
			Str("reason", gap.Reason).
			Str("request_id", gap.RequestID).
			Int64("dropped_total", dropped).
			Msg("capture gap marker dropped; marker queue full")
		return false
	}
}

// Close stops accepting markers, then drains everything buffered. The
// context bounds the final flush; pass context.Background() for unbounded.
func (g *GapRecorder) Close(ctx context.Context) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.closeOnce.Do(func() {
		g.drainCtx.Store(&ctx)
		g.sendMu.Lock()
		defer g.sendMu.Unlock()
		g.stopped.Store(true)
		close(g.queue)
	})
	g.wg.Wait()
}

// Written returns the count of gap markers persisted.
func (g *GapRecorder) Written() int64 {
	if g == nil {
		return 0
	}
	return g.written.Load()
}

// Dropped returns the count of gap markers dropped before storage.
func (g *GapRecorder) Dropped() int64 {
	if g == nil {
		return 0
	}
	return g.dropped.Load()
}

func (g *GapRecorder) run() {
	defer g.wg.Done()
	ticker := time.NewTicker(gapFlushInterval)
	defer ticker.Stop()
	batch := make([]*Gap, 0, gapMaxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := g.store.PutGaps(g.putContext(), batch); err != nil {
			failed := g.failed.Add(int64(len(batch)))
			g.metrics.IncFailed(gapPipelineName)
			g.logger.Error().
				Err(err).
				Int("batch_size", len(batch)).
				Int64("failed_total", failed).
				Msg("capture gap marker write failed")
		} else {
			g.written.Add(int64(len(batch)))
			g.metrics.IncWritten(gapPipelineName)
		}
		batch = batch[:0]
	}
	for {
		select {
		case gap, ok := <-g.queue:
			if !ok {
				flush()
				return
			}
			gp := gap // copy: channel value; keep a stable pointer
			batch = append(batch, &gp)
			g.metrics.SetQueueDepth(gapPipelineName, len(g.queue))
			if len(batch) >= gapMaxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (g *GapRecorder) putContext() context.Context {
	if ctx := g.drainCtx.Load(); ctx != nil {
		return *ctx
	}
	return context.Background()
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
