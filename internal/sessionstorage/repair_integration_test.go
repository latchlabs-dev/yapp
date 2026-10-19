package sessionstorage

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type interruptedRepairObjects struct {
	*blob.Memory
	started chan struct{}
	ready   chan struct{}
	once    sync.Once
}

func (o *interruptedRepairObjects) Read(ctx context.Context, key string) ([]byte, error) {
	o.once.Do(func() { close(o.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.ready:
		return o.Memory.Read(ctx, key)
	}
}

func TestRepairWorkerArchivesAndRecoversAfterRestart(t *testing.T) {
	dsn := os.Getenv("PIPROXY_REPAIR_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated repair test database not set")
	}
	ctx := context.Background()
	pool, err := pgstore.New(ctx, pgstore.Config{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant, conversation := id.New("tn"), id.New("conv")
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,status) VALUES ($1,'repair test','active')`, tenant); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM session_transcripts WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM conversation_payloads WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
	}()
	legacy := pgstore.NewConversationPayloadStore(pool)
	expiry := time.Now().Add(-time.Hour)
	for i := range 3 {
		p := &capture.Payload{RequestID: id.New("req"), TenantID: tenant, ConversationID: conversation, Provider: "openai", Status: "success",
			RequestBody: []byte(`{"messages":[{"role":"user","content":"retained history"}]}`), ResponseBody: []byte(`{"choices":[]}`),
			CreatedAt: time.Now().Add(-time.Hour).Add(time.Duration(i) * time.Second), ExpiresAt: &expiry}
		if err := legacy.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	objects := &interruptedRepairObjects{Memory: blob.NewMemory(), started: make(chan struct{}), ready: make(chan struct{})}
	makeService := func() *Services {
		payloads := pgstore.NewConversationPayloadStore(pool).WithObjects(objects, true, true)
		states := pgstore.NewSessionArchiveStore(pool)
		return &Services{Config: config.SessionArchiveConfig{Enabled: true, MergeSegments: 2}, Objects: objects, Payloads: payloads, States: states,
			Builder: sessionarchive.NewBuilder(objects, states, payloads, sessionarchive.BuilderOptions{}), Reader: sessionarchive.NewReader(objects), logger: zerolog.Nop(), baseCtx: ctx}
	}
	first := makeService()
	first.StartRepairWorker(ctx, nil)
	select {
	case <-objects.started:
	case <-time.After(5 * time.Second):
		_ = first.Close()
		t.Fatal("background worker did not discover legacy conversation")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	var pending, unleased bool
	if err := pool.QueryRow(ctx, `SELECT needs_rebuild,repair_lease_token IS NULL FROM session_transcripts WHERE tenant_id=$1 AND conversation_id=$2`, tenant, conversation).Scan(&pending, &unleased); err != nil || !pending || !unleased {
		t.Fatalf("restart lost pending work: pending=%v unleased=%v %v", pending, unleased, err)
	}
	close(objects.ready)
	_, _ = pool.Exec(ctx, `UPDATE session_transcripts SET repair_retry_at=now() WHERE tenant_id=$1`, tenant)
	second := makeService()
	second.StartRepairWorker(ctx, nil)
	defer second.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := second.States.Get(ctx, tenant, conversation)
		if err != nil {
			t.Fatal(err)
		}
		if state != nil && !state.NeedsRebuild && state.LastSeq == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart did not complete repair: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var durable, intact int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FILTER(WHERE body_location='memory' AND expires_at IS NULL),COUNT(*) FILTER(WHERE request_body_compressed IS NOT NULL AND response_body_compressed IS NOT NULL) FROM conversation_payloads WHERE tenant_id=$1`, tenant).Scan(&durable, &intact); err != nil || durable != 3 || intact != 3 {
		t.Fatalf("archival lost source: durable=%d intact=%d %v", durable, intact, err)
	}
	page, _, err := second.TranscriptPage(ctx, tenant, conversation, 1, 50)
	if err != nil || len(page.Turns) != 3 {
		t.Fatalf("repaired transcript unreadable: %+v %v", page, err)
	}
}
