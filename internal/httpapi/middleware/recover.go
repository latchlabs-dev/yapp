package middleware

import (
	"context"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/errors"
	"github.com/earlgrey/piproxy/internal/logging"
)

type recoveryLogStateKey struct{}

// recoveryLogState is created by the outer Recover middleware and populated
// by RequestLogging. The shared pointer survives stack unwinding, unlike the
// inner request value returned by r.WithContext, so a recovered handler panic
// still has the request-scoped logger and start time.
type recoveryLogState struct {
	mu     sync.Mutex
	logger zerolog.Logger
	start  time.Time
	set    bool
}

func withRecoveryLogState(ctx context.Context) context.Context {
	return context.WithValue(ctx, recoveryLogStateKey{}, &recoveryLogState{})
}

func setRecoveryLogger(ctx context.Context, logger zerolog.Logger, start time.Time) {
	state, ok := ctx.Value(recoveryLogStateKey{}).(*recoveryLogState)
	if !ok || state == nil {
		return
	}
	state.mu.Lock()
	state.logger = logger
	state.start = start
	state.set = true
	state.mu.Unlock()
}

func getRecoveryLogger(ctx context.Context) (zerolog.Logger, time.Time, bool) {
	state, ok := ctx.Value(recoveryLogStateKey{}).(*recoveryLogState)
	if !ok || state == nil {
		return zerolog.Nop(), time.Time{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.logger, state.start, state.set
}

// recordingWriter wraps an http.ResponseWriter and records whether any part of
// the response (status line or body) has been written. The Recover middleware
// uses the wrote flag to decide whether it is still safe to emit a 500 after a
// panic: once the client has seen bytes, a second WriteHeader would only produce
// a "superfluous WriteHeader" warning and corrupt the response.
//
// Flush and Unwrap are passed through so SSE streaming (which relies on
// http.Flusher) and any downstream http.Hijacker/Flusher unwrapping keep working
// when this writer sits at the top of the chain.
type recordingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *recordingWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's Flusher so streaming handlers can
// still push buffered bytes to the client.
func (w *recordingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer so net/http's ResponseController and any
// Hijacker/Flusher lookups can reach the real ResponseWriter.
func (w *recordingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Recover is the outermost middleware: it recovers any panic raised by a
// downstream handler or middleware, releases a quota reservation registered in
// the request context, logs the panic with the request id and a stack trace, and
// writes a structured 500 if the response has not started yet. It must be
// registered FIRST (outermost) in the router so it wraps everything.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithRollbackHolder(r.Context())
		ctx = withRecoveryLogState(ctx)
		r = r.WithContext(ctx)
		rw := &recordingWriter{ResponseWriter: w}
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is a deliberate abort signal; re-panic so
			// net/http handles it with its documented semantics instead of
			// swallowing it into a 500.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			// Release any quota/spend reservation stashed by the handler so a
			// panic mid-request does not leak a reserved amount. The closure is
			// consume-once, so this is a no-op if the success path already ran it.
			if rb := RollbackFromContext(r.Context()); rb != nil {
				rb()
			}

			// Log at error level with the request id and a stack trace. The
			// stack is attached directly because logging.Fields has no panic or
			// stack field.
			reqID := GetRequestID(r.Context())
			log := GetRequestLogger(r.Context())
			started := time.Time{}
			if recoveredLog, recoveredStart, ok := getRecoveryLogger(r.Context()); ok {
				log = recoveredLog
				started = recoveredStart
			}
			fields := logging.Fields{
				RequestID:  reqID,
				Status:     http.StatusInternalServerError,
				ErrorClass: "panic",
			}
			if !started.IsZero() {
				fields.DurationMs = time.Since(started).Milliseconds()
			}
			logging.WithFields(log.Error(), fields).
				Interface("panic", rec).
				Bytes("stack", debug.Stack()).
				Msg("request.panic")

			// Only write a 500 if nothing has been sent yet; otherwise the
			// headers/body are already on the wire and a second WriteHeader
			// would be ignored with a superfluous-WriteHeader warning.
			if !rw.wrote {
				errors.WriteErrorJSON(rw, errors.ErrorInternal, "internal server error")
			}
		}()
		next.ServeHTTP(rw, r)
	})
}
