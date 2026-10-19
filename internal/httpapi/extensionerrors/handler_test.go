package extensionerrors

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type recordingStore struct {
	record *pgstore.ExtensionErrorRecord
}

func (s *recordingStore) Upsert(_ context.Context, record *pgstore.ExtensionErrorRecord) error {
	s.record = record
	return nil
}

func TestHandlerAcceptsAuthenticatedReportWithoutOperationScope(t *testing.T) {
	store := &recordingStore{}
	handler := NewHandler(store, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/extension-errors", bytes.NewBufferString(`{
		"operation":"session_submit","error_code":"http_403","http_status":403,
		"message":"missing sessions:submit","project_id":"mprj_test",
		"conversation_id":"conv_test","extension_version":"1.3.23"
	}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tn_test", UserID: "usr_test", KeyID: "key_test", Scopes: []string{"llm:invoke"},
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if store.record == nil || store.record.TenantID != "tn_test" || store.record.KeyID != "key_test" {
		t.Fatalf("record = %+v", store.record)
	}
}

func TestHandlerRejectsUnauthenticatedReport(t *testing.T) {
	handler := NewHandler(&recordingStore{}, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/extension-errors", bytes.NewBufferString(`{"operation":"session_submit","error_code":"http_403"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}
