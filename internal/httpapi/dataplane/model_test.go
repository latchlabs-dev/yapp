package dataplane

import (
	"encoding/json"
	"testing"
)

func TestNormalizeModelStripsPrefixAndRewritesBody(t *testing.T) {
	body := []byte(`{"model":"corp-deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, model := NormalizeModel(body, "corp-deepseek-v4-flash")

	if model != "deepseek-v4-flash" {
		t.Errorf("routing model = %q, want deepseek-v4-flash", model)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("rewritten body not valid JSON: %v", err)
	}
	var m string
	_ = json.Unmarshal(parsed["model"], &m)
	if m != "deepseek-v4-flash" {
		t.Errorf("body model = %q, want deepseek-v4-flash", m)
	}
	// Other fields preserved.
	if _, ok := parsed["messages"]; !ok {
		t.Error("messages field lost in rewrite")
	}
	if _, ok := parsed["stream"]; !ok {
		t.Error("stream field lost in rewrite")
	}
}

func TestNormalizeModelLeavesUnprefixedUntouched(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","x":1}`)
	out, model := NormalizeModel(body, "deepseek-v4-flash")
	if model != "deepseek-v4-flash" {
		t.Errorf("model = %q", model)
	}
	if string(out) != string(body) {
		t.Errorf("body should be byte-identical when no prefix; got %s", out)
	}
}

func TestNormalizeModelEmptyModel(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	out, model := NormalizeModel(body, "")
	if model != "" || string(out) != string(body) {
		t.Errorf("empty model should pass through unchanged")
	}
}
