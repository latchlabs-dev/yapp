package middleware

import (
	"context"
	"errors"
	"testing"
)

// panickingUpdater panics on its first call, then succeeds. It records each
// keyID it was asked to update so the test can confirm the worker survived the
// panic and processed the next update.
type panickingUpdater struct {
	calls     []string
	panicOnce bool
}

func (p *panickingUpdater) UpdateLastUsed(_ context.Context, keyID string) error {
	p.calls = append(p.calls, keyID)
	if !p.panicOnce {
		p.panicOnce = true
		panic("simulated store panic")
	}
	return nil
}

// TestUpdateLoop_SurvivesPanic verifies that a panic inside UpdateLastUsed is
// recovered per-iteration so the worker keeps processing subsequent keyIDs.
// KeyStore is concrete, so we exercise the extracted processUpdate helper
// directly with a panicking stub injected via the lastUsedUpdater seam.
func TestUpdateLoop_SurvivesPanic(t *testing.T) {
	stub := &panickingUpdater{}
	m := &AuthMiddleware{lastUsed: stub}

	// First call panics internally; processUpdate must recover and return.
	m.processUpdate("key-1")
	// Second call must still run — proves the goroutine would not have died.
	m.processUpdate("key-2")

	if len(stub.calls) != 2 {
		t.Fatalf("expected both updates attempted, got %v", stub.calls)
	}
	if stub.calls[0] != "key-1" || stub.calls[1] != "key-2" {
		t.Fatalf("unexpected call order: %v", stub.calls)
	}
}

// erroringUpdater returns an error (no panic) to confirm the normal error path
// is swallowed as before.
type erroringUpdater struct{ called bool }

func (e *erroringUpdater) UpdateLastUsed(_ context.Context, _ string) error {
	e.called = true
	return errors.New("transient db error")
}

func TestProcessUpdate_ErrorIsIgnored(t *testing.T) {
	stub := &erroringUpdater{}
	m := &AuthMiddleware{lastUsed: stub}
	m.processUpdate("key-err") // must not panic
	if !stub.called {
		t.Fatal("expected UpdateLastUsed to be called")
	}
}
