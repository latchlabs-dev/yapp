// Package sessionarchive stores captured coding-agent sessions as Postgres
// metadata plus object-storage bodies: raw request/response captures, a
// deduplicated per-request transcript, and Pi session exports.
//
// Pi resends the whole conversation on every request, so storing each request
// verbatim grows quadratically. The transcript builder instead keeps a
// hash-chained "line" of the conversation items it has already stored and
// writes only the items each request adds, treating compaction, branches and
// out-of-order captures as explicit resets. See SESSION_ARCHIVE_PLAN.md.
package sessionarchive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var safeKeySegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// SafeSegment returns value unchanged when it is a safe single object-key
// segment, and a stable hash-derived replacement otherwise. Conversation,
// owner, session and request ids are caller-controlled, so this is what keeps
// every key inside its prefix.
func SafeSegment(value string) string {
	if value != "." && value != ".." && safeKeySegment.MatchString(value) {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "h-" + hex.EncodeToString(sum[:])[:32]
}

func tenantSegment(tenantID string) string {
	if strings.TrimSpace(tenantID) == "" {
		return "_"
	}
	return SafeSegment(tenantID)
}

func captureKey(tenantID string, createdAt time.Time, requestID, suffix string) string {
	day := createdAt.UTC()
	return fmt.Sprintf("captures/v1/%s/%04d/%02d/%02d/%s.%s",
		tenantSegment(tenantID), day.Year(), int(day.Month()), day.Day(), SafeSegment(requestID), suffix)
}

// CaptureRequestKey names the gzip raw request body of one captured request.
func CaptureRequestKey(tenantID string, createdAt time.Time, requestID string) string {
	return captureKey(tenantID, createdAt, requestID, "request.json.gz")
}

// CaptureResponseKey names the gzip raw response body (JSON or SSE transcript).
func CaptureResponseKey(tenantID string, createdAt time.Time, requestID string) string {
	return captureKey(tenantID, createdAt, requestID, "response.gz")
}

// GenerationTag names a transcript generation's key directory. Capture-time
// writes use the bare generation; a rebuild adds a random token so a rebuild
// that loses a race can delete everything it wrote without touching objects
// the winning generation references.
func GenerationTag(generation int, token string) string {
	if token == "" {
		return fmt.Sprintf("g%d", generation)
	}
	return fmt.Sprintf("g%d-%s", generation, SafeSegment(token))
}

func transcriptDir(tenantID, conversationID, generationTag string) string {
	return fmt.Sprintf("transcripts/v1/%s/%s/%s", tenantSegment(tenantID), SafeSegment(conversationID), SafeSegment(generationTag))
}

// SegmentKey names the delta segment written for one captured request.
func SegmentKey(tenantID, conversationID, generationTag string, seq int) string {
	return fmt.Sprintf("%s/seg-%06d.jsonl.gz", transcriptDir(tenantID, conversationID, generationTag), seq)
}

// MergedKey names a merged transcript covering segments 1..throughSeq.
func MergedKey(tenantID, conversationID, generationTag string, throughSeq int) string {
	return fmt.Sprintf("%s/merged-%06d.jsonl.gz", transcriptDir(tenantID, conversationID, generationTag), throughSeq)
}

// PiSessionKey names one content-addressed Pi session JSONL export.
func PiSessionKey(tenantID, ownerID, sessionID, contentSHA256 string) string {
	return fmt.Sprintf("pi-sessions/v1/%s/%s/%s/%s.jsonl.gz",
		tenantSegment(tenantID), SafeSegment(ownerID), SafeSegment(sessionID), SafeSegment(contentSHA256))
}
