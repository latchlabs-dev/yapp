package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeSessions struct {
	marked map[string]string // conversationID → distillErr
}

func (f *fakeSessions) MarkDistilledFor(ctx context.Context, identity pgstore.SessionIdentity, distillErr string) error {
	if f.marked == nil {
		f.marked = map[string]string{}
	}
	f.marked[identity.ConversationID] = distillErr
	return nil
}

// fakeSessionLogs stands in for the canonical Pi session log store. When err is
// set it is returned verbatim (e.g. pgstore.ErrSessionLogNotFound); otherwise
// the fixed JSONL is returned for any requested session id.
type fakeSessionLogs struct {
	jsonl []byte
	rec   *pgstore.PiSessionLogRecord
	err   error
}

func (f *fakeSessionLogs) GetBySession(ctx context.Context, tenantID, ownerID, sessionID string) (*pgstore.PiSessionLogRecord, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	rec := f.rec
	if rec == nil {
		rec = &pgstore.PiSessionLogRecord{TenantID: tenantID, OwnerID: ownerID, SessionID: sessionID}
	}
	return rec, f.jsonl, nil
}

type fakeJournal struct {
	appends map[string][]string
	err     error
}

func (f *fakeJournal) JournalAppend(ctx context.Context, path, content string) error {
	if f.err != nil {
		return f.err
	}
	if f.appends == nil {
		f.appends = map[string][]string{}
	}
	f.appends[path] = append(f.appends[path], content)
	return nil
}

type fakeReadableJournal struct {
	fakeJournal
	files   map[string]string
	readErr error
}

func (f *fakeReadableJournal) ReadFile(ctx context.Context, path string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	content, ok := f.files[path]
	if !ok {
		return "", duffel.ErrNotFound
	}
	return content, nil
}

// sampleSessionJSONL is a canonical Pi session log (one header + control entries
// + user/assistant message turns with text and thinking blocks).
const sampleSessionJSONL = `{"type":"session","version":3,"id":"conv1","timestamp":"2026-07-10T08:36:07.295Z","cwd":"/Users/example/code/earlgrey/piproxy"}
{"type":"model_change","id":"m1","timestamp":"2026-07-10T08:36:11Z","provider":"deepseek","modelId":"deepseek-v4-flash"}
{"type":"message","id":"u1","timestamp":"2026-07-10T08:36:13Z","message":{"role":"user","content":[{"type":"text","text":"fix the auth bug"}]}}
{"type":"message","id":"a1","timestamp":"2026-07-10T08:36:14Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret reasoning","thinkingSignature":"reasoning_content"},{"type":"text","text":"I broke the tests by assuming X"}]}}
{"type":"message","id":"u2","timestamp":"2026-07-10T08:36:20Z","message":{"role":"user","content":[{"type":"text","text":"tests are failing"}]}}
{"type":"message","id":"a2","timestamp":"2026-07-10T08:36:25Z","message":{"role":"assistant","content":[{"type":"text","text":"fixed it"}]}}`

func newTestDistiller(sessions *fakeSessions, logs *fakeSessionLogs, llm *mockLLM, journal *fakeJournal) *Distiller {
	return NewDistiller(sessions, logs, llm, journal, DistillerConfig{
		Model: "deepseek-v4-flash",
	}, zerolog.Nop())
}

func TestDistillOneHappyPath(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMText(`{"observations":[{"kind":"mistake","content":"Assumed X; tests broke."},{"kind":"lesson","content":"Run make test before committing."}],"prompt_reflection":{"quality_score":7,"strengths":["The task named the broken behavior."],"improvements":[{"issue":"No acceptance command was specified.","better_prompt":"Fix the auth bug and verify with make test.","expected_benefit":"Makes completion measurable and avoids a late test correction."}]}}`)
	journal := &fakeJournal{}

	d := newTestDistiller(sessions, logs, llm, journal)
	rec := testSessionRecord("conv1", "demo")
	if err := d.DistillOne(context.Background(), rec); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}

	entries := journal.appends["projects/tenant-1--demo/lessons.md"]
	if len(entries) != 1 {
		t.Fatalf("journal appends = %v", journal.appends)
	}
	if !strings.Contains(entries[0], "**[mistake]**") || !strings.Contains(entries[0], "conv1") {
		t.Errorf("entry = %q", entries[0])
	}
	reflections := journal.appends["projects/tenant-1--demo/prompt-reflections.md"]
	if len(reflections) != 1 || !strings.Contains(reflections[0], "**Prompt effectiveness:** 7/10") ||
		!strings.Contains(reflections[0], "**Better prompt:** Fix the auth bug and verify with make test.") {
		t.Errorf("prompt reflection entries = %v", reflections)
	}
	if sessions.marked["conv1"] != "" {
		t.Errorf("marked = %q, want success", sessions.marked["conv1"])
	}
	req := llm.request(t, 0)
	if req.Model != "deepseek-v4-flash" {
		t.Errorf("LLM model = %q", req.Model)
	}
	if req.System != distillerSystemPrompt {
		t.Error("LLM request should use the distiller system prompt")
	}
	if req.MaxTokens != 1536 {
		t.Errorf("LLM max tokens = %d", req.MaxTokens)
	}
	if !strings.Contains(req.User, "Project: tenant-1--demo") {
		t.Error("LLM request should include the sanitized project")
	}
	if !strings.Contains(req.User, "fix the auth bug") {
		t.Error("transcript missing user message")
	}
	if !strings.Contains(req.User, "fixed it") {
		t.Error("transcript missing final assistant turn")
	}
	if strings.Contains(req.User, "secret reasoning") {
		t.Error("thinking blocks must not leak into the transcript")
	}
}

func TestDistillOneNoProject(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	d := newTestDistiller(sessions, logs, newMockLLM(), &fakeJournal{})
	rec := testSessionRecord("conv2", "")
	if err := d.DistillOne(context.Background(), rec); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if !strings.Contains(sessions.marked["conv2"], "unresolved repository identity") {
		t.Errorf("marked = %q", sessions.marked["conv2"])
	}
}

func TestDistillOneFallsBackToSessionLogProject(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{
		jsonl: []byte(sampleSessionJSONL),
		rec:   &pgstore.PiSessionLogRecord{ProjectID: "demo"},
	}
	llm := newMockLLMText(`{"observations":[{"kind":"lesson","content":"resolved via canonical log project"}]}`)
	journal := &fakeJournal{}

	d := newTestDistiller(sessions, logs, llm, journal)
	rec := testSessionRecord("conv-fallback", "") // no project on the submission
	if err := d.DistillOne(context.Background(), rec); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if len(journal.appends["projects/tenant-1--demo/lessons.md"]) != 1 {
		t.Fatalf("expected append under the log's project; got %v", journal.appends)
	}
	if sessions.marked["conv-fallback"] != "" {
		t.Errorf("marked = %q, want success", sessions.marked["conv-fallback"])
	}
}

func TestDistillOneNoCanonicalLog(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{err: pgstore.ErrSessionLogNotFound}
	d := newTestDistiller(sessions, logs, newMockLLM(), &fakeJournal{})
	rec := testSessionRecord("conv3", "demo")
	if err := d.DistillOne(context.Background(), rec); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if !strings.Contains(sessions.marked["conv3"], "no canonical session log") {
		t.Errorf("marked = %q", sessions.marked["conv3"])
	}
}

func TestDistillOneCanonicalLogErrorRetries(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{err: errors.New("db unavailable")}
	d := newTestDistiller(sessions, logs, newMockLLM(), &fakeJournal{})
	if err := d.DistillOne(context.Background(), testSessionRecord("conv-dberr", "demo")); err == nil {
		t.Fatal("expected error so the session is retried")
	}
	if _, marked := sessions.marked["conv-dberr"]; marked {
		t.Error("a transient log-store failure must not mark the session distilled")
	}
}

func TestDistillOneEmptyObservations(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMText(`{"observations":[]}`)
	journal := &fakeJournal{}

	d := newTestDistiller(sessions, logs, llm, journal)
	rec := testSessionRecord("conv4", "demo")
	if err := d.DistillOne(context.Background(), rec); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if len(journal.appends) != 0 {
		t.Error("no observations should mean no journal write")
	}
	if !strings.Contains(sessions.marked["conv4"], "no durable observations") {
		t.Errorf("marked = %q", sessions.marked["conv4"])
	}
}

func TestDistillOneStoresPromptReflectionWithoutDurableObservations(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMText(`{"observations":[],"prompt_reflection":{"quality_score":9,"strengths":["The requested outcome was explicit."],"improvements":[]}}`)
	journal := &fakeJournal{}

	d := newTestDistiller(sessions, logs, llm, journal)
	if err := d.DistillOne(context.Background(), testSessionRecord("conv-prompt-only", "demo")); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if len(journal.appends["projects/tenant-1--demo/lessons.md"]) != 0 {
		t.Fatal("prompt-only result must not append to lessons")
	}
	entries := journal.appends["projects/tenant-1--demo/prompt-reflections.md"]
	if len(entries) != 1 || !strings.Contains(entries[0], "explicit") {
		t.Fatalf("prompt reflection entries = %v", entries)
	}
	if sessions.marked["conv-prompt-only"] != "" {
		t.Errorf("marked = %q, want success", sessions.marked["conv-prompt-only"])
	}
}

func TestDistillOneLLMErrorRetries(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMError(errors.New("rate limited"))

	d := newTestDistiller(sessions, logs, llm, &fakeJournal{})
	rec := testSessionRecord("conv5", "demo")
	if err := d.DistillOne(context.Background(), rec); err == nil {
		t.Fatal("expected error so the session is retried")
	}
	if _, marked := sessions.marked["conv5"]; marked {
		t.Error("LLM failure must not mark the session distilled (retry path)")
	}
}

func TestDistillOneRepairsMarkerWithoutDuplicateSynthesis(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLM()
	journal := &fakeReadableJournal{files: map[string]string{
		"projects/tenant-1--demo/lessons.md":            "(source: distiller, conversation: conv-retry)\n\n**[lesson]** already synthesized\n",
		"projects/tenant-1--demo/prompt-reflections.md": "(source: distiller, conversation: conv-retry)\n\n**Prompt effectiveness:** 8/10\n",
	}}

	d := NewDistiller(sessions, logs, llm, journal, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	if err := d.DistillOne(context.Background(), &pgstore.SessionSubmissionRecord{
		TenantID:       "tenant-1",
		UserID:         "user-1",
		OwnerID:        "user:user-1",
		ConversationID: "conv-retry",
		Project:        "demo",
		ProjectID:      "demo",
	}); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("LLM requests = %d, want 0 for already-synthesized session", len(llm.requests))
	}
	if len(journal.appends) != 0 {
		t.Fatalf("journal appends = %v, want none", journal.appends)
	}
	if sessions.marked["conv-retry"] != "" {
		t.Errorf("marked = %q, want repaired success marker", sessions.marked["conv-retry"])
	}
}

func TestDistillOneRetryWritesOnlyMissingPromptReflection(t *testing.T) {
	sessions := &fakeSessions{}
	logs := &fakeSessionLogs{jsonl: []byte(sampleSessionJSONL)}
	llm := newMockLLMText(`{"observations":[{"kind":"lesson","content":"already stored"}],"prompt_reflection":{"quality_score":8,"strengths":["Clear outcome."],"improvements":[]}}`)
	journal := &fakeReadableJournal{files: map[string]string{
		"projects/tenant-1--demo/lessons.md": "(source: distiller, conversation: conv-partial)\n\n**[lesson]** already stored\n",
	}}

	d := NewDistiller(sessions, logs, llm, journal, DistillerConfig{Model: "deepseek-v4-flash"}, zerolog.Nop())
	if err := d.DistillOne(context.Background(), testSessionRecord("conv-partial", "demo")); err != nil {
		t.Fatalf("DistillOne: %v", err)
	}
	if len(journal.appends["projects/tenant-1--demo/lessons.md"]) != 0 {
		t.Fatal("retry must not duplicate the existing lessons entry")
	}
	if len(journal.appends["projects/tenant-1--demo/prompt-reflections.md"]) != 1 {
		t.Fatalf("appends = %v, want only the missing prompt reflection", journal.appends)
	}
}

func testSessionRecord(conversationID, project string) *pgstore.SessionSubmissionRecord {
	return &pgstore.SessionSubmissionRecord{
		TenantID: "tenant-1", UserID: "user-1", OwnerID: "user:user-1",
		ConversationID: conversationID, Project: project, ProjectID: project,
	}
}

func TestParseObservations(t *testing.T) {
	t.Run("fenced", func(t *testing.T) {
		obs, err := ParseObservations("```json\n{\"observations\":[{\"kind\":\"lesson\",\"content\":\"x\"}]}\n```")
		if err != nil || len(obs) != 1 {
			t.Fatalf("obs = %v, err = %v", obs, err)
		}
	})
	t.Run("invalid kind dropped", func(t *testing.T) {
		obs, err := ParseObservations(`{"observations":[{"kind":"opinion","content":"x"},{"kind":"mistake","content":"y"}]}`)
		if err != nil || len(obs) != 1 || obs[0].Kind != "mistake" {
			t.Fatalf("obs = %v, err = %v", obs, err)
		}
	})
	t.Run("extracts JSON wrapped in prose", func(t *testing.T) {
		obs, err := ParseObservations("We learned this:\n{\"observations\":[{\"kind\":\"context\",\"content\":\"Start in internal/memory/distill.go.\"}]}\nHope that helps.")
		if err != nil || len(obs) != 1 || obs[0].Kind != "context" {
			t.Fatalf("obs = %v, err = %v", obs, err)
		}
	})
	t.Run("caps at five", func(t *testing.T) {
		raw := `{"observations":[` + strings.Repeat(`{"kind":"lesson","content":"x"},`, 7)
		raw = strings.TrimSuffix(raw, ",") + "]}"
		obs, err := ParseObservations(raw)
		if err != nil || len(obs) != 5 {
			t.Fatalf("obs = %d, err = %v", len(obs), err)
		}
	})
	t.Run("garbage errors", func(t *testing.T) {
		if _, err := ParseObservations("not json at all"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestParseDistillationResultPromptReflection(t *testing.T) {
	raw := `{"observations":[],"prompt_reflection":{"quality_score":6,"strengths":[" clear scope ","", "acceptance criteria", "extra"],"improvements":[{"issue":" vague verification ","better_prompt":" Run make test. ","expected_benefit":" Catches failures. "},{"issue":"missing fields"}]}}`
	result, err := ParseDistillationResult(raw)
	if err != nil {
		t.Fatalf("ParseDistillationResult: %v", err)
	}
	if result.PromptReflection.QualityScore != 6 {
		t.Fatalf("quality score = %d", result.PromptReflection.QualityScore)
	}
	if len(result.PromptReflection.Strengths) != 3 || result.PromptReflection.Strengths[0] != "clear scope" {
		t.Fatalf("strengths = %v", result.PromptReflection.Strengths)
	}
	if len(result.PromptReflection.Improvements) != 1 || result.PromptReflection.Improvements[0].BetterPrompt != "Run make test." {
		t.Fatalf("improvements = %v", result.PromptReflection.Improvements)
	}
}

func TestParseDistillationResultDropsInvalidPromptReflectionScore(t *testing.T) {
	result, err := ParseDistillationResult(`{"observations":[],"prompt_reflection":{"quality_score":11,"strengths":["x"],"improvements":[]}}`)
	if err != nil {
		t.Fatalf("ParseDistillationResult: %v", err)
	}
	if result.PromptReflection.QualityScore != 0 {
		t.Fatalf("prompt reflection = %+v, want dropped", result.PromptReflection)
	}
}

func TestFlattenPiSessionJSONL(t *testing.T) {
	out := FlattenPiSessionJSONL([]byte(sampleSessionJSONL), 4000)
	for _, want := range []string{
		"USER: fix the auth bug",
		"ASSISTANT: I broke the tests by assuming X",
		"USER: tests are failing",
		"ASSISTANT: fixed it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("flattened transcript missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret reasoning") {
		t.Error("thinking blocks must not appear in the transcript")
	}
	if strings.Contains(out, "/Users/example") {
		t.Error("session header (cwd) must not appear in the transcript")
	}

	// Tail truncation keeps the final turn even when an earlier turn is huge.
	long := `{"type":"session","version":3,"id":"x","timestamp":"t","cwd":"/tmp"}` + "\n" +
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("filler ", 5000) + `"}]}}` + "\n" +
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"final question"}]}}`
	trunc := FlattenPiSessionJSONL([]byte(long), 500)
	if EstimateTokens(trunc) > 600 {
		t.Errorf("transcript too long: %d tokens", EstimateTokens(trunc))
	}
	if !strings.Contains(trunc, "final question") {
		t.Error("tail truncation should keep the last turn")
	}
}

func TestFlattenTranscriptTruncation(t *testing.T) {
	long := strings.Repeat("word ", 5000)
	body := `{"messages":[{"role":"user","content":"early context"},{"role":"assistant","content":"` + long + `"},{"role":"user","content":"final question"}]}`
	out := FlattenTranscript([]byte(body), nil, 500)
	if EstimateTokens(out) > 600 {
		t.Errorf("transcript too long: %d tokens", EstimateTokens(out))
	}
	if !strings.Contains(out, "final question") {
		t.Error("tail truncation should keep the last turn")
	}
}
