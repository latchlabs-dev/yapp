package insights

import (
	"strings"
	"testing"
)

func TestParseTranscriptOwnsPiJSONLParsing(t *testing.T) {
	raw := []byte(`{"type":"session","id":"s1","cwd":"/secret"}
{"type":"message","id":"u1","timestamp":"2026-08-20T01:00:00Z","message":{"role":"user","content":[{"type":"text","text":"first question"}]}}
{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private chain"},{"type":"text","text":"use the leaf package"}]}}
{"type":"tool_result","id":"t1","content":"secret tool output"}
{"type":"message","message":{"role":"user","content":"final question"}}`)
	parsed := ParseTranscript(raw, "session-1", "project-1", 10, 1000)
	if parsed.SessionId != "session-1" || parsed.ProjectId != "project-1" {
		t.Fatalf("identity = %+v", parsed)
	}
	if len(parsed.Items) != 3 {
		t.Fatalf("items = %+v", parsed.Items)
	}
	if parsed.Items[0].Id != "u1" || parsed.Items[1].Id != "a1" || !strings.HasPrefix(parsed.Items[2].Id, "line-") {
		t.Fatalf("item IDs = %+v", parsed.Items)
	}
	joined := parsed.Items[0].Content + parsed.Items[1].Content + parsed.Items[2].Content
	if strings.Contains(joined, "private chain") || strings.Contains(joined, "secret tool") || strings.Contains(joined, "/secret") {
		t.Fatalf("unparsed private record crossed boundary: %s", joined)
	}
}

func TestParseTranscriptKeepsTailWithinConfiguredBudget(t *testing.T) {
	raw := []byte(`{"type":"message","id":"u1","message":{"role":"user","content":"` + strings.Repeat("a", 80) + `"}}
{"type":"message","id":"u2","message":{"role":"user","content":"final question"}}`)
	parsed := ParseTranscript(raw, "s", "p", 10, 20)
	if len(parsed.Items) == 0 || !strings.Contains(parsed.Items[len(parsed.Items)-1].Content, "final question") {
		t.Fatalf("items = %+v", parsed.Items)
	}
	total := 0
	for _, item := range parsed.Items {
		total += len([]rune(item.Content))
	}
	if total > 20 {
		t.Fatalf("parsed content chars = %d, want <= 20", total)
	}
}
