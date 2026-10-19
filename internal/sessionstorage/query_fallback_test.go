package sessionstorage

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

type fallbackStates struct{ lists int }

func (s *fallbackStates) Get(context.Context, string, string) (*sessionarchive.State, error) {
	return &sessionarchive.State{TranscriptID: "id", TenantID: "tenant", Generation: 1, LastSeq: 1, MergedKey: "merged"}, nil
}

func TestVerifiedQueryPageSkipsSourceObjectsAndTurnListing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := sha256.Sum256([]byte("tenant"))
		w.Header().Set("X-Transcript-Tenant", hex.EncodeToString(hash[:]))
		w.Header().Set("X-Transcript-ID", "id")
		w.Header().Set("X-Transcript-Generation", "1")
		w.Header().Set("X-Transcript-Last-Seq", "1")
		_, _ = w.Write([]byte("{\"t\":\"turn\",\"v\":1,\"seq\":1,\"request_id\":\"r1\"}\n"))
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	// No source object exists, so an unintended fallback would fail this check.
	objects := blob.NewMemory()
	services := &Services{Objects: objects, Reader: sessionarchive.NewReader(objects),
		Query: sessionarchive.NewQueryClient(server.URL, "token", time.Second), logger: zerolog.Nop()}
	states := &fallbackStates{}
	page, state, err := services.transcriptPage(context.Background(), states, "tenant", "conversation", 1, 50)
	if err != nil || state == nil || len(page.Turns) != 1 || page.Turns[0].RequestID != "r1" || states.lists != 0 {
		t.Fatalf("verified query read: page=%+v lists=%d err=%v", page, states.lists, err)
	}
}
func (s *fallbackStates) ListTurns(context.Context, string, int) ([]sessionarchive.Turn, error) {
	s.lists++
	return nil, nil
}

func TestQueryFailureFallsBackAndCancellationDoesNotReadObjects(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict) // New turns are not yet exported.
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	objects := blob.NewMemory()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("{\"t\":\"turn\",\"v\":1,\"seq\":1,\"request_id\":\"r1\"}\n"))
	_ = writer.Close()
	if err := objects.Put(context.Background(), "merged", compressed.Bytes(), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	services := &Services{Objects: objects, Reader: sessionarchive.NewReader(objects),
		Query: sessionarchive.NewQueryClient(server.URL, "token", time.Second), logger: zerolog.Nop()}
	states := &fallbackStates{}
	page, _, err := services.transcriptPage(context.Background(), states, "tenant", "conversation", 1, 50)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].RequestID != "r1" || states.lists != 1 {
		t.Fatalf("source fallback: page=%+v lists=%d err=%v", page, states.lists, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = services.transcriptPage(ctx, states, "tenant", "conversation", 1, 50)
	if err == nil || states.lists != 1 {
		t.Fatalf("cancellation must not start source I/O: lists=%d err=%v", states.lists, err)
	}
}
