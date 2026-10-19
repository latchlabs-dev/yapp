package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// UsageMetrics collects usage accounting Prometheus metrics.
// All methods are safe to call on a nil receiver.
type UsageMetrics struct {
	tokensTotal   *prometheus.CounterVec
	costTotal     *prometheus.CounterVec
	requestsTotal *prometheus.CounterVec
}

// NewUsageMetrics creates and registers usage metrics.
func NewUsageMetrics(reg *Registry) *UsageMetrics {
	m := &UsageMetrics{
		tokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_usage_tokens_total",
				Help: "Total tokens consumed by model and direction (input/output).",
			},
			[]string{"model", "direction"},
		),
		costTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_usage_cost_total",
				Help: "Total cost in USD by model.",
			},
			[]string{"model"},
		),
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_usage_requests_total",
				Help: "Total usage-tracked requests by model and provider.",
			},
			[]string{"model", "provider"},
		),
	}
	reg.MustRegister(m.tokensTotal, m.costTotal, m.requestsTotal)
	return m
}

// ObserveTokens records token counts. Nil-safe.
func (m *UsageMetrics) ObserveTokens(model, direction string, count int) {
	if m == nil {
		return
	}
	m.tokensTotal.WithLabelValues(model, direction).Add(float64(count))
}

// ObserveCost records cost. Nil-safe.
func (m *UsageMetrics) ObserveCost(model string, cost float64) {
	if m == nil {
		return
	}
	m.costTotal.WithLabelValues(model).Add(cost)
}

// ObserveRequest records a usage-tracked request. Nil-safe.
func (m *UsageMetrics) ObserveRequest(model, provider string) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(model, provider).Inc()
}
