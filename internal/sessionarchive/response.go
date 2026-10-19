package sessionarchive

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/earlgrey/piproxy/internal/capture"
)

// ParseResponseOutput extracts the model's output from a captured response
// body as items shaped like request items, so outputs and the next request's
// echo of them render through the same normalizer. It returns nil when the
// body holds no recognizable output (errors, truncated or empty streams).
func ParseResponseOutput(api API, responseFormat string, body []byte) []json.RawMessage {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if api == "" || api == APIUnknown {
		api = sniffResponseAPI(trimmed)
	}
	if isSSE(responseFormat, trimmed) {
		switch api {
		case APIResponses:
			return responsesSSEOutput(trimmed)
		case APIAnthropic:
			return anthropicSSEOutput(trimmed)
		default:
			return chatSSEOutput(trimmed)
		}
	}
	switch api {
	case APIResponses:
		return responsesJSONOutput(trimmed)
	case APIAnthropic:
		return anthropicJSONOutput(trimmed)
	default:
		return chatJSONOutput(trimmed)
	}
}

func isSSE(responseFormat string, body []byte) bool {
	return responseFormat == capture.ResponseFormatSSE ||
		bytes.HasPrefix(body, []byte("data:")) ||
		bytes.HasPrefix(body, []byte("event:")) ||
		bytes.Contains(body, []byte("\ndata:"))
}

func sniffResponseAPI(body []byte) API {
	switch {
	case bytes.Contains(body, []byte(`"type":"response.`)) || bytes.Contains(body, []byte(`"object":"response"`)):
		return APIResponses
	case bytes.Contains(body, []byte(`"content_block_`)) || bytes.Contains(body, []byte(`"message_start"`)):
		return APIAnthropic
	default:
		return APIChat
	}
}

// sseData calls fn with the payload of every data: line, skipping [DONE].
func sseData(body []byte, fn func(data []byte)) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		fn(data)
	}
}

func responsesSSEOutput(body []byte) []json.RawMessage {
	var done, completed []json.RawMessage
	var text strings.Builder
	sseData(body, func(data []byte) {
		var event struct {
			Type     string          `json:"type"`
			Item     json.RawMessage `json:"item"`
			Delta    json.RawMessage `json:"delta"`
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		switch event.Type {
		case "response.output_item.done":
			if present(event.Item) {
				done = append(done, compactJSON(event.Item))
			}
		case "response.completed", "response.done", "response.incomplete", "response.failed":
			if len(event.Response.Output) > 0 {
				completed = compactItems(event.Response.Output)
			}
		case "response.output_text.delta":
			var delta string
			if json.Unmarshal(event.Delta, &delta) == nil {
				text.WriteString(delta)
			}
		}
	})
	switch {
	case len(done) > 0:
		return done
	case len(completed) > 0:
		return completed
	case text.Len() > 0:
		// A stream cut off before any item completed still shows its text.
		item, _ := json.Marshal(map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]string{{"type": "output_text", "text": text.String()}},
		})
		return []json.RawMessage{item}
	default:
		return nil
	}
}

func responsesJSONOutput(body []byte) []json.RawMessage {
	var resp struct {
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(body, &resp) != nil || len(resp.Output) == 0 {
		return nil
	}
	return compactItems(resp.Output)
}

type chatToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatAssistantMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

func chatSSEOutput(body []byte) []json.RawMessage {
	var content, reasoning strings.Builder
	calls := make(map[int]*chatToolCall)
	sseData(body, func(data []byte) {
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			reasoning.WriteString(choice.Delta.Reasoning)
			for _, tc := range choice.Delta.ToolCalls {
				call, ok := calls[tc.Index]
				if !ok {
					call = &chatToolCall{Type: "function"}
					calls[tc.Index] = call
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Type != "" {
					call.Type = tc.Type
				}
				if tc.Function.Name != "" && call.Function.Name == "" {
					call.Function.Name = tc.Function.Name
				}
				call.Function.Arguments += tc.Function.Arguments
			}
		}
	})

	msg := chatAssistantMessage{Role: "assistant", Content: content.String(), ReasoningContent: reasoning.String()}
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		msg.ToolCalls = append(msg.ToolCalls, *calls[index])
	}
	if msg.Content == "" && msg.ReasoningContent == "" && len(msg.ToolCalls) == 0 {
		return nil
	}
	item, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return []json.RawMessage{item}
}

func chatJSONOutput(body []byte) []json.RawMessage {
	var resp struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &resp) != nil || len(resp.Choices) == 0 || !present(resp.Choices[0].Message) {
		return nil
	}
	return []json.RawMessage{compactJSON(resp.Choices[0].Message)}
}

func anthropicJSONOutput(body []byte) []json.RawMessage {
	var resp struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &resp) != nil || !present(resp.Content) {
		return nil
	}
	item, err := json.Marshal(map[string]json.RawMessage{
		"role":    json.RawMessage(`"assistant"`),
		"content": compactJSON(resp.Content),
	})
	if err != nil {
		return nil
	}
	return []json.RawMessage{item}
}

type anthropicBlock struct {
	kind     string
	text     strings.Builder
	id       string
	name     string
	input    strings.Builder
	startRaw json.RawMessage
}

func anthropicSSEOutput(body []byte) []json.RawMessage {
	blocks := make(map[int]*anthropicBlock)
	sseData(body, func(data []byte) {
		var event struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type     string          `json:"type"`
				Text     string          `json:"text"`
				Thinking string          `json:"thinking"`
				ID       string          `json:"id"`
				Name     string          `json:"name"`
				Input    json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		switch event.Type {
		case "content_block_start":
			block := &anthropicBlock{
				kind:     event.ContentBlock.Type,
				id:       event.ContentBlock.ID,
				name:     event.ContentBlock.Name,
				startRaw: event.ContentBlock.Input,
			}
			block.text.WriteString(event.ContentBlock.Text)
			block.text.WriteString(event.ContentBlock.Thinking)
			blocks[event.Index] = block
		case "content_block_delta":
			block, ok := blocks[event.Index]
			if !ok {
				return
			}
			block.text.WriteString(event.Delta.Text)
			block.text.WriteString(event.Delta.Thinking)
			block.input.WriteString(event.Delta.PartialJSON)
		}
	})
	if len(blocks) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	content := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		block := blocks[index]
		switch block.kind {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": block.text.String()})
		case "thinking":
			content = append(content, map[string]any{"type": "thinking", "thinking": block.text.String()})
		case "tool_use", "server_tool_use":
			input := json.RawMessage(block.input.String())
			if !json.Valid(input) {
				input = block.startRaw
			}
			if !present(input) {
				input = json.RawMessage(`{}`)
			}
			content = append(content, map[string]any{"type": block.kind, "id": block.id, "name": block.name, "input": input})
		default:
			content = append(content, map[string]any{"type": block.kind})
		}
	}
	item, err := json.Marshal(map[string]any{"role": "assistant", "content": content})
	if err != nil {
		return nil
	}
	return []json.RawMessage{item}
}
