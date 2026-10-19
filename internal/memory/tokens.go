package memory

import "unicode/utf8"

// EstimateTokens approximates the token count of a string. LLM tokenizers
// average roughly 4 characters per token for English/code; this deliberately
// cheap estimate is used only for budgeting, never billing.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (utf8.RuneCountInString(s) + 3) / 4
}

// TruncateToTokens cuts s so its estimated token count fits within budget,
// appending a truncation marker when content was dropped. keepTail keeps the
// end of the string instead of the beginning (useful for transcripts, where
// late turns carry resolutions).
func TruncateToTokens(s string, budget int, keepTail bool) string {
	if budget <= 0 {
		return ""
	}
	if EstimateTokens(s) <= budget {
		return s
	}

	const marker = "\n…[truncated]\n"
	runes := []rune(s)
	keep := budget * 4
	if keep >= len(runes) {
		return s
	}
	if keepTail {
		return marker + string(runes[len(runes)-keep:])
	}
	return string(runes[:keep]) + marker
}
