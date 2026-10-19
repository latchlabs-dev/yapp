package dataplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/hooks"
)

func TestExtractHookContentOpenAI(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"}
	]}`)

	content := extractHookContent(body)
	if content == nil {
		t.Fatal("expected content")
	}
	if content.System != "be brief" {
		t.Errorf("system = %q, want %q", content.System, "be brief")
	}
	if len(content.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system is lifted out)", len(content.Messages))
	}
	if content.Messages[0].Role != "user" || content.Messages[0].Text != "hello" {
		t.Errorf("first message = %+v", content.Messages[0])
	}
}

func TestExtractHookContentAnthropic(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"system":"be brief","messages":[
		{"role":"user","content":[{"type":"text","text":"hello"}]}
	]}`)

	content := extractHookContent(body)
	if content == nil {
		t.Fatal("expected content")
	}
	if content.System != "be brief" {
		t.Errorf("system = %q", content.System)
	}
	if len(content.Messages) != 1 || content.Messages[0].Text != "hello" {
		t.Fatalf("messages = %+v", content.Messages)
	}
}

// A hook must be told an image was present without receiving its bytes: the
// marker is what lets a guardrail reason about attachments at all.
func TestExtractHookContentMarksNonTextParts(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}
	]}`)

	content := extractHookContent(body)
	if content == nil || len(content.Messages) != 1 {
		t.Fatalf("content = %+v", content)
	}
	text := content.Messages[0].Text
	if !strings.Contains(text, "what is this") || !strings.Contains(text, "[image]") {
		t.Errorf("text = %q, want the prompt plus an [image] marker", text)
	}
	if strings.Contains(text, "AAAA") {
		t.Error("image bytes leaked into the hook payload")
	}
}

func TestApplyHookRewritePrependsOpenAISystemMessage(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)

	rewritten, err := applyHookRewrite(body, &hooks.Decision{SystemPrepend: "POLICY"})
	if err != nil {
		t.Fatalf("apply rewrite: %v", err)
	}
	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "POLICY" {
		t.Errorf("first message = %+v, want the injected system message", out.Messages[0])
	}
}

// Anthropic rejects system messages in the messages array, so the prepend has
// to become the top-level system field there instead.
func TestApplyHookRewriteSetsAnthropicSystemField(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`)

	rewritten, err := applyHookRewrite(body, &hooks.Decision{SystemPrepend: "POLICY"})
	if err != nil {
		t.Fatalf("apply rewrite: %v", err)
	}
	var out struct {
		System   string `json:"system"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if out.System != "POLICY" {
		t.Errorf("system = %q, want POLICY", out.System)
	}
	if len(out.Messages) != 1 {
		t.Errorf("messages = %d, want the original single message", len(out.Messages))
	}
}

func TestApplyHookRewriteJoinsExistingAnthropicSystem(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":100,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`)

	rewritten, err := applyHookRewrite(body, &hooks.Decision{SystemPrepend: "POLICY"})
	if err != nil {
		t.Fatalf("apply rewrite: %v", err)
	}
	var out struct {
		System string `json:"system"`
	}
	_ = json.Unmarshal(rewritten, &out)
	if !strings.HasPrefix(out.System, "POLICY") || !strings.Contains(out.System, "be brief") {
		t.Errorf("system = %q, want POLICY ahead of the original", out.System)
	}
}

func TestApplyHookRewriteRedactsMessageText(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"my card is 4111111111111111"}
	]}`)

	rewritten, err := applyHookRewrite(body, &hooks.Decision{
		RewrittenMessages: []hooks.ContentMessage{{Role: "user", Text: "my card is [REDACTED]"}},
	})
	if err != nil {
		t.Fatalf("apply rewrite: %v", err)
	}
	if strings.Contains(string(rewritten), "4111111111111111") {
		t.Fatal("the original card number survived the rewrite")
	}
	if !strings.Contains(string(rewritten), "[REDACTED]") {
		t.Fatal("the redacted text is missing")
	}
	if !strings.Contains(string(rewritten), "be brief") {
		t.Fatal("the system message was dropped by a rewrite that did not target it")
	}
}

// A rewrite carries text only. It must never be applied in a way that discards
// an attached image, so a multimodal message keeps every non-text part.
func TestApplyHookRewritePreservesAttachments(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":[
			{"type":"text","text":"secret 4111111111111111"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}
	]}`)

	rewritten, err := applyHookRewrite(body, &hooks.Decision{
		RewrittenMessages: []hooks.ContentMessage{{Role: "user", Text: "secret [REDACTED]"}},
	})
	if err != nil {
		t.Fatalf("apply rewrite: %v", err)
	}
	out := string(rewritten)
	if strings.Contains(out, "4111111111111111") {
		t.Error("the original text survived the rewrite")
	}
	if !strings.Contains(out, "image_url") || !strings.Contains(out, "AAAA") {
		t.Error("the image part was discarded by a text-only rewrite")
	}
}

// A rewrite whose shape does not line up with the request is refused outright.
// Applying it partially would silently drop turns.
func TestApplyHookRewriteRejectsShapeMismatch(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"one"},
		{"role":"assistant","content":"two"}
	]}`)

	cases := map[string][]hooks.ContentMessage{
		"too few":     {{Role: "user", Text: "one"}},
		"wrong roles": {{Role: "assistant", Text: "one"}, {Role: "user", Text: "two"}},
		"too many": {
			{Role: "user", Text: "one"},
			{Role: "assistant", Text: "two"},
			{Role: "user", Text: "three"},
		},
	}
	for name, rewrite := range cases {
		if _, err := applyHookRewrite(body, &hooks.Decision{RewrittenMessages: rewrite}); err == nil {
			t.Errorf("%s: expected the rewrite to be refused", name)
		}
	}
}

func TestApplyHookRewriteIsANoOpWithoutMutations(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`)

	out, err := applyHookRewrite(body, &hooks.Decision{Abort: false, Message: "fine"})
	if err != nil {
		t.Fatalf("apply rewrite: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed without a mutation: %s", out)
	}
}

func TestCapHookContentDropsOldestFirst(t *testing.T) {
	big := strings.Repeat("x", hookContentBudget/2)
	content := capHookContent(&hooks.EventContent{
		Messages: []hooks.ContentMessage{
			{Role: "user", Text: big},
			{Role: "assistant", Text: big},
			{Role: "user", Text: big},
		},
	})
	if !content.Truncated {
		t.Error("expected Truncated to be set when messages were dropped")
	}
	if len(content.Messages) >= 3 {
		t.Errorf("kept %d messages, expected the oldest to be dropped", len(content.Messages))
	}
	// What survives must be the newest turns: a guardrail cares most about what
	// was just said.
	if len(content.Messages) > 0 && content.Messages[len(content.Messages)-1].Role != "user" {
		t.Error("the most recent message was not retained")
	}
}
