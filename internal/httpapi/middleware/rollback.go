package middleware

import (
	"context"
	"sync"
)

// rollbackCtxKey is the context key under which a quota-rollback closure is
// stashed by provider handlers immediately after a reservation is taken, so the
// Recover middleware can release it if the handler panics. It reuses the
// package-private contextKey type declared in request_id.go.
const rollbackCtxKey contextKey = "quota_rollback"

// rollbackHolder makes the stored closure consume-once. The Recover middleware
// and a normal Complete/explicit-Rollback path must never double-fire it.
type rollbackHolder struct {
	mu   sync.Mutex
	once sync.Once
	fn   func()
}

func (h *rollbackHolder) set(fn func()) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fn = fn
}

func (h *rollbackHolder) run() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.mu.Lock()
		fn := h.fn
		h.mu.Unlock()
		if fn != nil {
			fn()
		}
	})
}

// WithRollbackHolder returns a context with a shared rollback holder. Recover
// installs this before dispatch so a handler can later populate the holder via
// WithRollback even though it only reassigns its local request pointer.
func WithRollbackHolder(ctx context.Context) context.Context {
	if _, ok := ctx.Value(rollbackCtxKey).(*rollbackHolder); ok {
		return ctx
	}
	return context.WithValue(ctx, rollbackCtxKey, &rollbackHolder{})
}

// WithRollback returns a context carrying a consume-once rollback closure.
// Handlers call this right after CheckAndReserve/CheckQuota succeeds and use the
// returned context (and request) for the remainder of the handler:
//
//	r = r.WithContext(middleware.WithRollback(r.Context(), func() {
//		dataplane.RollbackQuota(r, &h.services, resv)
//	}))
//
// Passing fn == nil is a no-op that returns ctx unchanged.
func WithRollback(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	if h, ok := ctx.Value(rollbackCtxKey).(*rollbackHolder); ok {
		h.set(fn)
		return ctx
	}
	return context.WithValue(ctx, rollbackCtxKey, &rollbackHolder{fn: fn})
}

// ConsumeRollback runs the registered rollback closure exactly once (if any) and
// is safe to call from the success path too — the sync.Once makes it a no-op on
// any subsequent call. Handlers SHOULD NOT normally call this; the underlying
// Enforcer.Rollback is already idempotent via resv.released, so the consume-once
// guard plus released flag give defense in depth.
func ConsumeRollback(ctx context.Context) {
	if h, ok := ctx.Value(rollbackCtxKey).(*rollbackHolder); ok {
		h.run()
	}
}

// RollbackFromContext returns the consume-once rollback func, or nil if none was
// registered. The Recover middleware calls it as:
//
//	if rb := middleware.RollbackFromContext(r.Context()); rb != nil { rb() }
//
// The returned func is the holder's run method, so invoking it more than once
// (or after the handler already rolled back/completed) is a safe no-op.
func RollbackFromContext(ctx context.Context) func() {
	if h, ok := ctx.Value(rollbackCtxKey).(*rollbackHolder); ok {
		return h.run
	}
	return nil
}
