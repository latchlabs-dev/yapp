package memory

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
	if got := EstimateTokens("abcd"); got != 1 {
		t.Errorf("4 chars = %d tokens, want 1", got)
	}
	if got := EstimateTokens(strings.Repeat("a", 400)); got != 100 {
		t.Errorf("400 chars = %d tokens, want 100", got)
	}
}

func TestTruncateToTokens(t *testing.T) {
	s := strings.Repeat("x", 400) // ~100 tokens

	if got := TruncateToTokens(s, 200, false); got != s {
		t.Error("under-budget string should be unchanged")
	}

	head := TruncateToTokens(s, 50, false)
	if EstimateTokens(head) > 60 {
		t.Errorf("head truncation too long: %d tokens", EstimateTokens(head))
	}
	if !strings.Contains(head, "truncated") {
		t.Error("head truncation should include marker")
	}
	if !strings.HasPrefix(head, "x") {
		t.Error("head truncation should keep the beginning")
	}

	tail := TruncateToTokens(s, 50, true)
	if !strings.HasSuffix(tail, "x") {
		t.Error("tail truncation should keep the end")
	}
	if !strings.Contains(tail, "truncated") {
		t.Error("tail truncation should include marker")
	}

	if TruncateToTokens(s, 0, false) != "" {
		t.Error("zero budget should produce empty string")
	}
}
