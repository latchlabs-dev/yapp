package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPayloadScopeRejectsInvalidWindow(t *testing.T) {
	for _, scope := range []string{`{`, `{}`, `{"from":"bad","to":"bad"}`, `{"from":"2026-10-19T12:00:00Z","to":"2026-10-18T12:00:00Z"}`} {
		req := httptest.NewRequest(http.MethodGet, "/conversation-payloads?session_scope="+url.QueryEscape(scope), nil)
		rec := httptest.NewRecorder()
		// A nil store proves invalid intervals are rejected before querying any data.
		listConversationPayloadsHandler(&Dependencies{}).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("scope %s: status %d", scope, rec.Code)
		}
	}
}
