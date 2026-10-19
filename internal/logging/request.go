// Package logging provides structured JSON logging with request-scoped context.
package logging

import (
	"time"

	"github.com/rs/zerolog"
)

// Fields holds structured log fields for a request.
type Fields struct {
	RequestID      string
	Method         string
	Path           string
	RemoteAddr     string
	TenantID       string
	UserID         string
	KeyPrefix      string
	Provider       string
	Model          string
	UpstreamURL    string
	Status         int
	UpstreamStatus int
	DurationMs     int64
	Streaming      bool
	ErrorClass     string
	Tokens         int64
	CostCents      float64
}

// WithFields returns an event with the given fields attached.
func WithFields(evt *zerolog.Event, f Fields) *zerolog.Event {
	if f.RequestID != "" {
		evt = evt.Str("request_id", f.RequestID)
	}
	if f.Method != "" {
		evt = evt.Str("method", f.Method)
	}
	if f.Path != "" {
		evt = evt.Str("path", f.Path)
	}
	if f.RemoteAddr != "" {
		evt = evt.Str("remote", Redact(f.RemoteAddr))
	}
	if f.TenantID != "" {
		evt = evt.Str("tenant_id", f.TenantID)
	}
	if f.UserID != "" {
		evt = evt.Str("user_id", f.UserID)
	}
	if f.KeyPrefix != "" {
		evt = evt.Str("key_prefix", f.KeyPrefix)
	}
	if f.Provider != "" {
		evt = evt.Str("provider", f.Provider)
	}
	if f.Model != "" {
		evt = evt.Str("model", f.Model)
	}
	if f.UpstreamURL != "" {
		evt = evt.Str("upstream_url", Redact(f.UpstreamURL))
	}
	if f.Status != 0 {
		evt = evt.Int("status", f.Status)
	}
	if f.UpstreamStatus != 0 {
		evt = evt.Int("upstream_status", f.UpstreamStatus)
	}
	if f.DurationMs != 0 {
		evt = evt.Int64("duration_ms", f.DurationMs)
	}
	if f.Streaming {
		evt = evt.Bool("streaming", true)
	}
	if f.ErrorClass != "" {
		// Use "error_class" (matching the field name everywhere else in the
		// codebase) so it never collides with zerolog's own "error" key set by
		// .Err(); a collision produces a duplicate JSON key and drops the real
		// error message in strict log parsers.
		evt = evt.Str("error_class", f.ErrorClass)
	}
	if f.Tokens != 0 {
		evt = evt.Int64("tokens", f.Tokens)
	}
	if f.CostCents != 0 {
		evt = evt.Float64("cost_cents", f.CostCents)
	}
	return evt
}

// RequestEvent logs a structured request lifecycle event on the given logger.
func RequestEvent(logger zerolog.Logger, message string, fields Fields) {
	evt := logger.Info()
	if fields.ErrorClass != "" {
		evt = logger.Warn()
	}
	if fields.Status >= 500 {
		evt = logger.Error()
	}
	WithFields(evt, fields).Msg(message)
}

// RequestError logs a request error with structured context. The real error
// (if any) is attached via zerolog's .Err() as the "error" key; callers may set
// fields.ErrorClass to add a classification ("error_class") alongside it.
func RequestError(logger zerolog.Logger, message string, err error, fields Fields) {
	WithFields(logger.Error().Err(err), fields).Msg(message)
}

// RequestWarn logs a request warning with structured context and an optional
// error value. Use it for expected client/network outcomes that should not page
// as server errors.
func RequestWarn(logger zerolog.Logger, message string, err error, fields Fields) {
	WithFields(logger.Warn().Err(err), fields).Msg(message)
}

// Timing records the start time of a request.
type Timing struct {
	start time.Time
}

// StartTiming begins a timing measurement.
func StartTiming() Timing {
	return Timing{start: time.Now()}
}

// Elapsed returns the duration in milliseconds since StartTiming was called.
func (t Timing) Elapsed() int64 {
	return time.Since(t.start).Milliseconds()
}

// StartedAt returns the timestamp captured when timing began.
func (t Timing) StartedAt() time.Time {
	return t.start
}
