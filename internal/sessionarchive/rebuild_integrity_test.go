package sessionarchive

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
)

type missingRebuildSource struct {
	*memoryPayloads
	missing string
}

type blockedCleanupObjects struct {
	*blob.Memory
	deletes int
}

func (o *blockedCleanupObjects) Delete(ctx context.Context, _ string) error {
	o.deletes++
	<-ctx.Done()
	return ctx.Err()
}
func TestRebuildCleanupCannotBlockRepairQueueAfterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := &blockedCleanupObjects{Memory: blob.NewMemory()}
		builder := NewBuilder(objects, NewMemoryStateStore(), nil, BuilderOptions{})
		start := time.Now()
		builder.deleteBestEffort(context.Background(), []string{"temporary-one", "temporary-two"})
		if time.Since(start) != 5*time.Second || objects.deletes != 1 {
			t.Fatalf("cleanup exceeded budget: %s deletes=%d", time.Since(start), objects.deletes)
		}
	})
}

func (s missingRebuildSource) LoadPayload(ctx context.Context, id string) (*capture.Payload, error) {
	if id == s.missing {
		return nil, blob.ErrNotFound
	}
	return s.memoryPayloads.LoadPayload(ctx, id)
}
func TestMissingSourceCannotReplaceGenerationOrClearRepairFlag(t *testing.T) {
	h := newHarness(t, 1000)
	c := &conversation{}
	c.user("earlier")
	old := c.request("old", false)
	h.payloads.add(old)
	h.capture(c.request("current", false))
	original := h.state()
	keys := h.objects.Keys()
	h.builder.payloads = missingRebuildSource{h.payloads, old.RequestID}
	if _, err := h.builder.Rebuild(context.Background(), testTenant, testConv); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing source accepted: %v", err)
	}
	current := h.state()
	if !current.NeedsRebuild || current.Generation != original.Generation || current.LastSeq != original.LastSeq || !reflect.DeepEqual(keys, h.objects.Keys()) {
		t.Fatalf("failed rebuild changed state: %+v", current)
	}
	h.builder.payloads = h.payloads
	rebuilt, err := h.builder.Rebuild(context.Background(), testTenant, testConv)
	if err != nil || rebuilt.NeedsRebuild || rebuilt.RequestCount != 2 {
		t.Fatalf("restored source must recover: %+v %v", rebuilt, err)
	}
}
