// Package audit provides SIEM-format export for audit events.
package audit

import (
	"encoding/json"
	"fmt"
	"time"
)

// SIEMEvent is the canonical audit event format for SIEM ingestion.
// Compatible with Common Event Format (CEF) and JSON-based SIEMs.
type SIEMEvent struct {
	Version      string    `json:"version"`
	EventTime    time.Time `json:"event_time"`
	EventType    string    `json:"event_type"` // "piproxy_audit"
	Action       string    `json:"action"`
	ActorType    string    `json:"actor_type"`
	ActorID      string    `json:"actor_id"`
	TargetType   string    `json:"target_type"`
	TargetID     string    `json:"target_id"`
	Result       string    `json:"result"`
	TenantID     string    `json:"tenant_id,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	SourceIP     string    `json:"source_ip,omitempty"`
	RedactedDiff string    `json:"redacted_diff,omitempty"`
	Severity     string    `json:"severity"` // INFO, WARN, CRIT
}

// ToSIEM converts an audit Event to SIEM format.
func (e *Event) ToSIEM() *SIEMEvent {
	severity := "INFO"
	switch e.Result {
	case "denied":
		severity = "WARN"
	case "error":
		severity = "WARN"
	}

	return &SIEMEvent{
		Version:    "1.0",
		EventTime:  e.CreatedAt,
		EventType:  "piproxy_audit",
		Action:     e.Action,
		ActorType:  e.ActorType,
		ActorID:    e.ActorID,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Result:     e.Result,
		TenantID:   e.TenantID,
		RequestID:  e.RequestID,
		Severity:   severity,
	}
}

// ToSIEMJSON serializes the event to JSON for SIEM ingestion.
func (e *Event) ToSIEMJSON() ([]byte, error) {
	siem := e.ToSIEM()
	data, err := json.Marshal(siem)
	if err != nil {
		return nil, fmt.Errorf("marshal SIEM event: %w", err)
	}
	return data, nil
}

// NewSIEMWriter creates a channel consumer that serializes audit events
// to SIEM JSON and writes them to the provided output function.
func NewSIEMWriter(events <-chan *Event, writeFn func([]byte) error) {
	go func() {
		for event := range events {
			data, err := event.ToSIEMJSON()
			if err != nil {
				continue
			}
			if err := writeFn(data); err != nil {
				_ = err // SIEM write failure is best-effort
			}
		}
	}()
}
