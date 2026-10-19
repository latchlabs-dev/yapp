package sessionarchive

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/capture"
)

// Metrics observes capture-time transcript building. Implementations must be
// nil-safe; a nil Metrics disables observation.
type Metrics interface {
	TranscriptApplied(kind string)
	TranscriptApplyFailed()
}

// CaptureStore is the capture.PayloadStore the recorder writes through: it
// stores the raw capture, then appends it to the conversation transcript.
// Raw storage failures fail the write; transcript failures never do — they
// flag the conversation for rebuild, because the raw capture is the source
// of truth and the transcript is derived from it.
type CaptureStore struct {
	payloads capture.PayloadStore
	builder  *Builder
	logger   zerolog.Logger
	metrics  Metrics
}

var _ capture.PayloadStore = (*CaptureStore)(nil)

// NewCaptureStore wraps payloads. builder may be nil to store raw captures
// only.
func NewCaptureStore(payloads capture.PayloadStore, builder *Builder, logger zerolog.Logger, metrics Metrics) *CaptureStore {
	return &CaptureStore{payloads: payloads, builder: builder, logger: logger, metrics: metrics}
}

// Put implements capture.PayloadStore.
func (s *CaptureStore) Put(ctx context.Context, p *capture.Payload) error {
	if err := s.payloads.Put(ctx, p); err != nil {
		return err
	}
	if s.builder == nil {
		return nil
	}
	turn, err := s.builder.Apply(ctx, p)
	if err != nil {
		if s.metrics != nil {
			s.metrics.TranscriptApplyFailed()
		}
		s.logger.Warn().Err(err).
			Str("request_id", p.RequestID).
			Str("conversation_id", p.ConversationID).
			Msg("transcript append failed; conversation flagged for rebuild")
		cause := truncateBytes(err.Error(), errorTextMaxBytes)
		if markErr := s.builder.states.MarkNeedsRebuild(context.WithoutCancel(ctx), p, cause); markErr != nil {
			s.logger.Error().Err(markErr).
				Str("request_id", p.RequestID).
				Str("conversation_id", p.ConversationID).
				Msg("failed to flag transcript for rebuild")
		}
		return nil
	}
	if turn != nil && s.metrics != nil {
		s.metrics.TranscriptApplied(string(turn.Kind))
	}
	return nil
}

// Get implements capture.PayloadStore.
func (s *CaptureStore) Get(ctx context.Context, requestID string) (*capture.Payload, error) {
	return s.payloads.Get(ctx, requestID)
}
