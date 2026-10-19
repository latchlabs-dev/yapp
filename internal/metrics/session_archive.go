package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SessionArchiveMetrics tracks capture-time transcript building. A rising
// failure count means conversations are being flagged for rebuild instead of
// appended (object storage or lock trouble); a rising reset share means
// clients are rewriting history (compaction, branches).
//
// All methods are safe to call on a nil receiver.
type SessionArchiveMetrics struct {
	turns    *prometheus.CounterVec
	failures prometheus.Counter
	repairs  *prometheus.CounterVec
	backlog  prometheus.Gauge
	oldest   prometheus.Gauge
}

// NewSessionArchiveMetrics creates and registers session archive metrics.
func NewSessionArchiveMetrics(reg *Registry) *SessionArchiveMetrics {
	m := &SessionArchiveMetrics{
		turns: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "corp_proxy_session_transcript_turns_total",
				Help: "Captured requests appended to session transcripts, by how they related to the stored history.",
			},
			[]string{"kind"},
		),
		failures: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "corp_proxy_session_transcript_apply_failures_total",
				Help: "Captured requests that could not be appended to their transcript and flagged it for rebuild.",
			},
		),
	}
	m.repairs = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "corp_proxy_session_transcript_repairs_total", Help: "Durable transcript repair attempts by outcome."}, []string{"outcome"})
	m.backlog = prometheus.NewGauge(prometheus.GaugeOpts{Name: "corp_proxy_session_transcript_repairs_pending", Help: "Conversations awaiting durable transcript repair."})
	m.oldest = prometheus.NewGauge(prometheus.GaugeOpts{Name: "corp_proxy_session_transcript_repair_oldest_age_seconds", Help: "Age of the oldest pending transcript repair."})
	reg.MustRegister(m.turns, m.failures, m.repairs, m.backlog, m.oldest)
	return m
}

func (m *SessionArchiveMetrics) RepairBacklog(count int64, oldest time.Time) {
	if m == nil {
		return
	}
	m.backlog.Set(float64(count))
	age := 0.0
	if !oldest.IsZero() {
		age = max(time.Since(oldest).Seconds(), 0)
	}
	m.oldest.Set(age)
}
func (m *SessionArchiveMetrics) RepairFinished(reason string) {
	if m == nil {
		return
	}
	if reason == "" {
		reason = "success"
	}
	m.repairs.WithLabelValues(reason).Inc()
}

// TranscriptApplied records one appended turn. Nil-safe.
func (m *SessionArchiveMetrics) TranscriptApplied(kind string) {
	if m == nil {
		return
	}
	m.turns.WithLabelValues(kind).Inc()
}

// TranscriptApplyFailed records one failed append. Nil-safe.
func (m *SessionArchiveMetrics) TranscriptApplyFailed() {
	if m == nil {
		return
	}
	m.failures.Inc()
}
