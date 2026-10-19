package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/hooks"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeSessionDistillationQueue struct {
	identity pgstore.SessionIdentity
	err      error
	allowed  map[string]bool
}

func (f *fakeSessionDistillationQueue) AllowsProject(_ context.Context, _, projectID string) (bool, error) {
	return f.allowed[projectID], nil
}

func (f *fakeSessionDistillationQueue) EnqueueSession(_ context.Context, identity pgstore.SessionIdentity) error {
	f.identity = identity
	return f.err
}

type fakeDistillationConsent struct{ enabled bool }

func (f fakeDistillationConsent) TenantDistillationEnabled(context.Context, string) (bool, error) {
	return f.enabled, nil
}

func TestSessionUploadedHookQueuesNightlyDistillation(t *testing.T) {
	queue := &fakeSessionDistillationQueue{allowed: map[string]bool{"mprj-demo": true}}
	handler := NewSessionUploadedHook(queue, fakeDistillationConsent{enabled: true}, zerolog.Nop())
	decision, err := handler.Handle(context.Background(), &hooks.Event{
		Type:           hooks.EventSessionUploaded,
		ConversationID: "conv-upload",
		TenantID:       "tenant-1",
		UserID:         "user-1",
		KeyID:          "key-1",
		Project:        "demo",
		ProjectID:      "mprj-demo",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if decision != nil {
		t.Fatalf("decision = %+v, want nil", decision)
	}
	if queue.identity.ConversationID != "conv-upload" || queue.identity.OwnerID != "user:user-1" {
		t.Fatalf("queued identity = %+v", queue.identity)
	}
}

func TestSessionUploadedHookSkipsDisallowedProject(t *testing.T) {
	queue := &fakeSessionDistillationQueue{allowed: map[string]bool{"mprj-demo": true}}
	handler := NewSessionUploadedHook(queue, fakeDistillationConsent{enabled: true}, zerolog.Nop())
	_, err := handler.Handle(context.Background(), &hooks.Event{
		Type:           hooks.EventSessionUploaded,
		ConversationID: "conv-upload",
		TenantID:       "tenant-1",
		UserID:         "user-1",
		KeyID:          "key-1",
		Project:        "other",
		ProjectID:      "mprj-other",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if queue.identity.ConversationID != "" {
		t.Fatalf("unexpected queued identity = %+v", queue.identity)
	}
}

func TestSessionUploadedHookReturnsQueueError(t *testing.T) {
	queue := &fakeSessionDistillationQueue{err: errors.New("queue down"), allowed: map[string]bool{"mprj-demo": true}}
	handler := NewSessionUploadedHook(queue, fakeDistillationConsent{enabled: true}, zerolog.Nop())
	_, err := handler.Handle(context.Background(), &hooks.Event{
		Type:           hooks.EventSessionUploaded,
		ConversationID: "conv-upload",
		TenantID:       "tenant-1",
		UserID:         "user-1",
		KeyID:          "key-1",
		ProjectID:      "mprj-demo",
	})
	if err == nil {
		t.Fatal("expected queue error")
	}
}
