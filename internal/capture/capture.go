// Package capture records raw provider request and response payloads off the
// hot path through a pluggable storage backend.
package capture

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	HeaderConversationID   = "X-Conversation-ID"
	HeaderSessionID        = "X-Session-ID"
	HeaderThreadID         = "X-Thread-ID"
	HeaderProjectID        = "X-Project-ID"
	HeaderExtensionVersion = "X-Extension-Version"

	ResponseFormatJSON = "json"
	ResponseFormatSSE  = "sse"
	ResponseFormatText = "text"
)

var sessionUUIDPattern = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:\.jsonl)?$`)
var projectIDPattern = regexp.MustCompile(`^mprj_[A-Za-z0-9._-]{1,195}$`)
var extensionVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// Payload is the raw conversation payload captured for one proxied request.
type Payload struct {
	ID                string     `json:"id"`
	RequestID         string     `json:"request_id"`
	ConversationID    string     `json:"conversation_id"`
	TenantID          string     `json:"tenant_id,omitempty"`
	UserID            string     `json:"user_id,omitempty"`
	KeyID             string     `json:"key_id,omitempty"`
	Provider          string     `json:"provider"`
	Model             string     `json:"model"`
	Status            string     `json:"status"`
	ErrorClass        string     `json:"error_class,omitempty"`
	Streaming         bool       `json:"streaming"`
	RequestBody       []byte     `json:"request_body,omitempty"`
	ResponseBody      []byte     `json:"response_body,omitempty"`
	RequestTruncated  bool       `json:"request_truncated"`
	ResponseTruncated bool       `json:"response_truncated"`
	ResponseFormat    string     `json:"response_format"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`

	// MaxPayloadBytes is a recorder-only limit. The data plane sets this before
	// enqueueing so truncation can happen in the capture worker rather than on
	// the request goroutine.
	MaxPayloadBytes int64 `json:"-"`

	// requestLimit is the limit preparePayloadForStorage applied, kept for
	// truncation gap markers. Unexported on purpose: it never round-trips
	// through JSON or storage.
	requestLimit int64 `json:"-"`
}

// PayloadStore is the pluggable backend interface for captured payloads.
type PayloadStore interface {
	Put(ctx context.Context, payload *Payload) error
	Get(ctx context.Context, requestID string) (*Payload, error)
}

// ExtractConversationID returns a caller-supplied conversation/session/thread
// identifier from headers first, then common JSON body locations.
func ExtractConversationID(headers http.Header, body []byte) string {
	if value := ExtractConversationIDFromHeaders(headers); value != "" {
		return value
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}

	return ExtractConversationIDFromBody(raw)
}

// ExtractConversationIDFromHeaders returns only header-supplied correlation IDs.
// It is cheap enough for the data-plane hot path; body JSON parsing is deferred
// to the capture worker when no header is present.
func ExtractConversationIDFromHeaders(headers http.Header) string {
	for _, name := range []string{HeaderConversationID, HeaderSessionID, HeaderThreadID} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return NormalizeConversationID(value)
		}
	}
	return ""
}

// ExtractProjectIDFromHeaders returns a syntactically valid canonical project
// ID. Tenant ownership is validated by the usage store before persistence.
func ExtractProjectIDFromHeaders(headers http.Header) string {
	value := strings.TrimSpace(headers.Get(HeaderProjectID))
	if !projectIDPattern.MatchString(value) {
		return ""
	}
	return value
}

// ExtractExtensionVersionFromHeaders returns a bounded extension version for
// durable request attribution. Invalid values are ignored rather than letting
// an untrusted header poison the asynchronous usage recorder.
func ExtractExtensionVersionFromHeaders(headers http.Header) string {
	value := strings.TrimSpace(headers.Get(HeaderExtensionVersion))
	if !extensionVersionPattern.MatchString(value) {
		return ""
	}
	return value
}

func ExtractConversationIDFromBody(raw map[string]interface{}) string {
	if raw == nil {
		return ""
	}

	for _, key := range []string{"conversation_id", "session_id", "thread_id"} {
		if value := stringField(raw, key); value != "" {
			return NormalizeConversationID(value)
		}
	}

	if metadata, ok := raw["metadata"].(map[string]interface{}); ok {
		for _, key := range []string{"conversation_id", "session_id", "thread_id"} {
			if value := stringField(metadata, key); value != "" {
				return NormalizeConversationID(value)
			}
		}
	}

	return ""
}

// NormalizeConversationID preserves caller-supplied opaque ids while collapsing
// Pi session-file paths into their stable filename UUID suffix. Older extension
// versions used the full local path as X-Session-ID, which made admin URLs ugly
// and leaked local filesystem details into the canonical correlation id.
func NormalizeConversationID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if match := sessionUUIDPattern.FindStringSubmatch(value); len(match) == 2 {
		return strings.ToLower(match[1])
	}
	if strings.HasSuffix(value, ".jsonl") || strings.Contains(value, "/.pi/agent/sessions/") || strings.Contains(value, `\.pi\agent\sessions\`) {
		normalized := strings.ReplaceAll(value, `\`, "/")
		if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
			normalized = normalized[idx+1:]
		}
		normalized = strings.TrimSuffix(normalized, ".jsonl")
		normalized = strings.TrimSpace(normalized)
		if normalized != "" {
			return normalized
		}
	}
	return value
}

func stringField(values map[string]interface{}, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// LimitBytes returns a copy of data capped to maxBytes and whether truncation
// occurred. Non-positive maxBytes means unlimited.
func LimitBytes(data []byte, maxBytes int64) ([]byte, bool) {
	if maxBytes <= 0 || int64(len(data)) <= maxBytes {
		out := make([]byte, len(data))
		copy(out, data)
		return out, false
	}
	out := make([]byte, maxBytes)
	copy(out, data[:maxBytes])
	return out, true
}
