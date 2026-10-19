package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// QuotaMetrics collects quota-related Prometheus metrics.
// All methods are safe to call on a nil receiver.
type QuotaMetrics struct {
	requestsTotal *prometheus.CounterVec
}

// NewQuotaMetrics creates and registers quota metrics.
func NewQuotaMetrics(reg *Registry) *QuotaMetrics {
	m := &QuotaMetrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_quota_requests_total",
				Help: "Total number of quota check results by scope type and decision.",
			},
			[]string{"scope_type", "decision"},
		),
	}
	reg.MustRegister(m.requestsTotal)
	return m
}

// ObserveQuota records a quota check result. Nil-safe.
func (m *QuotaMetrics) ObserveQuota(scopeType, decision string) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(scopeType, decision).Inc()
}
