package sessionarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
)

const (
	testTenant = "tn_test"
	testConv   = "conv-1"
)

var testEpoch = time.Date(2026, 9, 11, 6, 0, 0, 0, time.UTC)

// memoryPayloads is a PayloadSource over an in-memory capture list.
type memoryPayloads struct {
	mu       sync.Mutex
	payloads []*capture.Payload
	onLoad   func(requestID string)
}

func (m *memoryPayloads) add(p *capture.Payload) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payloads = append(m.payloads, p)
}

func (m *memoryPayloads) ListConversationRequests(_ context.Context, tenantID, conversationID string) ([]PayloadRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var refs []PayloadRef
	for _, p := range m.payloads {
		if p.TenantID == tenantID && p.ConversationID == conversationID {
			refs = append(refs, PayloadRef{RequestID: p.RequestID, CreatedAt: p.CreatedAt})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if !refs[i].CreatedAt.Equal(refs[j].CreatedAt) {
			return refs[i].CreatedAt.Before(refs[j].CreatedAt)
		}
		return refs[i].RequestID < refs[j].RequestID
	})
	return refs, nil
}

func (m *memoryPayloads) HasRequestsBefore(_ context.Context, tenantID, conversationID string, before time.Time, exclude string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.payloads {
		if p.TenantID == tenantID && p.ConversationID == conversationID && p.RequestID != exclude && p.CreatedAt.Before(before) {
			return true, nil
		}
	}
	return false, nil
}

func (m *memoryPayloads) LoadPayload(_ context.Context, requestID string) (*capture.Payload, error) {
	if m.onLoad != nil {
		m.onLoad(requestID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.payloads {
		if p.RequestID == requestID {
			return p, nil
		}
	}
	return nil, blob.ErrNotFound
}

type harness struct {
	t        *testing.T
	objects  *blob.Memory
	states   *MemoryStateStore
	payloads *memoryPayloads
	builder  *Builder
	reader   *Reader
}

func newHarness(t *testing.T, mergeSegments int) *harness {
	h := &harness{t: t, objects: blob.NewMemory(), states: NewMemoryStateStore(), payloads: &memoryPayloads{}}
	h.builder = NewBuilder(h.objects, h.states, h.payloads, BuilderOptions{MergeSegments: mergeSegments})
	h.reader = NewReader(h.objects)
	return h
}

// capture records p as captured and applies it, as the capture store does.
func (h *harness) capture(p *capture.Payload) *Turn {
	h.t.Helper()
	h.payloads.add(p)
	turn, err := h.builder.Apply(context.Background(), p)
	if err != nil {
		h.t.Fatalf("apply %s: %v", p.RequestID, err)
	}
	return turn
}

func (h *harness) state() *State {
	h.t.Helper()
	st, err := h.states.Get(context.Background(), testTenant, testConv)
	if err != nil || st == nil {
		h.t.Fatalf("state missing: %v", err)
	}
	return st
}

func (h *harness) page(fromSeq, limit int) *TranscriptPage {
	h.t.Helper()
	st := h.state()
	turns, err := h.states.ListTurns(context.Background(), st.TranscriptID, st.Generation)
	if err != nil {
		h.t.Fatal(err)
	}
	page, err := h.reader.Page(context.Background(), st, turns, fromSeq, limit)
	if err != nil {
		h.t.Fatal(err)
	}
	return page
}

// conversation builds Chat Completions captures the way Pi does: every
// request resends all prior messages.
type conversation struct {
	system   string
	messages []map[string]any
	seq      int
}

func (c *conversation) user(text string) {
	c.messages = append(c.messages, map[string]any{"role": "user", "content": text})
}

func (c *conversation) request(reply string, toolCall bool) *capture.Payload {
	c.seq++
	msgs := []map[string]any{}
	if c.system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": c.system})
	}
	msgs = append(msgs, c.messages...)
	body, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": msgs})

	assistant := map[string]any{"role": "assistant", "content": reply}
	var resp []string
	resp = append(resp, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, reply))
	if toolCall {
		callID := fmt.Sprintf("call_%d", c.seq)
		resp = append(resp, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":"bash","arguments":"{}"}}]}}]}`, callID))
		assistant["tool_calls"] = []map[string]any{{"id": callID, "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}}}
	}
	// The next request carries this reply (and a tool result when a tool ran).
	c.messages = append(c.messages, assistant)
	if toolCall {
		c.messages = append(c.messages, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", c.seq), "content": "output"})
	}
	return &capture.Payload{
		RequestID: fmt.Sprintf("req_%03d", c.seq), ConversationID: testConv, TenantID: testTenant, UserID: "usr_1",
		Provider: "deepseek-completions", Model: "m", Status: "success", Streaming: true,
		RequestBody: body, ResponseBody: sse(resp...), ResponseFormat: "sse",
		CreatedAt: testEpoch.Add(time.Duration(c.seq) * time.Minute),
	}
}

func TestApplyExtendsWithDeltasAndMarksEchoes(t *testing.T) {
	h := newHarness(t, 1000)
	c := &conversation{system: "you are pi"}
	c.user("fix the bug")
	h.capture(c.request("looking", true))
	h.capture(c.request("found it", true))
	c.user("thanks, now test it")
	h.capture(c.request("done", false))

	st := h.state()
	if st.RequestCount != 3 || st.UserTurnCount != 2 || st.ToolCallCount != 2 || st.ResetCount != 0 {
		t.Fatalf("counters: %+v", st)
	}
	if st.OpeningPrompt != "fix the bug" || st.LastPrompt != "thanks, now test it" {
		t.Fatalf("prompts: opening=%q last=%q", st.OpeningPrompt, st.LastPrompt)
	}
	turns, _ := h.states.ListTurns(context.Background(), st.TranscriptID, st.Generation)
	wantBase := []int{0, 1, 3}
	wantDelta := []int{1, 2, 3}
	for i, turn := range turns {
		if turn.Kind != KindExtend || turn.Base != wantBase[i] || turn.DeltaCount != wantDelta[i] {
			t.Errorf("turn %d: kind=%s base=%d delta=%d, want extend/%d/%d", turn.Seq, turn.Kind, turn.Base, turn.DeltaCount, wantBase[i], wantDelta[i])
		}
	}
	if !turns[0].SystemChanged || turns[1].SystemChanged {
		t.Errorf("system change flags: %v %v", turns[0].SystemChanged, turns[1].SystemChanged)
	}

	page := h.page(1, 10)
	if len(page.Turns) != 3 || page.NextSeq != nil || page.LastSeq != 3 {
		t.Fatalf("page: %d turns next=%v last=%d", len(page.Turns), page.NextSeq, page.LastSeq)
	}
	second := page.Turns[1]
	if len(second.Items) != 3 || !second.Items[0].Echo || !second.Items[1].Echo || second.Items[2].Echo {
		t.Fatalf("turn 2 items/echo: %+v", second.Items)
	}
	if second.Items[2].Kind != DisplayToolResult || *second.Items[2].Index != 2 {
		t.Fatalf("turn 2 tool result: %+v", second.Items[2])
	}
	if page.Turns[0].System == nil || *page.Turns[0].System != "you are pi" || page.Turns[1].System != nil {
		t.Fatal("system text must be stored only when it changes")
	}
	if got := page.Turns[2].Output; len(got) != 1 || got[0].Text != "done" {
		t.Fatalf("turn 3 output: %+v", got)
	}
}

func TestApplyHandlesResetPrefixAppendUnparsedAndDuplicates(t *testing.T) {
	h := newHarness(t, 1000)
	c := &conversation{}
	c.user("start")
	var captured []*capture.Payload
	for i := 0; i < 5; i++ {
		captured = append(captured, c.request(fmt.Sprintf("step %d", i), true))
	}
	for _, p := range captured[:3] {
		h.capture(p)
	}
	// Out of order: request 5 lands before request 4.
	h.capture(captured[4])
	late := h.capture(captured[3])
	if late.Kind != KindPrefix || late.DeltaCount != 0 || late.OutputCount != 1 {
		t.Fatalf("late request: %+v", late)
	}
	if h.state().LineCount() != len(c.messages)-2 {
		t.Fatalf("a prefix request must not shorten the line: %d", h.state().LineCount())
	}

	// Duplicate delivery of the most recent request is ignored.
	if dup, err := h.builder.Apply(context.Background(), captured[3]); err != nil || dup != nil {
		t.Fatalf("duplicate apply = %+v, %v", dup, err)
	}

	// Compaction: history replaced by a summary plus the latest message.
	c.messages = []map[string]any{{"role": "user", "content": "summary of earlier work"}, {"role": "user", "content": "continue"}}
	reset := h.capture(c.request("resuming", false))
	if reset.Kind != KindReset || reset.Base != 0 || reset.DeltaCount != 2 {
		t.Fatalf("compaction: %+v", reset)
	}
	after := h.capture(c.request("more", false))
	if after.Kind != KindExtend || after.Base != 2 {
		t.Fatalf("after compaction: %+v", after)
	}

	// Truncated body: unparsed, line untouched.
	bad := c.request("x", false)
	bad.RequestBody = bad.RequestBody[:len(bad.RequestBody)/2]
	bad.RequestTruncated = true
	lineBefore := h.state().LineCount()
	unparsed := h.capture(bad)
	if unparsed.Kind != KindUnparsed || h.state().LineCount() != lineBefore {
		t.Fatalf("unparsed: %+v line %d -> %d", unparsed, lineBefore, h.state().LineCount())
	}

	// previous_response_id: input holds only new items.
	appendBody, _ := json.Marshal(map[string]any{"previous_response_id": "resp_1", "input": []map[string]any{{"role": "user", "content": "delta only"}}})
	appended := h.capture(&capture.Payload{
		RequestID: "req_append", ConversationID: testConv, TenantID: testTenant, Provider: "openai-codex-responses",
		Status: "error", RequestBody: appendBody, ResponseBody: []byte(`{"error":"overloaded"}`), ResponseFormat: "json",
		CreatedAt: testEpoch.Add(time.Hour),
	})
	if appended.Kind != KindAppend || appended.Base != lineBefore || appended.InputCount != lineBefore+1 {
		t.Fatalf("append: %+v (line was %d)", appended, lineBefore)
	}

	st := h.state()
	if st.ResetCount != 1 || st.ErrorCount != 1 || st.API != APIResponses {
		t.Fatalf("state after mixed kinds: resets=%d errors=%d api=%s", st.ResetCount, st.ErrorCount, st.API)
	}
	page := h.page(1, 200)
	last := page.Turns[len(page.Turns)-1]
	if last.Error != `{"error":"overloaded"}` || len(last.Output) != 0 {
		t.Fatalf("error turn rendered as %+v", last)
	}
	for _, turn := range page.Turns {
		if turn.Kind == KindReset {
			for _, item := range turn.Items {
				if item.Echo {
					t.Fatal("reset turns must not mark echoes")
				}
			}
		}
	}
}

func TestSystemChangeIsRecordedWithoutResettingTheLine(t *testing.T) {
	h := newHarness(t, 1000)
	c := &conversation{system: "v1"}
	c.user("q")
	h.capture(c.request("a", false))
	c.system = "v2 with memory"
	c.user("q2")
	turn := h.capture(c.request("b", false))
	if turn.Kind != KindExtend || !turn.SystemChanged {
		t.Fatalf("system change: %+v", turn)
	}
	if got := h.page(2, 1).Turns[0].System; got == nil || *got != "v2 with memory" {
		t.Fatalf("system text = %v", got)
	}
}

func TestMergeProducesTheSameTranscriptAndCleansSegments(t *testing.T) {
	build := func(merge int) (*harness, []byte) {
		h := newHarness(t, merge)
		c := &conversation{system: "s"}
		c.user("go")
		for i := 0; i < 7; i++ {
			if i == 4 {
				c.user("follow up")
			}
			h.capture(c.request(fmt.Sprintf("r%d", i), i%2 == 0))
		}
		page, _ := json.Marshal(h.page(1, 200))
		return h, page
	}
	plainH, plain := build(1000)
	mergedH, merged := build(3)
	if string(plain) != string(merged) {
		t.Fatalf("merged transcript differs:\nplain  %s\nmerged %s", plain, merged)
	}

	st := mergedH.state()
	if st.MergedThroughSeq != 6 || st.LastSeq != 7 || st.MergedKey == "" {
		t.Fatalf("merge state: through=%d last=%d key=%q", st.MergedThroughSeq, st.LastSeq, st.MergedKey)
	}
	keys := mergedH.objects.Keys()
	if len(keys) != 2 || !strings.HasSuffix(keys[0], "merged-000006.jsonl.gz") || !strings.HasSuffix(keys[1], "seg-000007.jsonl.gz") {
		t.Fatalf("objects after merge: %v", keys)
	}
	if st.TranscriptBytes() != mergedH.objects.TotalBytes() {
		t.Fatalf("transcript bytes %d != stored %d", st.TranscriptBytes(), mergedH.objects.TotalBytes())
	}
	if len(plainH.objects.Keys()) != 7 {
		t.Fatalf("unmerged run should keep 7 segments, got %d", len(plainH.objects.Keys()))
	}

	// Paging across the merged/tail boundary.
	first := mergedH.page(1, 5)
	if len(first.Turns) != 5 || first.NextSeq == nil || *first.NextSeq != 6 {
		t.Fatalf("first page: %d turns next=%v", len(first.Turns), first.NextSeq)
	}
	rest := mergedH.page(*first.NextSeq, 5)
	if len(rest.Turns) != 2 || rest.NextSeq != nil || rest.Turns[1].Seq != 7 {
		t.Fatalf("second page: %+v", rest)
	}
}

func TestRebuildRegeneratesAndSwapsGeneration(t *testing.T) {
	h := newHarness(t, 3)
	c := &conversation{}
	c.user("hello")
	for i := 0; i < 5; i++ {
		h.capture(c.request(fmt.Sprintf("r%d", i), true))
	}
	before, _ := json.Marshal(h.page(1, 200).Turns)
	oldKeys := h.objects.Keys()

	late := c.request("arrived during rebuild", false)
	injected := false
	h.payloads.onLoad = func(requestID string) {
		if requestID == "req_005" && !injected {
			injected = true
			h.capture(late) // applied to the old generation mid-rebuild
		}
	}
	st, err := h.builder.Rebuild(context.Background(), testTenant, testConv)
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != 2 || st.RequestCount != 6 || st.NeedsRebuild {
		t.Fatalf("rebuilt state: gen=%d requests=%d needs=%v", st.Generation, st.RequestCount, st.NeedsRebuild)
	}
	stored := h.state()
	if stored.Generation != 2 || stored.LastSeq != 6 {
		t.Fatalf("stored state: gen=%d last=%d", stored.Generation, stored.LastSeq)
	}
	oldTurns, _ := h.states.ListTurns(context.Background(), stored.TranscriptID, 1)
	if len(oldTurns) != 0 {
		t.Fatalf("old generation turns not deleted: %d", len(oldTurns))
	}
	for _, key := range h.objects.Keys() {
		for _, old := range oldKeys {
			if key == old {
				t.Fatalf("old generation object %s survived the swap", key)
			}
		}
		if !strings.Contains(key, "/g2-") {
			t.Fatalf("unexpected object after rebuild: %s", key)
		}
	}
	after, _ := json.Marshal(h.page(1, 200).Turns[:5])
	if string(before) != string(after) {
		t.Fatalf("rebuild changed the transcript:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRebuildRaceKeepsWinnerAndDiscardsLoserObjects(t *testing.T) {
	h := newHarness(t, 1000)
	c := &conversation{}
	c.user("hello")
	for i := 0; i < 3; i++ {
		h.capture(c.request("r", false))
	}
	h.payloads.onLoad = func(requestID string) {
		if requestID == "req_003" {
			h.states.SetGeneration(testTenant, testConv, 7) // another process won
		}
	}
	keysBefore := h.objects.Keys()
	st, err := h.builder.Rebuild(context.Background(), testTenant, testConv)
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != 7 {
		t.Fatalf("lost race should return the winner's state, got generation %d", st.Generation)
	}
	if got := h.objects.Keys(); !reflect.DeepEqual(got, keysBefore) {
		t.Fatalf("loser left objects behind: %v", got)
	}
}

func TestNewTranscriptForOlderConversationNeedsRebuild(t *testing.T) {
	h := newHarness(t, 1000)
	c := &conversation{}
	c.user("hi")
	old := c.request("a", false)
	h.payloads.add(old) // captured before the archive existed
	h.capture(c.request("b", false))
	st := h.state()
	if !st.NeedsRebuild || st.RebuildRequestedAt == nil {
		t.Fatalf("transcript started mid-conversation must request a rebuild: %+v", st)
	}
	rebuilt, err := h.builder.Rebuild(context.Background(), testTenant, testConv)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.NeedsRebuild || rebuilt.RequestCount != 2 {
		t.Fatalf("rebuild must cover both captures and clear the flag: %+v", rebuilt)
	}
}

// locatedObjects is a memory store reporting a configurable location, to
// simulate switching object backends.
type locatedObjects struct {
	*blob.Memory
	location string
}

func (l *locatedObjects) Location() string { return l.location }

func TestBackendSwitchRefusesForeignTranscriptsUntilRebuilt(t *testing.T) {
	ctx := context.Background()
	states := NewMemoryStateStore()
	payloads := &memoryPayloads{}
	oldStore := &locatedObjects{Memory: blob.NewMemory(), location: "postgres"}
	c := &conversation{}
	c.user("hi")
	for i := 0; i < 3; i++ {
		p := c.request("r", false)
		payloads.add(p)
		if _, err := NewBuilder(oldStore, states, payloads, BuilderOptions{}).Apply(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	newStore := &locatedObjects{Memory: blob.NewMemory(), location: "gcs://bucket/prod"}
	builder := NewBuilder(newStore, states, payloads, BuilderOptions{})
	next := c.request("after switch", false)
	payloads.add(next)
	if _, err := builder.Apply(ctx, next); !errors.Is(err, errForeignObjectStore) {
		t.Fatalf("appending to a transcript from another store: err = %v", err)
	}
	if len(newStore.Keys()) != 0 {
		t.Fatalf("foreign append wrote objects: %v", newStore.Keys())
	}
	st, _ := states.Get(ctx, testTenant, testConv)
	if st.StoredIn(newStore.Location()) || !st.StoredIn("postgres") {
		t.Fatalf("StoredIn wrong for %q", st.ObjectsLocation)
	}

	rebuilt, err := builder.Rebuild(ctx, testTenant, testConv)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ObjectsLocation != newStore.Location() || rebuilt.RequestCount != 4 {
		t.Fatalf("rebuilt state: location %q requests %d", rebuilt.ObjectsLocation, rebuilt.RequestCount)
	}
	after := c.request("appends again", false)
	payloads.add(after)
	if turn, err := builder.Apply(ctx, after); err != nil || turn == nil || turn.Kind != KindExtend {
		t.Fatalf("append after rebuild: %+v %v", turn, err)
	}

	// Rows created by MarkNeedsRebuild have no location and no objects yet.
	if !(&State{}).StoredIn("anything") {
		t.Fatal("a state without a location must match any store")
	}
}

type failingObjects struct {
	*blob.Memory
	fail bool
}

func (f *failingObjects) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if f.fail {
		return errors.New("bucket unavailable")
	}
	return f.Memory.Put(ctx, key, data, contentType)
}

func TestApplyFailureCommitsNothing(t *testing.T) {
	objects := &failingObjects{Memory: blob.NewMemory(), fail: true}
	states := NewMemoryStateStore()
	builder := NewBuilder(objects, states, nil, BuilderOptions{})
	c := &conversation{}
	c.user("hi")
	if _, err := builder.Apply(context.Background(), c.request("a", false)); err == nil {
		t.Fatal("apply must surface object store failures")
	}
	if st, _ := states.Get(context.Background(), testTenant, testConv); st != nil {
		t.Fatalf("failed apply committed state: %+v", st)
	}
	if turn, err := builder.Apply(context.Background(), &capture.Payload{RequestID: "no-conv"}); turn != nil || err != nil {
		t.Fatalf("captures without a conversation id must be skipped, got %+v %v", turn, err)
	}
	if _, err := builder.Rebuild(context.Background(), testTenant, testConv); err == nil {
		t.Fatal("rebuild without a payload source must fail")
	}
}
