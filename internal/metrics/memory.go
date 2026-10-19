package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// MemoryMetrics tracks observability gaps in the context-memory subsystem.
// Today its only concern is degraded context bundles: when the memory backend
// (duffel) is unavailable the bundle silently degrades to empty and the API
// still returns 200, which previously had no metric. The `reason` label
// distinguishes why a bundle was degraded/empty.
//
// All methods are safe to call on a nil receiver.
type MemoryMetrics struct {
	degraded *prometheus.CounterVec
}

// NewMemoryMetrics creates and registers context-memory metrics.
func NewMemoryMetrics(reg *Registry) *MemoryMetrics {
	m := &MemoryMetrics{
		degraded: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_memory_degraded_bundles_total",
				Help: "Context bundles returned degraded/empty because the memory backend (duffel) was unavailable.",
			},
			[]string{"reason"},
		),
	}
	reg.MustRegister(m.degraded)
	return m
}

// IncDegraded records a degraded/empty bundle for the given reason
// (e.g. "unavailable"). Nil-safe.
func (m *MemoryMetrics) IncDegraded(reason string) {
	if m == nil {
		return
	}
	m.degraded.WithLabelValues(reason).Inc()
}
