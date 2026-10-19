package audit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/audit"
)

func TestNewLogger(t *testing.T) {
	l := audit.NewLogger(100)
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestLoggerCloseConcurrentLog(t *testing.T) {
	l := audit.NewLogger(8)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 2000; j++ {
				l.Log(&audit.Event{
					ActorType:  "system",
					ActorID:    "sys",
					Action:     audit.ActionProxyRequest,
					TargetType: "request",
					Result:     "success",
				})
			}
		}()
	}

	close(start)
	l.Close()
	wg.Wait()
}

func TestLogEvent(t *testing.T) {
	l := audit.NewLogger(10)
	l.Log(&audit.Event{
		ActorType:  "user",
		ActorID:    "usr_123",
		Action:     audit.ActionKeyCreated,
		TargetType: "api_key",
		TargetID:   "key_456",
		Result:     "success",
	})

	select {
	case event := <-l.Events():
		if event.Action != audit.ActionKeyCreated {
			t.Errorf("expected 'key.created', got %q", event.Action)
		}
		if event.CreatedAt.IsZero() {
			t.Error("expected non-zero timestamp")
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestLogf(t *testing.T) {
	l := audit.NewLogger(10)
	l.Logf("system", "sys_001", audit.ActionAuthFailure, "request", "req_789", "denied", "req_789")

	select {
	case event := <-l.Events():
		if event.Action != audit.ActionAuthFailure {
			t.Errorf("expected 'proxy.auth_failure', got %q", event.Action)
		}
		if event.RequestID != "req_789" {
			t.Errorf("expected 'req_789', got %q", event.RequestID)
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestLoggerBufferFull(t *testing.T) {
	l := audit.NewLogger(1)
	// Fill the buffer.
	l.Log(&audit.Event{Action: "event1"})
	// This should not block.
	l.Log(&audit.Event{Action: "event2"})

	time.Sleep(10 * time.Millisecond)

	select {
	case <-l.Events():
		// Got one event.
	default:
	}
}

func TestActionConstants(t *testing.T) {
	actions := []string{
		audit.ActionTenantCreated,
		audit.ActionUserCreated,
		audit.ActionKeyRevoked,
		audit.ActionAdminLogin,
		audit.ActionProxyRequest,
		audit.ActionQuotaExceeded,
	}

	for _, action := range actions {
		if action == "" {
			t.Error("action constant should not be empty")
		}
	}
}
