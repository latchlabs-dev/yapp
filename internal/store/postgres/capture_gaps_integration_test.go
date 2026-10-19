package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
)

func TestCaptureGapStoreRoundTrip(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	store := NewCaptureGapStore(pool)

	convID := "conv-gap-" + id.New("t")
	gaps := []*capture.Gap{
		{
			ID:             id.New("gap"),
			ConversationID: convID,
			TenantID:       "tn_test",
			RequestID:      "req_gap_1",
			Provider:       "deepseek",
			Model:          "deepseek-chat",
			Streaming:      true,
			Reason:         capture.GapReasonQueueOverflow,
			Detail:         "queue_size=2000 request_bytes=100 response_bytes=0",
			CreatedAt:      time.Now().UTC(),
		},
		{
			ID:             id.New("gap"),
			ConversationID: convID,
			RequestID:      "req_gap_2",
			Reason:         capture.GapReasonResponseTruncated,
			Detail:         "limit=10485760 original_bytes=99999999",
			CreatedAt:      time.Now().UTC().Add(time.Second),
		},
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM capture_gaps WHERE conversation_id=$1`, convID)
	})

	if err := store.PutGaps(ctx, gaps); err != nil {
		t.Fatalf("PutGaps: %v", err)
	}
	if err := store.PutGaps(ctx, nil); err != nil {
		t.Fatalf("PutGaps(nil) must be a no-op: %v", err)
	}

	listed, total, err := store.List(ctx, convID, "", "", 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 || len(listed) != 2 {
		t.Fatalf("expected 2 gaps, got total=%d len=%d", total, len(listed))
	}
	// Newest first.
	if listed[0].RequestID != "req_gap_2" || listed[1].RequestID != "req_gap_1" {
		t.Fatalf("unexpected order: [%s %s]", listed[0].RequestID, listed[1].RequestID)
	}
	if listed[1].Streaming != true || listed[1].Provider != "deepseek" {
		t.Fatalf("round-trip lost fields: %+v", listed[1])
	}
	if listed[1].TenantID != "tn_test" {
		t.Fatalf("expected tenant id preserved, got %q", listed[1].TenantID)
	}

	// Reason filter narrows.
	onlyOverflow, total, err := store.List(ctx, convID, "", capture.GapReasonQueueOverflow, 50, 0)
	if err != nil || total != 1 || len(onlyOverflow) != 1 {
		t.Fatalf("reason filter: total=%d len=%d err=%v", total, len(onlyOverflow), err)
	}
}

func TestCaptureGapStoreNilPool(t *testing.T) {
	store := NewCaptureGapStore(nil)
	if err := store.PutGaps(context.Background(), []*capture.Gap{{ID: "g", Reason: "x"}}); err == nil {
		t.Fatal("expected error without a pool")
	}
	if _, _, err := store.List(context.Background(), "", "", "", 10, 0); err == nil {
		t.Fatal("expected list error without a pool")
	}
	if err := store.PutGaps(context.Background(), nil); err != nil {
		t.Fatalf("empty batch must be a no-op even without a pool: %v", err)
	}
}
