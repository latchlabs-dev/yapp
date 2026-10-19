// Package metrics provides Prometheus metrics registration and exposition.
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry wraps a prometheus.Registry with convenience methods.
type Registry struct {
	*prometheus.Registry
	mu sync.Mutex
}

// NewRegistry creates a new custom Prometheus registry.
func NewRegistry() *Registry {
	return &Registry{
		Registry: prometheus.NewRegistry(),
	}
}

// MustRegister registers a collector, panicking on error.
func (r *Registry) MustRegister(cs ...prometheus.Collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range cs {
		r.Registry.MustRegister(c)
	}
}

// Handler returns an HTTP handler that serves metrics in Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// RegisterDefault registers Go runtime and process metrics.
func (r *Registry) RegisterDefault() {
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// BuildInfoMetric returns a gauge with build metadata.
func (r *Registry) BuildInfoMetric(version, commit, goVersion string) {
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "corp_proxy_build_info",
		Help: "Build information for the proxy service.",
		ConstLabels: prometheus.Labels{
			"version":    version,
			"commit":     commit,
			"go_version": goVersion,
		},
	})
	info.Set(1)
	r.MustRegister(info)
}
