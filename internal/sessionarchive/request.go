package sessionarchive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParsedRequest is a captured request split into its system text and the
// conversation items that make up the transcript line.
type ParsedRequest struct {
	API    API
	System string
	Items  []json.RawMessage
	// Append marks a Responses request chained with previous_response_id:
	// Items are only the new items, not the whole conversation.
	Append bool
}

var errUnrecognizedRequest = errors.New("unrecognized request body")

// ParseRequest recognizes Chat Completions, Responses and Anthropic Messages
// request bodies. Leading system/developer items are split off into System so
// a changed system prompt does not look like a rewritten conversation.
func ParseRequest(provider string, body []byte) (*ParsedRequest, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("%w: %v", errUnrecognizedRequest, err)
	}
	switch {
	case present(top["input"]):
		return parseResponsesRequest(top)
	case present(top["messages"]):
		if present(top["system"]) || strings.Contains(strings.ToLower(provider), "anthropic") {
			return parseMessagesRequest(APIAnthropic, top)
		}
		return parseMessagesRequest(APIChat, top)
	default:
		return nil, errUnrecognizedRequest
	}
}

func parseResponsesRequest(top map[string]json.RawMessage) (*ParsedRequest, error) {
	req := &ParsedRequest{API: APIResponses}
	var systems []string
	if text := strings.TrimSpace(contentText(top["instructions"])); text != "" {
		systems = append(systems, text)
	}
	if present(top["previous_response_id"]) {
		var previous string
		if json.Unmarshal(top["previous_response_id"], &previous) == nil {
			req.Append = strings.TrimSpace(previous) != ""
		}
	}

	input := bytes.TrimSpace(top["input"])
	var items []json.RawMessage
	if input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return nil, fmt.Errorf("%w: input: %v", errUnrecognizedRequest, err)
		}
		item, err := json.Marshal(map[string]string{"role": "user", "content": text})
		if err != nil {
			return nil, err
		}
		items = []json.RawMessage{item}
	} else if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("%w: input: %v", errUnrecognizedRequest, err)
	}

	items, leading := splitLeadingSystem(items)
	req.System = strings.Join(append(systems, leading...), "\n\n")
	req.Items = compactItems(items)
	return req, nil
}

func parseMessagesRequest(api API, top map[string]json.RawMessage) (*ParsedRequest, error) {
	req := &ParsedRequest{API: api}
	var items []json.RawMessage
	if err := json.Unmarshal(top["messages"], &items); err != nil {
		return nil, fmt.Errorf("%w: messages: %v", errUnrecognizedRequest, err)
	}
	var systems []string
	if text := strings.TrimSpace(contentText(top["system"])); text != "" {
		systems = append(systems, text)
	}
	items, leading := splitLeadingSystem(items)
	req.System = strings.Join(append(systems, leading...), "\n\n")
	req.Items = compactItems(items)
	return req, nil
}

// splitLeadingSystem removes leading system/developer items and returns their
// text. Later system items stay in the line: they are part of the conversation.
func splitLeadingSystem(items []json.RawMessage) ([]json.RawMessage, []string) {
	var texts []string
	for len(items) > 0 {
		var head struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(items[0], &head) != nil || (head.Role != "system" && head.Role != "developer") {
			break
		}
		if text := strings.TrimSpace(contentText(head.Content)); text != "" {
			texts = append(texts, text)
		}
		items = items[1:]
	}
	return items, texts
}

// compactItems normalizes insignificant whitespace so that the same item
// hashes identically however the client happened to serialize it.
func compactItems(items []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		out[i] = compactJSON(item)
	}
	return out
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return buf.Bytes()
}

func present(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}
