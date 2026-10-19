package sessionarchive

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Display item kinds.
const (
	DisplayUser       = "user"
	DisplayAssistant  = "assistant"
	DisplayReasoning  = "reasoning"
	DisplayToolCall   = "tool_call"
	DisplayToolResult = "tool_result"
	DisplaySystem     = "system"
	DisplayOther      = "other"
)

// DisplayItem is a protocol-independent rendering of one piece of a
// conversation item. One stored item can yield several (for example a Chat
// Completions assistant message with reasoning, text and tool calls).
type DisplayItem struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Index is the item's position in the conversation line (input items only).
	Index *int `json:"index,omitempty"`
	// Echo marks input that repeats the previous turn's output, which the
	// transcript already shows.
	Echo      bool `json:"echo,omitempty"`
	Encrypted bool `json:"encrypted,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

type rawToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// NormalizeItem renders a Chat Completions message, a Responses input/output
// item or an Anthropic message for display.
func NormalizeItem(raw json.RawMessage) []DisplayItem {
	var item struct {
		Type             string          `json:"type"`
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCalls        []rawToolCall   `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		Name             string          `json:"name"`
		CallID           string          `json:"call_id"`
		Arguments        json.RawMessage `json:"arguments"`
		Input            json.RawMessage `json:"input"`
		Output           json.RawMessage `json:"output"`
		Summary          []struct {
			Text string `json:"text"`
		} `json:"summary"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return limitAll([]DisplayItem{{Kind: DisplayOther, Text: string(raw)}})
	}

	switch item.Type {
	case "reasoning":
		var parts []string
		for _, s := range item.Summary {
			if text := strings.TrimSpace(s.Text); text != "" {
				parts = append(parts, text)
			}
		}
		if text := strings.TrimSpace(contentText(item.Content)); text != "" {
			parts = append(parts, text)
		}
		d := DisplayItem{Kind: DisplayReasoning, Text: strings.Join(parts, "\n\n")}
		d.Encrypted = d.Text == "" && item.EncryptedContent != ""
		return limitAll([]DisplayItem{d})
	case "function_call", "custom_tool_call", "mcp_call", "local_shell_call":
		args := rawText(item.Arguments)
		if args == "" {
			args = rawText(item.Input)
		}
		if args == "" {
			args = string(compactJSON(raw))
		}
		return limitAll([]DisplayItem{{Kind: DisplayToolCall, Name: item.Name, CallID: item.CallID, Arguments: args}})
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "mcp_call_output":
		return limitAll([]DisplayItem{{Kind: DisplayToolResult, CallID: item.CallID, Text: contentText(item.Output)}})
	case "", "message":
		if item.Role != "" {
			return limitAll(normalizeMessage(item.Role, item.Content, item.ReasoningContent, item.ToolCalls, item.ToolCallID))
		}
	}
	return limitAll([]DisplayItem{{Kind: DisplayOther, Name: item.Type, Text: string(compactJSON(raw))}})
}

func normalizeMessage(role string, content json.RawMessage, reasoningContent string, toolCalls []rawToolCall, toolCallID string) []DisplayItem {
	parts := parseParts(content)
	text := strings.Join(parts.texts, "\n")
	var out []DisplayItem
	switch role {
	case "tool", "function":
		return []DisplayItem{{Kind: DisplayToolResult, CallID: toolCallID, Text: text}}
	case "system", "developer":
		return []DisplayItem{{Kind: DisplaySystem, Text: text}}
	case "assistant", "model":
		if r := strings.TrimSpace(reasoningContent); r != "" {
			out = append(out, DisplayItem{Kind: DisplayReasoning, Text: r})
		}
		out = append(out, parts.reasoning...)
		if strings.TrimSpace(text) != "" {
			out = append(out, DisplayItem{Kind: DisplayAssistant, Text: text})
		}
		for _, tc := range toolCalls {
			out = append(out, DisplayItem{Kind: DisplayToolCall, Name: tc.Function.Name, CallID: tc.ID, Arguments: rawText(tc.Function.Arguments)})
		}
		out = append(out, parts.toolCalls...)
		out = append(out, parts.toolResults...)
	case "user":
		out = append(out, parts.toolResults...)
		if strings.TrimSpace(text) != "" {
			out = append(out, DisplayItem{Kind: DisplayUser, Text: text})
		}
		out = append(out, parts.toolCalls...)
	default:
		if strings.TrimSpace(text) != "" {
			out = append(out, DisplayItem{Kind: DisplayOther, Name: role, Text: text})
		}
	}
	return out
}

type contentParts struct {
	texts       []string
	reasoning   []DisplayItem
	toolCalls   []DisplayItem
	toolResults []DisplayItem
}

// parseParts splits message content (a string, a content-part array, or a
// single part object) into text and structured parts.
func parseParts(content json.RawMessage) contentParts {
	var parts contentParts
	trimmed := bytes.TrimSpace(content)
	if !present(trimmed) {
		return parts
	}
	var elements []json.RawMessage
	switch trimmed[0] {
	case '"':
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			parts.texts = append(parts.texts, text)
		}
		return parts
	case '[':
		if json.Unmarshal(trimmed, &elements) != nil {
			parts.texts = append(parts.texts, string(trimmed))
			return parts
		}
	case '{':
		elements = []json.RawMessage{trimmed}
	default:
		parts.texts = append(parts.texts, string(trimmed))
		return parts
	}

	for _, element := range elements {
		element = bytes.TrimSpace(element)
		if len(element) > 0 && element[0] == '"' {
			var text string
			if json.Unmarshal(element, &text) == nil {
				parts.texts = append(parts.texts, text)
			}
			continue
		}
		var part struct {
			Type      string          `json:"type"`
			Text      json.RawMessage `json:"text"`
			Thinking  string          `json:"thinking"`
			Refusal   string          `json:"refusal"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(element, &part) != nil {
			parts.texts = append(parts.texts, string(element))
			continue
		}
		switch part.Type {
		case "text", "input_text", "output_text", "summary_text", "reasoning_text":
			parts.texts = append(parts.texts, rawText(part.Text))
		case "refusal":
			parts.texts = append(parts.texts, part.Refusal)
		case "thinking":
			parts.reasoning = append(parts.reasoning, DisplayItem{Kind: DisplayReasoning, Text: part.Thinking})
		case "redacted_thinking":
			parts.reasoning = append(parts.reasoning, DisplayItem{Kind: DisplayReasoning, Encrypted: true})
		case "tool_use", "server_tool_use":
			parts.toolCalls = append(parts.toolCalls, DisplayItem{Kind: DisplayToolCall, Name: part.Name, CallID: part.ID, Arguments: rawText(part.Input)})
		case "tool_result":
			parts.toolResults = append(parts.toolResults, DisplayItem{Kind: DisplayToolResult, CallID: part.ToolUseID, Text: contentText(part.Content)})
		case "image", "image_url", "input_image":
			parts.texts = append(parts.texts, "[image]")
		case "file", "input_file", "document":
			parts.texts = append(parts.texts, "[file]")
		default:
			if present(part.Text) {
				parts.texts = append(parts.texts, rawText(part.Text))
			} else {
				parts.texts = append(parts.texts, string(compactJSON(element)))
			}
		}
	}
	return parts
}

// contentText flattens message content, tool output or instructions to text.
func contentText(content json.RawMessage) string {
	parts := parseParts(content)
	texts := parts.texts
	for _, result := range parts.toolResults {
		texts = append(texts, result.Text)
	}
	return strings.Join(texts, "\n")
}

// rawText returns a JSON string's value, or the compact JSON of anything else.
func rawText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if !present(trimmed) {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			return text
		}
	}
	return string(compactJSON(trimmed))
}

func limitAll(items []DisplayItem) []DisplayItem {
	for i := range items {
		var cut bool
		items[i].Text, cut = truncateRunes(items[i].Text, displayTextMaxRunes)
		items[i].Truncated = items[i].Truncated || cut
		items[i].Arguments, cut = truncateRunes(items[i].Arguments, displayTextMaxRunes)
		items[i].Truncated = items[i].Truncated || cut
	}
	return items
}

// truncateRunes cuts text to at most limit runes, reporting whether it cut.
func truncateRunes(text string, limit int) (string, bool) {
	if len(text) <= limit || utf8.RuneCountInString(text) <= limit {
		return text, false
	}
	count := 0
	for i := range text {
		if count == limit {
			return text[:i], true
		}
		count++
	}
	return text, false
}

// truncateBytes cuts text to at most limit bytes on a rune boundary.
func truncateBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
