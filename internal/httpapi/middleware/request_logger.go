package middleware

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/logging"
)

type loggerCtxKey string

const (
	ctxLoggerKey    loggerCtxKey = "logger"
	ctxStartTimeKey loggerCtxKey = "start_time"
	ctxLogFieldsKey loggerCtxKey = "log_fields"
)

type requestLogFields struct {
	mu     sync.Mutex
	fields logging.Fields
}

// RequestLogging attaches a request-scoped logger and logs request outcomes.
func RequestLogging(rootLogger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Build request-scoped logger with fields shared by all events from
			// this request.
			loggerCtx := rootLogger.With().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Str("remote", logging.Redact(r.RemoteAddr))
			if reqID := GetRequestID(r.Context()); reqID != "" {
				loggerCtx = loggerCtx.Str("request_id", reqID)
			}
			reqLogger := loggerCtx.Logger()
			setRecoveryLogger(r.Context(), reqLogger, start)

			ctx := context.WithValue(r.Context(), ctxLoggerKey, reqLogger)
			ctx = context.WithValue(ctx, ctxStartTimeKey, start)
			ctx = context.WithValue(ctx, ctxLogFieldsKey, &requestLogFields{})
			r = r.WithContext(ctx)

			reqLogger.Debug().Msg("request.start")

			// Wrap ResponseWriter to capture status code.
			sw := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}
			next.ServeHTTP(sw, r)

			// Log request completion.
			elapsed := time.Since(start).Milliseconds()
			authCtx := GetAuthContext(r.Context())
			fields := RequestLogFields(r.Context())
			fields.Status = sw.statusCode
			fields.DurationMs = elapsed
			if authCtx != nil {
				fields.TenantID = authCtx.TenantID
				fields.UserID = authCtx.UserID
				fields.KeyPrefix = authCtx.KeyPrefix
			}

			if sw.statusCode >= 500 {
				logging.RequestError(reqLogger, "request.error", nil, fields)
			} else if sw.statusCode >= 400 {
				logging.RequestEvent(reqLogger, "request.rejected", fields)
			} else if fields.ErrorClass != "" {
				// Streaming transports may return HTTP 200 before an upstream
				// semantic error arrives in-band. Preserve the wire status while
				// recording the request outcome as an error, not a completion.
				logging.RequestEvent(reqLogger, "request.error", fields)
			} else {
				logging.RequestEvent(reqLogger, "request.complete", fields)
			}
		})
	}
}

// AddRequestLogFields stores extra fields for the final request outcome log.
func AddRequestLogFields(ctx context.Context, fields logging.Fields) {
	store, ok := ctx.Value(ctxLogFieldsKey).(*requestLogFields)
	if !ok || store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	mergeLogFields(&store.fields, fields)
}

// RequestLogFields retrieves fields accumulated for the request outcome log.
func RequestLogFields(ctx context.Context) logging.Fields {
	store, ok := ctx.Value(ctxLogFieldsKey).(*requestLogFields)
	if !ok || store == nil {
		return logging.Fields{}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.fields
}

// GetRequestLogger retrieves the request-scoped logger from context.
func GetRequestLogger(ctx context.Context) zerolog.Logger {
	if l, ok := ctx.Value(ctxLoggerKey).(zerolog.Logger); ok {
		return l
	}
	return zerolog.Nop()
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func mergeLogFields(f *logging.Fields, update logging.Fields) {
	if update.RequestID != "" {
		f.RequestID = update.RequestID
	}
	if update.Method != "" {
		f.Method = update.Method
	}
	if update.Path != "" {
		f.Path = update.Path
	}
	if update.RemoteAddr != "" {
		f.RemoteAddr = update.RemoteAddr
	}
	if update.TenantID != "" {
		f.TenantID = update.TenantID
	}
	if update.UserID != "" {
		f.UserID = update.UserID
	}
	if update.KeyPrefix != "" {
		f.KeyPrefix = update.KeyPrefix
	}
	if update.Provider != "" {
		f.Provider = update.Provider
	}
	if update.Model != "" {
		f.Model = update.Model
	}
	if update.UpstreamURL != "" {
		f.UpstreamURL = update.UpstreamURL
	}
	if update.Status != 0 {
		f.Status = update.Status
	}
	if update.UpstreamStatus != 0 {
		f.UpstreamStatus = update.UpstreamStatus
	}
	if update.DurationMs != 0 {
		f.DurationMs = update.DurationMs
	}
	if update.Streaming {
		f.Streaming = true
	}
	if update.ErrorClass != "" {
		f.ErrorClass = update.ErrorClass
	}
	if update.Tokens != 0 {
		f.Tokens = update.Tokens
	}
	if update.CostCents != 0 {
		f.CostCents = update.CostCents
	}
}
