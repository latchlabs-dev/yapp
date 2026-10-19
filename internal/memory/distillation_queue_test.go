package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeDistillationStore struct {
	record           *pgstore.SessionSubmissionRecord
	getErr           error
	undistilledPages [][]*pgstore.SessionSubmissionRecord
	listAfters       []string
	attempts         int
	lastErr          string
	marked           map[string]string
}

func (f *fakeDistillationStore) GetByIdentity(context.Context, pgstore.SessionIdentity) (*pgstore.SessionSubmissionRecord, []byte, error) {
	if f.getErr != nil {
		return nil, nil, f.getErr
	}
	return f.record, nil, nil
}

func (f *fakeDistillationStore) ListUndistilledSessions(_ context.Context, after string, _ int) ([]*pgstore.SessionSubmissionRecord, error) {
	f.listAfters = append(f.listAfters, after)
	if len(f.undistilledPages) == 0 {
		return nil, nil
	}
	page := f.undistilledPages[0]
	f.undistilledPages = f.undistilledPages[1:]
	return page, nil
}

func (f *fakeDistillationStore) RecordDistillAttemptFor(context.Context, pgstore.SessionIdentity) error {
	f.attempts++
	return nil
}

func (f *fakeDistillationStore) RecordDistillErrorFor(_ context.Context, _ pgstore.SessionIdentity, distillErr string) error {
	f.lastErr = distillErr
	return nil
}

func (f *fakeDistillationStore) MarkDistilledFor(_ context.Context, identity pgstore.SessionIdentity, distillErr string) error {
	if f.marked == nil {
		f.marked = map[string]string{}
	}
	f.marked[identity.ConversationID] = distillErr
	return nil
}

func (f *fakeDistillationStore) TenantDistillationEnabled(context.Context, string) (bool, error) {
	return true, nil
}

func TestNextNightlyAt(t *testing.T) {
	nightly, err := parseClockUTC("23:00", false)
	if err != nil {
		t.Fatalf("parse nightly time: %v", err)
	}
	cases := map[string]string{
		"2026-07-10T22:59:00Z": "2026-07-10T23:00:00Z",
		"2026-07-10T23:00:00Z": "2026-07-11T23:00:00Z",
		"2026-07-10T23:01:00Z": "2026-07-11T23:00:00Z",
	}
	for input, want := range cases {
		got := nextNightlyAt(mustUTC(t, input), nightly)
		if !got.Equal(mustUTC(t, want)) {
			t.Errorf("nextNightlyAt(%s) = %s, want %s", input, got.Format(time.RFC3339), want)
		}
	}
}

func TestDistillationQueueRunTaskSynthesizesOnce(t *testing.T) {
	endedAt := mustUTC(t, "2026-07-10T22:00:00Z")
	record := testSessionRecord("conv-nightly", "demo")
	record.EndedAt = &endedAt
	store := &fakeDistillationStore{record: record}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMText(`{"observations":[{"kind":"lesson","content":"nightly synthesis worked"}]}`)
	journal := &fakeJournal{}
	distiller := NewDistiller(store, logs, llm, journal, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	now := mustUTC(t, "2026-07-10T23:05:00Z")
	queue := &DistillationQueue{
		distiller: distiller,
		store:     store,
		allowed:   map[string]struct{}{"demo": {}},
		now:       func() time.Time { return now },
	}

	result, err := queue.runTask(context.Background(), DistillationTaskParams{
		TenantID:       "tenant-1",
		OwnerID:        "user:user-1",
		ConversationID: "conv-nightly",
		ScheduledFor:   mustUTC(t, "2026-07-10T23:00:00Z"),
	})
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if result.ConversationID != "conv-nightly" || result.Project != "demo" || result.AlreadyDistilled {
		t.Fatalf("result = %+v", result)
	}
	if store.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", store.attempts)
	}
	if len(llm.requests) != 1 || len(journal.appends["projects/tenant-1--demo/lessons.md"]) != 1 {
		t.Fatalf("LLM requests = %d, journal = %v", len(llm.requests), journal.appends)
	}
	if store.marked["conv-nightly"] != "" {
		t.Fatalf("distilled marker = %q", store.marked["conv-nightly"])
	}
}

func TestDistillationQueueRunTaskSkipsAlreadyDistilled(t *testing.T) {
	distilledAt := mustUTC(t, "2026-07-10T22:00:00Z")
	record := testSessionRecord("conv-done", "demo")
	record.DistilledAt = &distilledAt
	store := &fakeDistillationStore{record: record}
	llm := newMockLLM()
	distiller := NewDistiller(store, &fakeSessionLogs{}, llm, &fakeJournal{}, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	queue := &DistillationQueue{
		distiller: distiller,
		store:     store,
		allowed:   map[string]struct{}{"demo": {}},
		now:       func() time.Time { return mustUTC(t, "2026-07-10T23:05:00Z") },
	}

	result, err := queue.runTask(context.Background(), DistillationTaskParams{
		TenantID:       "tenant-1",
		OwnerID:        "user:user-1",
		ConversationID: "conv-done",
		ScheduledFor:   mustUTC(t, "2026-07-10T23:00:00Z"),
	})
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if !result.AlreadyDistilled || store.attempts != 0 || len(llm.requests) != 0 {
		t.Fatalf("result = %+v, attempts = %d, LLM requests = %d", result, store.attempts, len(llm.requests))
	}
}

func TestDistillationQueueUsesDatabaseMatcherWhenAvailableAndAllowsLegacyResolvedProjects(t *testing.T) {
	queue := &DistillationQueue{}
	allowed, err := queue.AllowsProject(context.Background(), "tenant-1", "demo")
	if err != nil || !allowed {
		t.Fatalf("resolved legacy project allowed = %t, err = %v", allowed, err)
	}
	allowed, err = (*DistillationQueue)(nil).AllowsProject(context.Background(), "tenant-1", "demo")
	if err != nil || allowed {
		t.Fatal("nil queue unexpectedly allowed a project")
	}
}

func TestDistillationQueueRecordsRetryableError(t *testing.T) {
	endedAt := mustUTC(t, "2026-07-10T22:00:00Z")
	record := testSessionRecord("conv-retry", "demo")
	record.EndedAt = &endedAt
	store := &fakeDistillationStore{record: record}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMError(errors.New("provider unavailable"))
	distiller := NewDistiller(store, logs, llm, &fakeJournal{}, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	queue := &DistillationQueue{
		distiller: distiller,
		store:     store,
		allowed:   map[string]struct{}{"demo": {}},
		now:       func() time.Time { return mustUTC(t, "2026-07-10T23:05:00Z") },
	}

	_, err := queue.runTask(context.Background(), DistillationTaskParams{
		TenantID:       "tenant-1",
		OwnerID:        "user:user-1",
		ConversationID: "conv-retry",
		ScheduledFor:   mustUTC(t, "2026-07-10T23:00:00Z"),
	})
	if err == nil {
		t.Fatal("expected retryable task error")
	}
	if store.attempts != 1 || store.lastErr == "" {
		t.Fatalf("attempts = %d, lastErr = %q", store.attempts, store.lastErr)
	}
}

func TestDistillationQueueWaitsForSessionInactivityAndRechecks(t *testing.T) {
	endedAt := mustUTC(t, "2026-07-10T22:58:00Z")
	record := testSessionRecord("conv-active", "demo")
	record.EndedAt = &endedAt
	store := &fakeDistillationStore{record: record}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMText(`{"observations":[{"kind":"lesson","content":"waited for the final turn"}]}`)
	distiller := NewDistiller(store, logs, llm, &fakeJournal{}, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	now := mustUTC(t, "2026-07-10T23:00:00Z")
	var wakeTimes []time.Time
	queue := &DistillationQueue{
		distiller: distiller,
		store:     store,
		allowed:   map[string]struct{}{"demo": {}},
		cfg:       DistillationQueueConfig{IdleAfter: 15 * time.Minute},
		now:       func() time.Time { return now },
		sleepUntil: func(_ context.Context, stepName string, wakeAt time.Time) error {
			if !strings.HasPrefix(stepName, "wait-for-session-inactivity:") {
				t.Fatalf("unexpected sleep step %q", stepName)
			}
			wakeTimes = append(wakeTimes, wakeAt)
			now = wakeAt
			if len(wakeTimes) == 1 {
				newActivity := mustUTC(t, "2026-07-10T23:12:00Z")
				store.record.EndedAt = &newActivity
			}
			return nil
		},
	}

	_, err := queue.runTask(context.Background(), DistillationTaskParams{
		TenantID:       "tenant-1",
		OwnerID:        "user:user-1",
		ConversationID: "conv-active",
		ScheduledFor:   mustUTC(t, "2026-07-10T23:00:00Z"),
	})
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	wantWakeTimes := []time.Time{
		mustUTC(t, "2026-07-10T23:13:00Z"),
		mustUTC(t, "2026-07-10T23:27:00Z"),
	}
	if len(wakeTimes) != len(wantWakeTimes) {
		t.Fatalf("wake times = %v, want %v", wakeTimes, wantWakeTimes)
	}
	for i := range wantWakeTimes {
		if !wakeTimes[i].Equal(wantWakeTimes[i]) {
			t.Errorf("wake time %d = %s, want %s", i, wakeTimes[i], wantWakeTimes[i])
		}
	}
	if store.attempts != 1 || len(llm.requests) != 1 {
		t.Fatalf("attempts = %d, LLM requests = %d", store.attempts, len(llm.requests))
	}
}

func TestDistillationQueueRunTaskSkipsDisallowedProject(t *testing.T) {
	record := testSessionRecord("conv-blocked", "other")
	store := &fakeDistillationStore{record: record}
	llm := newMockLLM()
	distiller := NewDistiller(store, &fakeSessionLogs{}, llm, &fakeJournal{}, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	queue := &DistillationQueue{
		distiller: distiller,
		store:     store,
		allowed:   map[string]struct{}{"demo": {}},
		now:       func() time.Time { return mustUTC(t, "2026-07-10T23:05:00Z") },
	}

	result, err := queue.runTask(context.Background(), DistillationTaskParams{
		TenantID:       "tenant-1",
		OwnerID:        "user:user-1",
		ConversationID: "conv-blocked",
		ScheduledFor:   mustUTC(t, "2026-07-10T23:00:00Z"),
	})
	if err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if !result.SkippedDisallowedProject || store.attempts != 0 || len(llm.requests) != 0 {
		t.Fatalf("result = %+v, attempts = %d, LLM requests = %d", result, store.attempts, len(llm.requests))
	}
}

func TestReconcileUndistilledPaginatesAndEnqueuesAllowedSessions(t *testing.T) {
	firstPage := make([]*pgstore.SessionSubmissionRecord, distillationReconcileBatch)
	for i := range firstPage {
		firstPage[i] = testSessionRecord(fmt.Sprintf("conv-%03d", i), "demo")
		firstPage[i].ID = fmt.Sprintf("ses-%03d", i)
	}
	firstPage[50].ProjectID = "blocked"
	last := testSessionRecord("conv-100", "blocked")
	last.ID = "ses-100"
	store := &fakeDistillationStore{undistilledPages: [][]*pgstore.SessionSubmissionRecord{firstPage, {last}}}
	var enqueued []string

	count, err := reconcileUndistilled(context.Background(), store, func(_ context.Context, tenantID, projectID string) (bool, error) {
		return tenantID == "tenant-1" && projectID == "demo", nil
	}, func(_ context.Context, identity pgstore.SessionIdentity) error {
		enqueued = append(enqueued, identity.ConversationID)
		return nil
	})
	if err != nil {
		t.Fatalf("reconcileUndistilled: %v", err)
	}
	if count != 99 || len(enqueued) != 99 {
		t.Fatalf("count = %d, enqueued = %d", count, len(enqueued))
	}
	if len(store.listAfters) != 2 || store.listAfters[0] != "" || store.listAfters[1] != "ses-099" {
		t.Fatalf("pagination cursors = %v", store.listAfters)
	}
}
