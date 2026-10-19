package sessionarchive

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseRequestResponsesSplitsInstructionsAndStringInput(t *testing.T) {
	body := `{"model":"gpt","instructions":"be terse","input":[
		{"role":"developer","content":[{"type":"input_text","text":"dev rules"}]},
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[],"encrypted_content":"xyz"},
		{"type":"function_call","name":"read","call_id":"c1","arguments":"{\"path\":\"a\"}"},
		{"type":"function_call_output","call_id":"c1","output":"file"}
	]}`
	req, err := ParseRequest("openai-codex-responses", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.API != APIResponses || req.Append {
		t.Fatalf("api=%s append=%v", req.API, req.Append)
	}
	if req.System != "be terse\n\ndev rules" {
		t.Fatalf("system = %q", req.System)
	}
	if len(req.Items) != 4 {
		t.Fatalf("items = %d, want 4 (leading developer item split off)", len(req.Items))
	}
	if bytes.Contains(req.Items[0], []byte("\n")) || bytes.Contains(req.Items[0], []byte("\t")) {
		t.Fatalf("items must be compacted: %s", req.Items[0])
	}

	str, err := ParseRequest("x", []byte(`{"input":"just text","previous_response_id":"resp_1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !str.Append || len(str.Items) != 1 || string(str.Items[0]) != `{"content":"just text","role":"user"}` {
		t.Fatalf("string input parsed as %+v / %s", str, str.Items)
	}
}

func TestParseRequestChatAndAnthropic(t *testing.T) {
	chat, err := ParseRequest("deepseek-completions", []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":[{"type":"text","text":"q"}]},
		{"role":"system","content":"mid-conversation system stays"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if chat.API != APIChat || chat.System != "sys" || len(chat.Items) != 2 {
		t.Fatalf("chat parsed as api=%s system=%q items=%d", chat.API, chat.System, len(chat.Items))
	}

	anth, err := ParseRequest("anthropic", []byte(`{"system":[{"type":"text","text":"s1"}],"messages":[{"role":"user","content":"q"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if anth.API != APIAnthropic || anth.System != "s1" || len(anth.Items) != 1 {
		t.Fatalf("anthropic parsed as %+v", anth)
	}

	for _, bad := range []string{`{"messages":`, `{"model":"x"}`, `[]`, ``} {
		if _, err := ParseRequest("x", []byte(bad)); err == nil {
			t.Errorf("ParseRequest(%q) succeeded, want error", bad)
		}
	}
}

func sse(events ...string) []byte {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: ")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}

func TestParseResponseOutputResponsesSSE(t *testing.T) {
	body := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"think\"}]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\"}]}}\n\n")
	out := ParseResponseOutput(APIResponses, "sse", body)
	if len(out) != 2 {
		t.Fatalf("outputs = %d, want 2 output_item.done items", len(out))
	}
	if d := NormalizeItem(out[1]); len(d) != 1 || d[0].Kind != DisplayAssistant || d[0].Text != "Hello" {
		t.Fatalf("assistant output rendered as %+v", d)
	}

	cut := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n")
	out = ParseResponseOutput(APIUnknown, "text", cut)
	if len(out) != 1 || NormalizeItem(out[0])[0].Text != "partial" {
		t.Fatalf("truncated stream fallback = %s", out)
	}
}

func TestParseResponseOutputChatSSEAssemblesToolCalls(t *testing.T) {
	body := sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"plan "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"it"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"write","arguments":"{\"x\""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":":1}"}}]}}]}`,
		`{"choices":[],"usage":{"total_tokens":5}}`,
	)
	out := ParseResponseOutput(APIChat, "sse", body)
	if len(out) != 1 {
		t.Fatalf("outputs = %d, want one assistant message", len(out))
	}
	var msg chatAssistantMessage
	if err := json.Unmarshal(out[0], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Content != "ok" || msg.ReasoningContent != "plan it" || len(msg.ToolCalls) != 2 {
		t.Fatalf("assembled %+v", msg)
	}
	if msg.ToolCalls[0].Function.Name != "read" || msg.ToolCalls[1].Function.Arguments != `{"x":1}` {
		t.Fatalf("tool calls out of order or not concatenated: %+v", msg.ToolCalls)
	}

	if out := ParseResponseOutput(APIChat, "sse", sse(`{"choices":[{"index":0,"delta":{}}]}`)); out != nil {
		t.Fatalf("empty stream produced %s", out)
	}
	if out := ParseResponseOutput(APIChat, "json", []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)); len(out) != 1 {
		t.Fatalf("chat json output = %s", out)
	}
	if out := ParseResponseOutput(APIChat, "json", []byte(`{"error":{"message":"rate limited"}}`)); out != nil {
		t.Fatalf("error body produced output %s", out)
	}
}

func TestParseResponseOutputAnthropic(t *testing.T) {
	body := sse(
		`{"type":"message_start","message":{}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
	)
	out := ParseResponseOutput(APIAnthropic, "sse", body)
	if len(out) != 1 {
		t.Fatalf("outputs = %d", len(out))
	}
	d := NormalizeItem(out[0])
	if len(d) != 3 || d[0].Kind != DisplayReasoning || d[1].Kind != DisplayAssistant || d[2].Kind != DisplayToolCall || d[2].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("anthropic stream rendered as %+v", d)
	}
	if out := ParseResponseOutput(APIAnthropic, "json", []byte(`{"content":[{"type":"text","text":"x"}]}`)); len(out) != 1 {
		t.Fatalf("anthropic json output = %s", out)
	}
}

func TestNormalizeItemShapes(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		kinds []string
	}{
		{"responses user", `{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:"}]}`, []string{DisplayUser}},
		{"responses encrypted reasoning", `{"type":"reasoning","summary":[],"encrypted_content":"abc"}`, []string{DisplayReasoning}},
		{"responses function call", `{"type":"function_call","name":"bash","call_id":"c","arguments":"{\"cmd\":\"ls\"}"}`, []string{DisplayToolCall}},
		{"responses function output", `{"type":"function_call_output","call_id":"c","output":"ok"}`, []string{DisplayToolResult}},
		{"chat assistant", `{"role":"assistant","content":"a","reasoning_content":"r","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}`, []string{DisplayReasoning, DisplayAssistant, DisplayToolCall}},
		{"chat assistant null content", `{"role":"assistant","content":null,"tool_calls":[{"id":"1","function":{"name":"f","arguments":"{}"}}]}`, []string{DisplayToolCall}},
		{"chat tool", `{"role":"tool","tool_call_id":"1","content":"out"}`, []string{DisplayToolResult}},
		{"anthropic tool result", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"r"}]},{"type":"text","text":"next"}]}`, []string{DisplayToolResult, DisplayUser}},
		{"unknown item type", `{"type":"web_search_call","status":"done"}`, []string{DisplayOther}},
		{"not json", `nope`, []string{DisplayOther}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeItem(json.RawMessage(tc.raw))
			kinds := make([]string, len(got))
			for i, d := range got {
				kinds[i] = d.Kind
			}
			if strings.Join(kinds, ",") != strings.Join(tc.kinds, ",") {
				t.Fatalf("kinds = %v, want %v (%+v)", kinds, tc.kinds, got)
			}
		})
	}

	enc := NormalizeItem(json.RawMessage(`{"type":"reasoning","summary":[],"encrypted_content":"abc"}`))
	if !enc[0].Encrypted {
		t.Error("encrypted reasoning without summary must be flagged")
	}
	user := NormalizeItem(json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:"}]}`))
	if user[0].Text != "hi\n[image]" {
		t.Errorf("image placeholder text = %q", user[0].Text)
	}
	big := NormalizeItem(json.RawMessage(`{"role":"tool","tool_call_id":"1","content":"` + strings.Repeat("é", displayTextMaxRunes+5) + `"}`))
	if !big[0].Truncated || len([]rune(big[0].Text)) != displayTextMaxRunes {
		t.Errorf("long tool output not truncated to %d runes: truncated=%v runes=%d", displayTextMaxRunes, big[0].Truncated, len([]rune(big[0].Text)))
	}
}

func TestChainHashesAndCommonPrefix(t *testing.T) {
	items := func(values ...string) []json.RawMessage {
		out := make([]json.RawMessage, len(values))
		for i, v := range values {
			out[i] = json.RawMessage(`"` + v + `"`)
		}
		return out
	}
	a := extendChain(nil, items("a", "b", "c"))
	if len(a) != 3*hashSize {
		t.Fatalf("chain length %d", len(a))
	}
	if got := extendChain(extendChain(nil, items("a")), items("b", "c")); !bytes.Equal(got, a) {
		t.Fatal("extending a chain must equal hashing the whole line")
	}
	if n := commonPrefixItems(a, extendChain(nil, items("a", "b", "x", "y"))); n != 2 {
		t.Fatalf("common prefix = %d, want 2", n)
	}
	if n := commonPrefixItems(a, extendChain(nil, items("z", "b", "c"))); n != 0 {
		t.Fatalf("a changed first item must break the whole chain, got %d", n)
	}
	if n := commonPrefixItems(a, nil); n != 0 {
		t.Fatalf("prefix with empty = %d", n)
	}
	if systemDigest("") != "" || systemDigest("x") == "" {
		t.Fatal("system digest of empty text must be empty")
	}
}

func TestSegmentRoundTripAndConcatenation(t *testing.T) {
	system := "sys"
	first := &SegmentTurn{
		Seq: 1, RequestID: "r1", CreatedAt: time.Date(2026, 9, 11, 1, 2, 3, 0, time.UTC), Provider: "p", Model: "m",
		Status: "success", API: APIChat, Kind: KindExtend, Base: 0, InputCount: 2, System: &system,
		Inputs:  []IndexedItem{{Index: 0, Raw: json.RawMessage(`{"role":"user","content":"<a&b>"}`)}, {Index: 1, Raw: json.RawMessage(`{"role":"user","content":"x"}`)}},
		Outputs: []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"y"}`)},
	}
	second := &SegmentTurn{Seq: 2, RequestID: "r2", Kind: KindUnparsed, Base: 2, InputCount: 2, Status: "error", Error: "boom", RequestTruncated: true}

	a, err := encodeSegment(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeSegment(second)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	zr, _ := gzip.NewReader(bytes.NewReader(a))
	_, _ = raw.ReadFrom(zr)
	if !bytes.Contains(raw.Bytes(), []byte(`<a&b>`)) {
		t.Error("segment JSON must not HTML-escape content")
	}

	turns, err := decodeTranscript(append(append([]byte(nil), a...), b...))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("decoded %d turns from concatenated members, want 2", len(turns))
	}
	got := turns[0]
	if got.RequestID != "r1" || !got.CreatedAt.Equal(first.CreatedAt) || got.System == nil || *got.System != "sys" ||
		len(got.Inputs) != 2 || got.Inputs[1].Index != 1 || len(got.Outputs) != 1 {
		t.Fatalf("first turn decoded as %+v", got)
	}
	if turns[1].Error != "boom" || !turns[1].RequestTruncated || turns[1].System != nil || turns[1].Kind != KindUnparsed {
		t.Fatalf("second turn decoded as %+v", turns[1])
	}
}
