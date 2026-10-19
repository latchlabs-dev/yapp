package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/earlgrey/piproxy/internal/hooks"
)

// hookContentBudget caps the text handed to a content-aware hook. Beyond this
// the oldest turns are dropped and Truncated is set, so a hook can tell it is
// judging a partial conversation rather than assuming it saw everything.
const hookContentBudget = 128 << 10

// errRewriteShapeMismatch is returned when a hook's rewritten message list does
// not line up with the request it was given. We refuse such a rewrite rather
// than applying it: a mismatched list would drop turns, and — because rewrites
// carry text only — would silently discard every attached image and document.
var errRewriteShapeMismatch = errors.New("hook rewrite does not match the request's message shape")

// extractHookContent pulls the normalized conversation out of a provider
// request body. Both dialects reduce to the same {role, text} list, so one hook
// implementation covers OpenAI, DeepSeek and Anthropic alike.
func extractHookContent(body []byte) *hooks.EventContent {
	if content := extractAnthropicContent(body); content != nil {
		return content
	}
	return extractOpenAIContent(body)
}

func extractOpenAIContent(body []byte) *hooks.EventContent {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return nil
	}
	content := &hooks.EventContent{}
	for _, m := range req.Messages {
		text := flattenContent(m.Content)
		if m.Role == "system" || m.Role == "developer" {
			content.System = joinNonEmpty(content.System, text)
			continue
		}
		content.Messages = append(content.Messages, hooks.ContentMessage{Role: m.Role, Text: text})
	}
	return capHookContent(content)
}

func extractAnthropicContent(body []byte) *hooks.EventContent {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	// max_tokens is required by the Anthropic messages API and absent from
	// chat-completions bodies, which is the cheapest reliable discriminator.
	if req.MaxTokens == nil && len(req.System) == 0 {
		return nil
	}
	content := &hooks.EventContent{System: flattenContent(req.System)}
	for _, m := range req.Messages {
		content.Messages = append(content.Messages, hooks.ContentMessage{
			Role: m.Role,
			Text: flattenContent(m.Content),
		})
	}
	return capHookContent(content)
}

// flattenContent renders a message's content — a plain string, or an array of
// typed parts — down to its text. Non-text parts contribute a short marker, so
// a hook can tell an image was present without receiving its bytes.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text", "text_delta":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		case "image", "image_url":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[image]")
		case "document":
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[document]")
		}
	}
	return b.String()
}

func capHookContent(content *hooks.EventContent) *hooks.EventContent {
	budget := hookContentBudget - len(content.System)
	if budget < 0 {
		budget = 0
	}
	kept := make([]hooks.ContentMessage, 0, len(content.Messages))
	for i := len(content.Messages) - 1; i >= 0; i-- {
		if len(content.Messages[i].Text) > budget {
			content.Truncated = true
			break
		}
		budget -= len(content.Messages[i].Text)
		kept = append(kept, content.Messages[i])
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	if len(kept) != len(content.Messages) {
		content.Truncated = true
	}
	content.Messages = kept
	return content
}

// applyHookRewrite folds a hook's mutations back into the provider request.
//
// The rewrite carries text only, so it must not be allowed to replace a
// message that had an image or document attached to it — that would strip the
// attachment while appearing to succeed. The rewritten list is therefore
// required to match the original message-for-message by role, and only the
// text parts of each message are substituted; every non-text part is preserved
// exactly as sent.
func applyHookRewrite(body []byte, decision *hooks.Decision) ([]byte, error) {
	if decision == nil || !decision.Mutates() {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("parse request for hook rewrite: %w", err)
	}

	if decision.SystemPrepend != "" {
		if err := prependSystem(envelope, decision.SystemPrepend); err != nil {
			return nil, err
		}
	}
	if len(decision.RewrittenMessages) > 0 {
		if err := rewriteMessages(envelope, decision.RewrittenMessages); err != nil {
			return nil, err
		}
	}
	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("re-encode request after hook rewrite: %w", err)
	}
	return rewritten, nil
}

// prependSystem puts the hook's text in front of the existing system prompt.
// Anthropic carries `system` at the top level; chat-completions bodies carry it
// as the first message, so the two need different surgery.
func prependSystem(envelope map[string]json.RawMessage, prepend string) error {
	if raw, ok := envelope["system"]; ok {
		combined, err := json.Marshal(joinNonEmpty(prepend, flattenContent(raw)))
		if err != nil {
			return fmt.Errorf("encode rewritten system prompt: %w", err)
		}
		envelope["system"] = combined
		return nil
	}

	var messages []json.RawMessage
	if raw, ok := envelope["messages"]; ok {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return fmt.Errorf("parse messages for hook rewrite: %w", err)
		}
	}
	// An Anthropic body with no system field yet: add one rather than inserting
	// a system message, which that API rejects.
	if _, isAnthropic := envelope["max_tokens"]; isAnthropic {
		combined, err := json.Marshal(prepend)
		if err != nil {
			return fmt.Errorf("encode rewritten system prompt: %w", err)
		}
		envelope["system"] = combined
		return nil
	}

	injected, err := json.Marshal(map[string]string{"role": "system", "content": prepend})
	if err != nil {
		return fmt.Errorf("encode injected system message: %w", err)
	}
	encoded, err := json.Marshal(append([]json.RawMessage{injected}, messages...))
	if err != nil {
		return fmt.Errorf("re-encode messages after hook rewrite: %w", err)
	}
	envelope["messages"] = encoded
	return nil
}

func rewriteMessages(envelope map[string]json.RawMessage, rewritten []hooks.ContentMessage) error {
	raw, ok := envelope["messages"]
	if !ok {
		return errRewriteShapeMismatch
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return fmt.Errorf("parse messages for hook rewrite: %w", err)
	}

	// The hook saw the conversation without system turns, so line the rewrite
	// up against the same subset.
	indices := make([]int, 0, len(messages))
	for i, m := range messages {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role == "system" || role == "developer" {
			continue
		}
		indices = append(indices, i)
	}
	if len(indices) != len(rewritten) {
		return errRewriteShapeMismatch
	}

	for n, idx := range indices {
		var role string
		_ = json.Unmarshal(messages[idx]["role"], &role)
		if role != rewritten[n].Role {
			return errRewriteShapeMismatch
		}
		replaced, err := replaceTextParts(messages[idx]["content"], rewritten[n].Text)
		if err != nil {
			return err
		}
		messages[idx]["content"] = replaced
	}

	encoded, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("re-encode messages after hook rewrite: %w", err)
	}
	envelope["messages"] = encoded
	return nil
}

// replaceTextParts substitutes a message's text while leaving images and
// documents untouched. A message that was a plain string stays a plain string;
// a multimodal message keeps its non-text parts in their original order.
func replaceTextParts(raw json.RawMessage, text string) (json.RawMessage, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, fmt.Errorf("encode rewritten message: %w", err)
		}
		return encoded, nil
	}

	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Content in a shape we do not model: refuse rather than clobber it.
		return nil, errRewriteShapeMismatch
	}
	encodedText, err := json.Marshal(text)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten message: %w", err)
	}

	out := make([]map[string]json.RawMessage, 0, len(parts)+1)
	replaced := false
	for _, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		if partType != "text" {
			out = append(out, part)
			continue
		}
		if replaced {
			// The rewrite is one string, so the first text part absorbs it all
			// and any further text parts are dropped.
			continue
		}
		part["text"] = encodedText
		out = append(out, part)
		replaced = true
	}
	if !replaced {
		out = append([]map[string]json.RawMessage{{
			"type": json.RawMessage(`"text"`),
			"text": encodedText,
		}}, out...)
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("re-encode message parts after hook rewrite: %w", err)
	}
	return encoded, nil
}

func joinNonEmpty(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n\n" + second
	}
}
