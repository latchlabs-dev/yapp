package hooks

import (
	"context"
	"sync"
)

type outputReviewContextKey struct{}

type outputReviewState struct {
	mu    sync.Mutex
	event *Event
}

// WithOutputReview marks a request whose caller owns synchronous post-send
// review and subsequent async publication. Delegated handlers must not publish
// the draft themselves; usage, capture and quota accounting remain unchanged.
func WithOutputReview(ctx context.Context) context.Context {
	return context.WithValue(ctx, outputReviewContextKey{}, &outputReviewState{})
}

func HasOutputReview(ctx context.Context) bool {
	state, _ := ctx.Value(outputReviewContextKey{}).(*outputReviewState)
	return state != nil
}

// RecordOutputReviewEvent retains the delegated pipeline's completed metadata
// for the caller's approved publication. This keeps cost/timing/usage hooks
// authoritative in the same place as CLI traffic, without publishing a draft.
func RecordOutputReviewEvent(ctx context.Context, ev *Event) {
	state, _ := ctx.Value(outputReviewContextKey{}).(*outputReviewState)
	if state == nil || ev == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	copy := *ev
	copy.Content = nil
	state.event = &copy
}

func OutputReviewEvent(ctx context.Context) *Event {
	state, _ := ctx.Value(outputReviewContextKey{}).(*outputReviewState)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.event == nil {
		return nil
	}
	copy := *state.event
	return &copy
}

// EmitOutputReview runs synchronous review without queueing an unreviewed
// event to observers. EmitReviewedOutput publishes the resulting safe event
// exactly once after its decision has been applied.
func (m *Manager) EmitOutputReview(ctx context.Context, ev *Event) (*Decision, error) {
	return m.emitBlocking(ctx, ev, false)
}

func (m *Manager) EmitReviewedOutput(ev *Event) {
	if m == nil || ev == nil || !m.hasAsyncDestinations() {
		return
	}
	normalize(ev)
	m.enqueue(ev, false)
}
