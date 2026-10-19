package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// RecorderMetrics tracks the health of the async write pipelines (usage events
// and conversation capture). These pipelines are best-effort: under overload
// they drop records silently, which previously had no observability. The
// `pipeline` label distinguishes "usage" from "capture".
//
// All methods are safe to call on a nil receiver.
type RecorderMetrics struct {
	written    *prometheus.CounterVec
	dropped    *prometheus.CounterVec
	failed     *prometheus.CounterVec
	queueDepth *prometheus.GaugeVec
}

// NewRecorderMetrics creates and registers async recorder pipeline metrics.
func NewRecorderMetrics(reg *Registry) *RecorderMetrics {
	m := &RecorderMetrics{
		written: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_recorder_written_total",
				Help: "Records successfully persisted by an async write pipeline.",
			},
			[]string{"pipeline"},
		),
		dropped: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_recorder_dropped_total",
				Help: "Records dropped before storage because the pipeline queue was full.",
			},
			[]string{"pipeline"},
		),
		failed: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_recorder_failed_total",
				Help: "Records that reached the worker but failed to persist to the backend.",
			},
			[]string{"pipeline"},
		),
		queueDepth: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "corp_proxy_recorder_queue_depth",
				Help: "Current number of records buffered in an async pipeline queue.",
			},
			[]string{"pipeline"},
		),
	}
	reg.MustRegister(m.written, m.dropped, m.failed, m.queueDepth)
	return m
}

// IncWritten records a successful backend write. Nil-safe.
func (m *RecorderMetrics) IncWritten(pipeline string) {
	if m == nil {
		return
	}
	m.written.WithLabelValues(pipeline).Inc()
}

// IncDropped records a queue-full drop. Nil-safe.
func (m *RecorderMetrics) IncDropped(pipeline string) {
	if m == nil {
		return
	}
	m.dropped.WithLabelValues(pipeline).Inc()
}

// IncFailed records a backend write failure. Nil-safe.
func (m *RecorderMetrics) IncFailed(pipeline string) {
	if m == nil {
		return
	}
	m.failed.WithLabelValues(pipeline).Inc()
}

// SetQueueDepth reports the current queue depth for a pipeline. Nil-safe.
func (m *RecorderMetrics) SetQueueDepth(pipeline string, depth int) {
	if m == nil {
		return
	}
	m.queueDepth.WithLabelValues(pipeline).Set(float64(depth))
}
