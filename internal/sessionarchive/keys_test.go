package sessionarchive

import (
	"strings"
	"testing"
	"time"
)

func TestSafeSegmentKeepsSafeIDsAndHashesEverythingElse(t *testing.T) {
	keep := []string{
		"01a080b7-63ee-7731-9780-11b5f229fa5b",
		"req_01J8ZQ.abc-DEF",
		"user_01ABC",
	}
	for _, value := range keep {
		if got := SafeSegment(value); got != value {
			t.Errorf("SafeSegment(%q) = %q, want unchanged", value, got)
		}
	}

	replace := []string{"", ".", "..", "a/b", "../../etc", "user:abc", "has space", strings.Repeat("x", 129), "ümlaut"}
	for _, value := range replace {
		got := SafeSegment(value)
		if !strings.HasPrefix(got, "h-") || len(got) != 34 {
			t.Errorf("SafeSegment(%q) = %q, want h-<32 hex>", value, got)
		}
		if SafeSegment(value) != got {
			t.Errorf("SafeSegment(%q) is not stable", value)
		}
	}
	if SafeSegment("a/b") == SafeSegment("a_b") {
		t.Error("distinct unsafe inputs must not collide with safe ones")
	}
}

func TestObjectKeysLayout(t *testing.T) {
	created := time.Date(2026, 9, 8, 23, 30, 0, 0, time.FixedZone("PKT", 5*3600))
	if got, want := CaptureRequestKey("tn_1", created, "req_1"), "captures/v1/tn_1/2026/09/08/req_1.request.json.gz"; got != want {
		t.Errorf("request key = %q, want %q (date must be UTC)", got, want)
	}
	if got, want := CaptureResponseKey("", created, "req_1"), "captures/v1/_/2026/09/08/req_1.response.gz"; got != want {
		t.Errorf("response key = %q, want %q", got, want)
	}
	if got, want := SegmentKey("tn_1", "conv", GenerationTag(2, ""), 17), "transcripts/v1/tn_1/conv/g2/seg-000017.jsonl.gz"; got != want {
		t.Errorf("segment key = %q, want %q", got, want)
	}
	if got, want := MergedKey("tn_1", "conv", GenerationTag(1, "01ABC"), 32), "transcripts/v1/tn_1/conv/g1-01ABC/merged-000032.jsonl.gz"; got != want {
		t.Errorf("merged key = %q, want %q", got, want)
	}
	pi := PiSessionKey("tn_1", "user:u1", "sess", "abc123")
	if strings.Count(pi, "/") != 5 || !strings.HasPrefix(pi, "pi-sessions/v1/tn_1/h-") || !strings.HasSuffix(pi, "/sess/abc123.jsonl.gz") {
		t.Errorf("pi key = %q: owner with ':' must be hashed into one segment", pi)
	}
	if strings.Contains(SegmentKey("tn_1", "../../x", GenerationTag(1, "../y"), 1), "..") {
		t.Error("traversal in conversation id escaped into the key")
	}
}
