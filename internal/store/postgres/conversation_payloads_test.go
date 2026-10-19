package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
)

func TestConversationSessionListQueryFetchesBodiesAfterPagination(t *testing.T) {
	query := conversationSessionListQuery("WHERE 1=1", 1)
	pagedAt := strings.Index(query, "paged AS MATERIALIZED")
	bodyAt := strings.Index(query, "latest_cp.request_body_compressed")
	if pagedAt < 0 || bodyAt < 0 {
		t.Fatalf("expected materialized pagination followed by payload-body selection:\n%s", query)
	}
	if bodyAt < pagedAt {
		t.Fatalf("payload body must not be selected before pagination:\n%s", query)
	}
	if prePagination := query[:pagedAt]; strings.Contains(prePagination, "request_body_compressed") {
		t.Fatalf("sessionization must not read compressed payload bodies:\n%s", query)
	}
	if !strings.Contains(query[pagedAt:], "LEFT JOIN conversation_payloads latest_cp ON latest_cp.request_id = p.last_request_id") {
		t.Fatalf("expected paginated sessions to fetch only their latest payload body:\n%s", query)
	}
}

func TestExtractLastPrompt(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRole string
		wantText string
	}{
		{
			name:     "chat completions messages",
			body:     `{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"text","text":"latest"}]}]}`,
			wantRole: "user",
			wantText: "latest",
		},
		{
			name:     "responses string input",
			body:     `{"input":"direct prompt"}`,
			wantRole: "user",
			wantText: "direct prompt",
		},
		{
			name: "responses input items",
			body: `{
				"input": [
					{"role":"user","content":[{"type":"input_text","text":"first prompt"}]},
					{"type":"reasoning","summary":[]},
					{"type":"function_call","name":"lookup","arguments":"{}"},
					{"type":"function_call_output","output":"tool result"},
					{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
					{"role":"user","content":[{"type":"input_text","text":"latest prompt"}]}
				]
			}`,
			wantRole: "user",
			wantText: "latest prompt",
		},
		{
			name: "responses tool output",
			body: `{
				"input": [
					{"role":"user","content":[{"type":"input_text","text":"prompt"}]},
					{"type":"function_call_output","output":"latest tool result"},
					{"type":"reasoning","summary":[]}
				]
			}`,
			wantRole: "tool",
			wantText: "latest tool result",
		},
		{
			name:     "responses content item",
			body:     `{"input":[{"type":"input_text","text":"top-level content item"}]}`,
			wantRole: "user",
			wantText: "top-level content item",
		},
		{
			name:     "invalid JSON",
			body:     `{`,
			wantRole: "",
			wantText: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, text := extractLastPrompt([]byte(tt.body))
			if role != tt.wantRole || text != tt.wantText {
				t.Fatalf("extractLastPrompt() = (%q, %q), want (%q, %q)", role, text, tt.wantRole, tt.wantText)
			}
		})
	}
}

func TestConversationPayloadArgsUsesNullOptionalRefs(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	done := now.Add(time.Second)
	expires := now.Add(24 * time.Hour)
	payload := &capture.Payload{
		ID:             "cap_123",
		RequestID:      "req_123",
		Provider:       "openai",
		Model:          "gpt-4o",
		Status:         "success",
		ResponseFormat: capture.ResponseFormatJSON,
		CreatedAt:      now,
		CompletedAt:    &done,
		ExpiresAt:      &expires,
	}

	bodies := storedBodies{location: BodyLocationPostgres, request: []byte("req.gz"), response: []byte("resp.gz")}
	args := conversationPayloadArgs(payload, bodies)
	if len(args) != 24 {
		t.Fatalf("expected 24 args, got %d", len(args))
	}
	for _, idx := range []int{2, 3, 4, 5, 9, 20, 21} {
		if args[idx] != nil {
			t.Fatalf("expected arg %d to be nil, got %#v", idx, args[idx])
		}
	}

	payload.ConversationID = "conv_123"
	payload.TenantID = "ten_123"
	payload.UserID = "usr_123"
	payload.KeyID = "key_123"
	payload.ErrorClass = "upstream_error"
	bodies.requestKey, bodies.responseKey = "captures/req", "captures/resp"
	args = conversationPayloadArgs(payload, bodies)
	if args[2] != payload.ConversationID || args[3] != payload.TenantID || args[4] != payload.UserID || args[5] != payload.KeyID || args[9] != payload.ErrorClass {
		t.Fatalf("expected populated optional refs, got %#v", args[:10])
	}
	if args[19] != BodyLocationPostgres || args[20] != "captures/req" || args[21] != "captures/resp" {
		t.Fatalf("expected body location and object keys, got %#v", args[19:])
	}
}
