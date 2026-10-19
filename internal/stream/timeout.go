package stream

import (
	"context"
	"time"
)

// NewStreamTimeout creates a context that times out after timeout.
// A non-positive timeout returns a cancellable child context without a deadline.
func NewStreamTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
