package dataplane

import (
	"context"
	stderrors "errors"
	"net/http"

	"github.com/earlgrey/piproxy/internal/stream"
)

const (
	// StatusClientClosedRequest mirrors nginx's non-standard 499 status for
	// requests where the downstream client closed the connection first.
	StatusClientClosedRequest = 499
	ErrorClassClientClosed    = "client_closed"
)

// IsClientClosed reports whether err represents downstream request
// cancellation or a broken client connection, rather than an upstream failure.
func IsClientClosed(r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if stderrors.Is(err, context.Canceled) || stream.IsClientClosed(err) {
		return true
	}
	if r != nil && r.Context().Err() == context.Canceled {
		return true
	}
	return false
}

// WriteClientClosed writes a best-effort HTTP-style 499 response. In real
// disconnects the bytes usually go nowhere, but this keeps pre-header
// cancellations observable in handler tests and proxy logs.
func WriteClientClosed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(StatusClientClosedRequest)
	_, _ = w.Write([]byte(`{"error":{"type":"client_closed","message":"client closed request"}}`))
}
