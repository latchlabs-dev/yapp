package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
	"github.com/earlgrey/piproxy/internal/usage"
)

func seedArchiveTenant(t *testing.T, pool *Pool) string {
	t.Helper()
	ctx := context.Background()
	tenantID := id.New("tn")
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, status, created_at, updated_at) VALUES ($1, 'archive test', 'active', now(), now())`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM session_transcripts WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(ctx, `DELETE FROM conversation_payloads WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(ctx, `DELETE FROM session_objects WHERE key LIKE $1`, "%/"+tenantID+"/%")
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return tenantID
}

// chatCaptures returns n Chat Completions captures of one conversation, each
// resending the full history like Pi does.
func chatCaptures(tenantID, conversationID string, n int, start time.Time) []*capture.Payload {
	messages := []map[string]any{{"role": "user", "content": "please refactor the parser"}}
	var out []*capture.Payload
	for i := 1; i <= n; i++ {
		body, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": messages})
		reply := fmt.Sprintf("step %d", i)
		resp := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\ndata: [DONE]\n\n", reply)
		out = append(out, &capture.Payload{
			RequestID: id.New("req"), ConversationID: conversationID, TenantID: tenantID,
			Provider: "deepseek-completions", Model: "m", Status: "success", Streaming: true,
			RequestBody: body, ResponseBody: []byte(resp), ResponseFormat: capture.ResponseFormatSSE,
			CreatedAt: start.Add(time.Duration(i) * time.Second),
		})
		messages = append(messages, map[string]any{"role": "assistant", "content": reply}, map[string]any{"role": "user", "content": fmt.Sprintf("next %d", i)})
	}
	return out
}

func TestSessionArchiveEndToEndOnPostgres(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID := seedArchiveTenant(t, pool)
	conversationID := id.New("conv")

	objects := NewSessionObjectStore(pool)
	payloads := NewConversationPayloadStore(pool)
	states := NewSessionArchiveStore(pool)
	builder := sessionarchive.NewBuilder(objects, states, payloads, sessionarchive.BuilderOptions{MergeSegments: 4})
	store := sessionarchive.NewCaptureStore(payloads, builder, zerolog.Nop(), nil)

	captures := chatCaptures(tenantID, conversationID, 9, time.Now().UTC().Add(-time.Hour).Truncate(time.Second))
	for _, p := range captures {
		if err := store.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	st, err := states.Get(ctx, tenantID, conversationID)
	if err != nil || st == nil {
		t.Fatalf("state: %+v %v", st, err)
	}
	if st.RequestCount != 9 || st.UserTurnCount != 9 || st.LastSeq != 9 || st.MergedThroughSeq != 8 || st.NeedsRebuild {
		t.Fatalf("state after 9 captures: %+v", st)
	}
	if st.OpeningPrompt != "please refactor the parser" || st.ObjectsLocation != "postgres" {
		t.Fatalf("opening prompt / location: %q %q", st.OpeningPrompt, st.ObjectsLocation)
	}
	turns, err := states.ListTurns(ctx, st.TranscriptID, st.Generation)
	if err != nil || len(turns) != 9 {
		t.Fatalf("turns: %d %v", len(turns), err)
	}
	// Request k carries 2k-1 items, so turn 4 extends request 3's 5 items by 2.
	if turns[3].Kind != sessionarchive.KindExtend || turns[3].Base != 5 || turns[3].InputCount != 7 || turns[3].DeltaCount != 2 || turns[3].SegmentKey == "" {
		t.Fatalf("turn 4: %+v", turns[3])
	}

	reader := sessionarchive.NewReader(objects)
	page, err := reader.Page(ctx, st, turns, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 9 || page.Turns[8].Output[0].Text != "step 9" {
		t.Fatalf("page: %d turns", len(page.Turns))
	}

	// A capture failure raised concurrently survives a normal save.
	if err := states.MarkNeedsRebuild(ctx, captures[0], "simulated"); err != nil {
		t.Fatal(err)
	}
	extra := chatCaptures(tenantID, conversationID, 10, captures[0].CreatedAt)[9]
	if err := store.Put(ctx, extra); err != nil {
		t.Fatal(err)
	}
	st, _ = states.Get(ctx, tenantID, conversationID)
	if !st.NeedsRebuild || st.LastError != "simulated" || st.RequestCount != 10 {
		t.Fatalf("non-exact save cleared a concurrent rebuild flag: %+v", st)
	}

	rebuilt, err := builder.Rebuild(ctx, tenantID, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Generation != 2 || rebuilt.RequestCount != 10 || rebuilt.NeedsRebuild {
		t.Fatalf("rebuilt: %+v", rebuilt)
	}
	st, _ = states.Get(ctx, tenantID, conversationID)
	oldTurns, _ := states.ListTurns(ctx, st.TranscriptID, 1)
	newTurns, _ := states.ListTurns(ctx, st.TranscriptID, 2)
	if st.Generation != 2 || len(oldTurns) != 0 || len(newTurns) != 10 || st.NeedsRebuild || st.LastError != "" {
		t.Fatalf("after swap: gen=%d old=%d new=%d needs=%v err=%q", st.Generation, len(oldTurns), len(newTurns), st.NeedsRebuild, st.LastError)
	}
	var generation1Objects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_objects WHERE key LIKE $1`, "%/"+conversationID+"/g1/%").Scan(&generation1Objects); err != nil || generation1Objects != 0 {
		t.Fatalf("generation 1 objects left: %d %v", generation1Objects, err)
	}

	matches, err := states.FindByConversation(ctx, conversationID)
	if err != nil || len(matches) != 1 || matches[0].TenantID != tenantID {
		t.Fatalf("find by conversation: %+v %v", matches, err)
	}
}

func TestSessionArchiveLockTimesOut(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	states := NewSessionArchiveStore(pool).WithLockTimeout(200 * time.Millisecond)

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- states.WithConversationLock(ctx, "tn_lock", "conv_lock", func(sessionarchive.StateTx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	start := time.Now()
	err := states.WithConversationLock(ctx, "tn_lock", "conv_lock", func(sessionarchive.StateTx) error { return nil })
	close(release)
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("second lock err = %v, want lock timeout", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("lock wait %v ignored the timeout", waited)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := states.WithConversationLock(ctx, "tn_lock", "other_conv", func(sessionarchive.StateTx) error { return nil }); err != nil {
		t.Fatalf("other conversations must not share the lock: %v", err)
	}
}

func TestConversationPayloadBodyLocations(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID := seedArchiveTenant(t, pool)
	objects := blob.NewMemory()
	past := time.Now().Add(-time.Hour)

	type row struct {
		location string
		reqNull  bool
		reqKey   string
		expires  *time.Time
		reqBytes int64
	}
	load := func(requestID string) row {
		var r row
		if err := pool.QueryRow(ctx, `
			SELECT body_location, request_body_compressed IS NULL, COALESCE(request_object_key, ''), expires_at, COALESCE(request_bytes, -1)
			  FROM conversation_payloads WHERE request_id = $1`, requestID).Scan(&r.location, &r.reqNull, &r.reqKey, &r.expires, &r.reqBytes); err != nil {
			t.Fatal(err)
		}
		return r
	}

	cases := []struct {
		name               string
		inObjects, inPG    bool
		wantLocation       string
		wantNull, wantKeep bool
	}{
		{"postgres", false, true, "postgres", false, false},
		{"objects", true, false, "memory", true, true},
		{"both", true, true, "memory", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewConversationPayloadStore(pool).WithObjects(objects, tc.inObjects, tc.inPG)
			p := chatCaptures(tenantID, id.New("conv"), 1, time.Now())[0]
			expires := past
			p.ExpiresAt = &expires
			if err := store.Put(ctx, p); err != nil {
				t.Fatal(err)
			}
			r := load(p.RequestID)
			if r.location != tc.wantLocation || r.reqNull != tc.wantNull || (r.reqKey != "") != tc.inObjects || r.reqBytes != int64(len(p.RequestBody)) {
				t.Fatalf("row = %+v", r)
			}
			if (r.expires == nil) != tc.wantKeep {
				t.Fatalf("expires_at = %v, object-backed rows must never expire", r.expires)
			}

			got, err := store.Get(ctx, p.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.RequestBody) != string(p.RequestBody) || string(got.ResponseBody) != string(p.ResponseBody) {
				t.Fatal("bodies did not round-trip")
			}
			// Postgres-only readers cannot resolve object-only rows.
			if tc.wantNull {
				if _, err := NewConversationPayloadStore(pool).Get(ctx, p.RequestID); err == nil {
					t.Fatal("reading object-backed bodies without objects must fail")
				}
			}
		})
	}

	// Unbuilt named conversations must survive retention until repair.
	deleted, err := NewConversationPayloadStore(pool).DeleteExpired(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var remaining int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM conversation_payloads WHERE tenant_id = $1`, tenantID).Scan(&remaining)
	if deleted != 0 || remaining != 3 {
		t.Fatalf("deleted=%d remaining=%d, want pending legacy and object-backed rows kept", deleted, remaining)
	}

	store := NewConversationPayloadStore(pool).WithObjects(objects, true, false)
	if _, err := store.LoadPayload(ctx, "req_missing"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing capture err = %v, want blob.ErrNotFound", err)
	}
}

func TestSessionArchiveTurnIPs(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID := seedArchiveTenant(t, pool)
	conversationID := id.New("conv")

	objects := NewSessionObjectStore(pool)
	payloads := NewConversationPayloadStore(pool)
	states := NewSessionArchiveStore(pool)
	builder := sessionarchive.NewBuilder(objects, states, payloads, sessionarchive.BuilderOptions{MergeSegments: 4})
	store := sessionarchive.NewCaptureStore(payloads, builder, zerolog.Nop(), nil)

	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	captures := chatCaptures(tenantID, conversationID, 4, start)
	for _, p := range captures {
		if err := store.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	st, err := states.Get(ctx, tenantID, conversationID)
	if err != nil || st == nil {
		t.Fatalf("state: %+v %v", st, err)
	}

	// Usage events carry the client IPs the proxy observed: three requests
	// from the laptop, one from a server, one request without an IP.
	ips := []string{"100.64.0.10", "100.64.0.10", "", "192.0.2.9"}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM usage_events WHERE tenant_id = $1`, tenantID) })
	usageStore := NewUsageStore(pool)
	for i, p := range captures {
		completed := p.CreatedAt.Add(2 * time.Second)
		if err := usageStore.InsertUsageEvent(ctx, &usage.Event{
			RequestID: p.RequestID, TenantID: tenantID,
			Provider: p.Provider, Model: p.Model, Status: "success",
			ClientIP: ips[i], CostTotalCents: 1,
			StartedAt: p.CreatedAt, CompletedAt: &completed,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := states.TurnIPs(ctx, st.TranscriptID, st.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].IP != "100.64.0.10" || got[0].Requests != 2 || got[1].IP != "192.0.2.9" || got[1].Requests != 1 {
		t.Fatalf("turn ips: %+v", got)
	}
	if !got[0].FirstSeen.Equal(captures[0].CreatedAt) || !got[0].LastSeen.Equal(captures[1].CreatedAt.Add(2*time.Second)) {
		t.Fatalf("first/last seen: %+v want %v..%v", got[0], captures[0].CreatedAt, captures[1].CreatedAt.Add(2*time.Second))
	}
	if !got[1].FirstSeen.Equal(captures[3].CreatedAt) || !got[1].LastSeen.Equal(captures[3].CreatedAt.Add(2*time.Second)) {
		t.Fatalf("server ip window: %+v", got[1])
	}
}

// Two application pools share ten PgBouncer backends. Exercise the nested
// payload/object reads, binary COPY, merges and rebuild swaps under pressure.
func TestArchiveAdmissionConcurrentCaptureAndRebuild(t *testing.T) {
	pool := openIntegrationPool(t)
	other, err := New(context.Background(), Config{DSN: os.Getenv("PIPROXY_TEST_POSTGRES_DSN"), MaxOpenConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	tenant := seedArchiveTenant(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	builders := make([]*sessionarchive.Builder, 2)
	payloads := NewConversationPayloadStore(pool)
	for i, p := range []*Pool{pool, other} {
		builders[i] = sessionarchive.NewBuilder(NewSessionObjectStore(p), NewSessionArchiveStore(p), NewConversationPayloadStore(p), sessionarchive.BuilderOptions{MergeSegments: 2})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		conv := id.New("conv")
		captures := chatCaptures(tenant, conv, 4, time.Now())
		for _, capture := range captures {
			if err := payloads.Put(ctx, capture); err != nil {
				t.Fatal(err)
			}
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := builders[i%2]
			for _, capture := range captures {
				if _, err := b.Apply(ctx, capture); err != nil {
					errs <- err
					return
				}
			}
			state, err := b.Rebuild(ctx, tenant, conv)
			if err == nil && (state.RequestCount != 4 || state.Generation != 2) {
				err = fmt.Errorf("unexpected rebuilt state: requests=%d generation=%d", state.RequestCount, state.Generation)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestArchiveAdmissionCancellationReleasesSlots(t *testing.T) {
	pool := openIntegrationPool(t)
	// Admission is shared even when stores are constructed separately.
	first, second := NewSessionArchiveStore(pool), NewSessionArchiveStore(pool)
	held := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	done := make(chan error, 2)
	for i, store := range []*SessionArchiveStore{first, second} {
		go func() {
			done <- store.WithConversationLock(context.Background(), "admission", fmt.Sprint(i), func(sessionarchive.StateTx) error { held <- struct{}{}; <-release; return nil })
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatal("archive holders did not enter")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := first.WithConversationLock(ctx, "admission", "waiting", func(sessionarchive.StateTx) error { t.Error("cancelled admission entered transaction"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("admission cancellation: %v", err)
	}
	release <- struct{}{}
	release <- struct{}{}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := second.WithConversationLock(context.Background(), "admission", "after", func(sessionarchive.StateTx) error { return errors.New("rollback requested") }); err == nil {
		t.Fatal("callback failure ignored")
	}
	if err := first.WithConversationLock(context.Background(), "admission", "after", func(sessionarchive.StateTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
