// Package audit provides audit event logging for the proxy.
package audit

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Event represents a single audit event.
type Event struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id,omitempty"`
	ActorType    string          `json:"actor_type"` // "user", "admin", "system"
	ActorID      string          `json:"actor_id"`
	Action       string          `json:"action"`
	TargetType   string          `json:"target_type"`
	TargetID     string          `json:"target_id,omitempty"`
	Result       string          `json:"result"` // "success", "denied", "error"
	RedactedDiff json.RawMessage `json:"redacted_diff,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Logger handles writing audit events.
type Logger struct {
	events chan *Event

	// sendMu serializes Log's non-blocking send with Close's channel close.
	// stopped makes late producers drop events after shutdown begins.
	sendMu    sync.RWMutex
	stopped   atomic.Bool
	closeOnce sync.Once
}

// NewLogger creates a new audit logger with a buffered channel.
func NewLogger(bufferSize int) *Logger {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	return &Logger{
		events: make(chan *Event, bufferSize),
	}
}

// Log queues an audit event for writing.
func (l *Logger) Log(event *Event) {
	// Reject events once shutdown has begun so we never send on a closed
	// channel.
	if l.stopped.Load() {
		return
	}
	event.CreatedAt = time.Now()

	l.sendMu.RLock()
	defer l.sendMu.RUnlock()
	if l.stopped.Load() {
		return
	}

	select {
	case l.events <- event:
	default:
		// Channel full, drop event.
	}
}

// Logf is a convenience method to log a structured event.
func (l *Logger) Logf(actorType, actorID, action, targetType, targetID, result, requestID string) {
	l.Log(&Event{
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Result:     result,
		RequestID:  requestID,
	})
}

// Events returns the event channel for processing.
func (l *Logger) Events() <-chan *Event {
	return l.events
}

// Close stops accepting new events and closes the events channel so a consumer
// ranging over Events() drains the remaining buffered events and then exits. It
// is safe to call more than once. Close does not wait for the consumer; the
// caller is expected to wait on the consumer goroutine (e.g. via a WaitGroup)
// after calling Close.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.closeOnce.Do(func() {
		l.sendMu.Lock()
		defer l.sendMu.Unlock()
		l.stopped.Store(true)
		close(l.events)
	})
}

// Action constants for audit events.
const (
	ActionTenantCreated     = "tenant.created"
	ActionTenantUpdated     = "tenant.updated"
	ActionUserCreated       = "user.created"
	ActionUserDisabled      = "user.disabled"
	ActionKeyCreated        = "key.created"
	ActionKeyRevoked        = "key.revoked"
	ActionQuotaCreated      = "quota.created"
	ActionQuotaUpdated      = "quota.updated"
	ActionProviderCreated   = "provider.created"
	ActionProviderUpdated   = "provider.updated"
	ActionCredentialCreated = "provider_credential.created"
	ActionCredentialRotated = "provider_credential.rotated"
	ActionCredentialRevoked = "provider_credential.revoked"
	ActionAliasCreated      = "model_alias.created"
	ActionAliasUpdated      = "model_alias.updated"
	ActionAdminLogin        = "admin.login"
	ActionPermissionDenied  = "admin.permission_denied"
	ActionCacheEnabled      = "cache.enabled"
	ActionCacheDisabled     = "cache.disabled"
	ActionProxyRequest      = "proxy.request"
	ActionQuotaExceeded     = "proxy.quota_exceeded"
	ActionAuthFailure       = "proxy.auth_failure"
)
