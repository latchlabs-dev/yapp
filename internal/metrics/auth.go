// Package metrics provides Prometheus metrics for authentication, quota, and usage.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// AuthMetrics collects authentication-related Prometheus metrics.
// All methods are safe to call on a nil receiver.
type AuthMetrics struct {
	requestsTotal *prometheus.CounterVec
}

// NewAuthMetrics creates and registers auth metrics.
func NewAuthMetrics(reg *Registry) *AuthMetrics {
	c := &AuthMetrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_auth_requests_total",
				Help: "Total number of authentication requests by result.",
			},
			[]string{"result"},
		),
	}
	reg.MustRegister(c.requestsTotal)
	return c
}

// ObserveAuth records an auth request result. Nil-safe.
func (m *AuthMetrics) ObserveAuth(result string) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(result).Inc()
}
