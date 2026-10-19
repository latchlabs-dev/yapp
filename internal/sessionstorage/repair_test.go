package sessionstorage

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type flaggedReadStates struct{ lists int }

func (s *flaggedReadStates) Get(context.Context, string, string) (*sessionarchive.State, error) {
	return &sessionarchive.State{TranscriptID: "id", Generation: 1, LastSeq: 1, ObjectsLocation: "memory", NeedsRebuild: true}, nil
}
func (s *flaggedReadStates) ListTurns(context.Context, string, int) ([]sessionarchive.Turn, error) {
	s.lists++
	return nil, nil
}
func TestIncompleteTranscriptCannotReadPartialSource(t *testing.T) {
	objects := blob.NewMemory()
	states := &flaggedReadStates{}
	services := &Services{Objects: objects, Reader: sessionarchive.NewReader(objects), logger: zerolog.Nop()}
	page, _, err := services.transcriptPage(context.Background(), states, "tenant", "conversation", 1, 50)
	if !errors.Is(err, sessionarchive.ErrTranscriptRebuilding) || page != nil || states.lists != 0 {
		t.Fatalf("partial read: page=%v lists=%d error=%v", page, states.lists, err)
	}
}
func TestRepairLeaseBlockedRenewalCancelsAtOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		err := withRepairLease(context.Background(), start.Add(30*time.Second), func(ctx context.Context) (time.Time, error) { <-ctx.Done(); return time.Time{}, ctx.Err() }, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
		if !errors.Is(err, pgstore.ErrRepairLeaseLost) || time.Since(start) != 25*time.Second {
			t.Fatalf("deadline moved: %s %v", time.Since(start), err)
		}
	})
}
func TestRepairLeaseOwnerLossCancelsWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		err := withRepairLease(context.Background(), start.Add(2*time.Minute), func(context.Context) (time.Time, error) { return time.Time{}, pgstore.ErrRepairLeaseLost }, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
		if !errors.Is(err, pgstore.ErrRepairLeaseLost) || time.Since(start) != 20*time.Second {
			t.Fatalf("owner loss: %s %v", time.Since(start), err)
		}
	})
}
