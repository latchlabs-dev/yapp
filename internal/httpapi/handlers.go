// Package httpapi provides HTTP handlers for the proxy service.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/earlgrey/piproxy/internal/config"
)

// HealthzHandler returns a simple liveness check (always 200).
func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// VersionHandler returns build version information.
func VersionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(config.BuildInfo())
}

// ReadyCheck is a function that checks whether all service dependencies
// are healthy. Returns nil if ready, or a descriptive error if not.
type ReadyCheck func() error

// NewReadyzHandler creates a readiness handler that checks dependencies.
// When check is nil, it always returns 200 (shallow check).
func NewReadyzHandler(check ReadyCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			if err := check(); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"status": "not_ready",
					"reason": "dependency check failed",
				})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}
