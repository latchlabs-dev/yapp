package insights

import (
	"encoding/json"
	"strings"
	"testing"

	insightsv1 "github.com/earlgrey/piproxy/internal/insightsrpc/v1"
)

func TestNormalizeResponseEnforcesConfigurationTagPolicy(t *testing.T) {
	transcript := &insightsv1.ParsedTranscript{Items: []*insightsv1.TranscriptItem{{Id: "u1"}}}
	current := []*insightsv1.TagDefinition{{Name: "lesson"}}
	response := &insightsv1.GenerateInsightsResponse{
		Insights: []*insightsv1.Insight{{
			Summary: "one\ntwo\nthree", Content: "Never expose sk-secret-value",
			Tags: []string{"Lesson", "Novel Tag"}, SourceItemIds: []string{"u1", "invented"},
		}},
		ProposedTags: []*insightsv1.TagDefinition{{Name: "novel tag", Description: "new concept"}},
	}
	results, proposed, _, _ := normalizeResponse(response, transcript, current, false)
	if len(results) != 1 || strings.Join(results[0].Tags, ",") != "lesson" {
		t.Fatalf("restricted tags = %v", results)
	}
	if len(proposed) != 0 {
		t.Fatalf("proposed tags = %v, want none", proposed)
	}
	if strings.Join(results[0].SourceItemIDs, ",") != "u1" {
		t.Fatalf("source IDs = %v", results[0].SourceItemIDs)
	}
	if results[0].Summary != "one\ntwo" {
		t.Fatalf("summary = %q", results[0].Summary)
	}
	if !strings.Contains(results[0].Content, "[REDACTED]") {
		t.Fatalf("content was not redacted: %q", results[0].Content)
	}

	results, proposed, usage, warnings := normalizeResponse(response, transcript, current, true)
	if strings.Join(results[0].Tags, ",") != "lesson,novel-tag" {
		t.Fatalf("flexible tags = %v", results[0].Tags)
	}
	if len(proposed) != 1 || proposed[0].Name != "novel-tag" {
		t.Fatalf("proposed tags = %+v", proposed)
	}
	if !json.Valid(usage) || !json.Valid(warnings) {
		t.Fatalf("usage=%s warnings=%s", usage, warnings)
	}
}
