package capture

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/metrics"
)

// pipelineName labels this recorder's metrics in the shared recorder metric set.
const pipelineName = "capture"

// Recorder writes captures asynchronously. Submit is intentionally best-effort
// so payload storage does not sit on the proxy hot path. A pool of workers
// drains the queue concurrently so a slow gzip+DB write on one payload does not
// stall the others.
type Recorder struct {
	store   PayloadStore
	gaps    GapSink
	logger  zerolog.Logger
	metrics *metrics.RecorderMetrics
	queue   chan *Payload
	wg      sync.WaitGroup

	// sendMu serializes Submit's non-blocking send with Close's channel close.
	// stopped makes late producers drop captures after shutdown begins.
	sendMu    sync.RWMutex
	stopped   atomic.Bool
	closeOnce sync.Once

	// drainCtx bounds the store writes performed while draining the queue on
	// shutdown. It is installed by Close so a shutdown deadline caps the flush.
	drainCtx atomic.Pointer[context.Context]

	dropped atomic.Int64
	failed  atomic.Int64
	written atomic.Int64
}

// NewRecorder creates and starts an async capture recorder with `workers`
// concurrent queue drainers. Both queueSize and workers fall back to sane
// defaults when non-positive. metrics may be nil (all metric calls are no-ops).
// gaps may be nil; when set, drop and truncation markers are submitted to it
// whenever a payload fails to reach storage intact.
func NewRecorder(store PayloadStore, gaps GapSink, queueSize, workers int, recorderMetrics *metrics.RecorderMetrics, logger zerolog.Logger) *Recorder {
	if queueSize <= 0 {
		queueSize = 2000
	}
	if workers <= 0 {
		workers = 4
	}
	r := &Recorder{
		store:   store,
		gaps:    gaps,
		logger:  logger,
		metrics: recorderMetrics,
		queue:   make(chan *Payload, queueSize),
	}
	for i := 0; i < workers; i++ {
		r.wg.Add(1)
		go r.run()
	}
	return r
}

// Submit enqueues a payload without blocking. It returns false if the capture
// was dropped because the recorder is unavailable or overloaded.
func (r *Recorder) Submit(payload *Payload) bool {
	if r == nil || r.store == nil || payload == nil {
		return false
	}
	// Reject submissions once shutdown has begun. This both avoids a
	// send-on-closed-channel panic and signals the caller the capture was not
	// accepted.
	if r.stopped.Load() {
		return false
	}
	r.sendMu.RLock()
	defer r.sendMu.RUnlock()
	if r.stopped.Load() {
		return false
	}
	select {
	case r.queue <- payload:
		r.metrics.SetQueueDepth(pipelineName, len(r.queue))
		return true
	default:
		dropped := r.dropped.Add(1)
		r.metrics.IncDropped(pipelineName)
		r.metrics.SetQueueDepth(pipelineName, len(r.queue))
		r.logger.Warn().
			Str("request_id", payload.RequestID).
			Int("queue_size", cap(r.queue)).
			Int64("dropped_total", dropped).
			Msg("conversation capture queue full; dropping payload")
		r.recordGap(Gap{
			ConversationID: payload.ConversationID,
			TenantID:       payload.TenantID,
			UserID:         payload.UserID,
			RequestID:      payload.RequestID,
			Provider:       payload.Provider,
			Model:          payload.Model,
			Streaming:      payload.Streaming,
			Reason:         GapReasonQueueOverflow,
			Detail:         fmt.Sprintf("queue_size=%d request_bytes=%d response_bytes=%d", cap(r.queue), len(payload.RequestBody), len(payload.ResponseBody)),
		})
		return false
	}
}

// RecordGap forwards a data-plane-originated gap marker (for example an SSE
// transcript that hit the capture ceiling mid-relay) to the gap sink. It is
// nil-safe and never blocks.
func (r *Recorder) RecordGap(gap Gap) bool {
	if r == nil {
		return false
	}
	return r.recordGap(gap)
}

func (r *Recorder) recordGap(gap Gap) bool {
	if r.gaps == nil {
		return false
	}
	return r.gaps.Record(gap)
}

// Close stops accepting new submissions, then drains every payload already
// buffered in the queue before returning. The provided context bounds the
// store writes performed during the drain so a shutdown deadline caps the
// flush; pass context.Background() for an unbounded drain. Close is safe to
// call more than once (subsequent calls are no-ops).
func (r *Recorder) Close(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeOnce.Do(func() {
		// Install the drain context before stopping intake so workers picking
		// up the final buffered payloads use the bounded context.
		r.drainCtx.Store(&ctx)
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		// stopped guards Submit so no producer sends after the channel closes.
		r.stopped.Store(true)
		// Closing the queue lets each worker's `for range` drain the buffer to
		// completion and then exit.
		close(r.queue)
	})
	r.wg.Wait()
}

// Dropped returns the count of payloads dropped before storage.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// Failed returns the count of backend writes that failed.
func (r *Recorder) Failed() int64 {
	if r == nil {
		return 0
	}
	return r.failed.Load()
}

func (r *Recorder) run() {
	defer r.wg.Done()
	// Ranging over the queue drains every buffered payload before the loop
	// ends: the loop only terminates once Close has closed the channel AND the
	// buffer is empty, so no buffered capture is dropped on shutdown.
	for payload := range r.queue {
		prepared := preparePayloadForStorage(payload)
		r.metrics.SetQueueDepth(pipelineName, len(r.queue))
		if err := r.store.Put(r.putContext(), prepared.payload); err != nil {
			failed := r.failed.Add(1)
			r.metrics.IncFailed(pipelineName)
			r.logger.Error().
				Err(err).
				Str("request_id", payload.RequestID).
				Int64("failed_total", failed).
				Msg("conversation capture write failed")
			r.recordGap(Gap{
				ConversationID: prepared.payload.ConversationID,
				TenantID:       prepared.payload.TenantID,
				UserID:         prepared.payload.UserID,
				RequestID:      prepared.payload.RequestID,
				Provider:       prepared.payload.Provider,
				Model:          prepared.payload.Model,
				Streaming:      prepared.payload.Streaming,
				Reason:         GapReasonStoreFailure,
				Detail:         fmt.Sprintf("error=%s request_bytes=%d response_bytes=%d", err.Error(), prepared.origRequestBytes, prepared.origResponseBytes),
			})
			continue
		}
		// Only fresh truncation applied by this worker is marked here. Flags
		// set upstream (SSE relay ceilings) carry their own markers from the
		// data plane, and double-marking would overstate the loss.
		if prepared.requestTruncated {
			r.recordGap(Gap{
				ConversationID: prepared.payload.ConversationID,
				TenantID:       prepared.payload.TenantID,
				UserID:         prepared.payload.UserID,
				RequestID:      prepared.payload.RequestID,
				Provider:       prepared.payload.Provider,
				Model:          prepared.payload.Model,
				Streaming:      prepared.payload.Streaming,
				Reason:         GapReasonRequestTruncated,
				Detail:         fmt.Sprintf("limit=%d original_bytes=%d", prepared.payload.requestLimit, prepared.origRequestBytes),
			})
		}
		if prepared.responseTruncated {
			r.recordGap(Gap{
				ConversationID: prepared.payload.ConversationID,
				TenantID:       prepared.payload.TenantID,
				UserID:         prepared.payload.UserID,
				RequestID:      prepared.payload.RequestID,
				Provider:       prepared.payload.Provider,
				Model:          prepared.payload.Model,
				Streaming:      prepared.payload.Streaming,
				Reason:         GapReasonResponseTruncated,
				Detail:         fmt.Sprintf("limit=%d original_bytes=%d", prepared.payload.requestLimit, prepared.origResponseBytes),
			})
		}
		r.written.Add(1)
		r.metrics.IncWritten(pipelineName)
	}
}

// preparedPayload is the worker-side result of preparePayloadForStorage.
type preparedPayload struct {
	payload *Payload
	// orig*Bytes are the pre-truncation body lengths.
	origRequestBytes  int64
	origResponseBytes int64
	// requestTruncated/responseTruncated record truncation freshly applied
	// by this worker, as opposed to flags already set on the payload by the
	// data plane (SSE relay ceilings).
	requestTruncated  bool
	responseTruncated bool
}

// preparePayloadForStorage fills a missing conversation id from the request
// body and applies the configured payload ceiling, if any. A limit <= 0 means
// no truncation: bodies are stored in full (request bodies are already
// bounded by the body-size middleware and buffered responses by
// ReadUpstreamBody).
func preparePayloadForStorage(payload *Payload) *preparedPayload {
	if payload == nil {
		return &preparedPayload{}
	}
	prepared := *payload
	if prepared.ConversationID == "" {
		prepared.ConversationID = ExtractConversationID(nil, prepared.RequestBody)
	}

	limit := prepared.MaxPayloadBytes
	prepared.requestLimit = limit
	reqBody, reqTruncated := LimitBytes(prepared.RequestBody, limit)
	respBody, respTruncated := LimitBytes(prepared.ResponseBody, limit)
	prepared.RequestBody = reqBody
	prepared.ResponseBody = respBody
	prepared.RequestTruncated = prepared.RequestTruncated || reqTruncated
	prepared.ResponseTruncated = prepared.ResponseTruncated || respTruncated
	prepared.MaxPayloadBytes = 0
	return &preparedPayload{
		payload:           &prepared,
		origRequestBytes:  int64(len(payload.RequestBody)),
		origResponseBytes: int64(len(payload.ResponseBody)),
		requestTruncated:  reqTruncated,
		responseTruncated: respTruncated,
	}
}

// putContext returns the context used for a store write. During normal
// operation this is context.Background(); once Close installs a drain context
// the bounded shutdown context is used so the flush respects the deadline.
func (r *Recorder) putContext() context.Context {
	if ctx := r.drainCtx.Load(); ctx != nil {
		return *ctx
	}
	return context.Background()
}
