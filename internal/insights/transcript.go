package insights

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	insightsv1 "github.com/earlgrey/piproxy/internal/insightsrpc/v1"
)

const maxItemChars = 20000

// ParseTranscript converts canonical Pi session JSONL into the service-facing
// transcript. Parsing remains a PiProxy responsibility, so processors never
// need to understand Pi's storage format or raw tool/thinking records.
func ParseTranscript(raw []byte, sessionID, projectID string, maxItems, maxChars int) *insightsv1.ParsedTranscript {
	if maxItems <= 0 {
		maxItems = 1000
	}
	if maxChars <= 0 {
		maxChars = 500000
	}
	items := make([]*insightsv1.TranscriptItem, 0, min(maxItems, 128))
	lineNumber := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		lineNumber++
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Timestamp string          `json:"timestamp"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Message   struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "message" {
			continue
		}
		role, content := entry.Message.Role, entry.Message.Content
		if role == "" {
			role = entry.Role
		}
		if len(content) == 0 {
			content = entry.Content
		}
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(transcriptContentText(content))
		if text == "" {
			continue
		}
		if runes := []rune(text); len(runes) > maxItemChars {
			text = string(runes[:maxItemChars]) + " …[truncated]"
		}
		itemID := strings.TrimSpace(entry.ID)
		if itemID == "" {
			itemID = fmt.Sprintf("line-%06d", lineNumber)
		}
		items = append(items, &insightsv1.TranscriptItem{Id: itemID, Role: role, Content: text, Timestamp: entry.Timestamp})
	}
	if len(items) > maxItems {
		items = items[len(items)-maxItems:]
	}
	items = tailBudget(items, maxChars)
	return &insightsv1.ParsedTranscript{SessionId: sessionID, ProjectId: projectID, Items: items}
}

func tailBudget(items []*insightsv1.TranscriptItem, maxChars int) []*insightsv1.TranscriptItem {
	remaining := maxChars
	start := len(items)
	for i := len(items) - 1; i >= 0 && remaining > 0; i-- {
		length := len([]rune(items[i].Content))
		if length > remaining {
			runes := []rune(items[i].Content)
			const prefix = "…[truncated] "
			prefixRunes := []rune(prefix)
			if remaining > len(prefixRunes) {
				items[i].Content = prefix + string(runes[length-(remaining-len(prefixRunes)):])
			} else {
				items[i].Content = string(runes[length-remaining:])
			}
			start = i
			break
		}
		remaining -= length
		start = i
	}
	if start == len(items) {
		return nil
	}
	return items[start:]
}

func transcriptContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		// Thinking and tool records are deliberately excluded from the parsed
		// transcript; only user-visible text crosses the service boundary.
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}
