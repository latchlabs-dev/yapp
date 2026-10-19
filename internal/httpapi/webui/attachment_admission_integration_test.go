package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/id"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type admissionBlobs struct {
	data    []byte
	size    int64
	expires time.Time
	deletes int
}

func (b *admissionBlobs) Bucket() string { return "admission-test" }
func (b *admissionBlobs) SignedUpload(_ context.Context, _, _ string, size int64, expires time.Time) (string, error) {
	b.size, b.expires = size, expires
	return "https://storage.googleapis.com/admission-test/file", nil
}
func (b *admissionBlobs) SignedDownload(context.Context, string, string, time.Duration) (string, error) {
	return "", fmt.Errorf("unused")
}
func (b *admissionBlobs) Stat(context.Context, string) (*blob.ObjectInfo, error) {
	return &blob.ObjectInfo{Size: int64(len(b.data))}, nil
}
func (b *admissionBlobs) ReadAtMost(_ context.Context, _ string, limit int64) ([]byte, error) {
	data := b.data
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, nil
}
func (b *admissionBlobs) Read(context.Context, string) ([]byte, error) { return b.data, nil }
func (b *admissionBlobs) Delete(context.Context, string) error         { b.deletes++; return nil }
func (b *admissionBlobs) Close() error                                 { return nil }

func TestAttachmentHandlersBoundAuthorityAndRetainRejectedUpload(t *testing.T) {
	dsn := os.Getenv("PIPROXY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIPROXY_TEST_POSTGRES_DSN is not set")
	}
	pool, err := pgstore.New(t.Context(), pgstore.Config{DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tenant, user := id.New("tn"), id.New("usr")
	if _, err := pool.Exec(t.Context(), `INSERT INTO tenants(id,name,status) VALUES($1,'Attachment handler test','active')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO users(id,tenant_id,email,status) VALUES($1,$2,$3,'active')`, user, tenant, user+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM web_attachments WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
	}()
	blobs := &admissionBlobs{}
	deps := &Dependencies{Config: Config{AttachmentMaxBytes: 100, AttachmentURLTTL: time.Minute}, Logger: zerolog.Nop(), Attachments: pgstore.NewWebAttachmentStore(pool), Blobs: blobs}
	authCtx := &auth.Context{TenantID: tenant, UserID: user, Scopes: []string{auth.ScopeLLMInvoke}}
	request := httptest.NewRequest(http.MethodPost, "/attachments", bytes.NewBufferString(`{"filename":"tiny.txt","content_type":"text/plain","size_bytes":5}`))
	request = request.WithContext(middleware.WithAuthContext(request.Context(), authCtx))
	response := httptest.NewRecorder()
	createAttachmentHandler(deps)(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("reserve status=%d body=%s", response.Code, response.Body.String())
	}
	var ticket struct {
		ID      string            `json:"id"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if blobs.size != 5 || ticket.Headers["X-Goog-Content-Length-Range"] != "5,5" || ticket.Headers["X-Goog-If-Generation-Match"] != "0" {
		t.Fatalf("upload authority did not match reservation: %+v signed size=%d", ticket, blobs.size)
	}
	var expires time.Time
	if err := pool.QueryRow(t.Context(), `SELECT upload_expires_at FROM web_attachments WHERE id=$1`, ticket.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if delta := expires.Sub(blobs.expires); delta > time.Microsecond || delta < -time.Microsecond {
		t.Fatalf("stored and signed expiry differ: %v", delta)
	}
	// Exercise defensive verification for an old or non-conforming upload URL.
	blobs.data = []byte("sixsix")
	route := chi.NewRouteContext()
	route.URLParams.Add("attachmentID", ticket.ID)
	request = httptest.NewRequest(http.MethodPost, "/attachments/"+ticket.ID+"/finalize", nil)
	request = request.WithContext(context.WithValue(middleware.WithAuthContext(request.Context(), authCtx), chi.RouteCtxKey, route))
	response = httptest.NewRecorder()
	finalizeAttachmentHandler(deps)(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("mismatched size status=%d body=%s", response.Code, response.Body.String())
	}
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM web_attachments WHERE id=$1`, ticket.ID).Scan(&status); err != nil || status != pgstore.AttachmentRejected {
		t.Fatalf("rejected authority lost tracking: status=%q err=%v", status, err)
	}
	if blobs.deletes != 0 {
		t.Fatal("object was removed while its create-only URL was still live")
	}
}
