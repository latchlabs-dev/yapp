// Package llmclient provides a minimal internal LLM completion client for
// proxy-side jobs (memory distillation, compaction). It reuses the provider
// registry, credential store, and upstream HTTP client rather than going
// through the public data-plane handlers, so internal calls are not subject
// to tenant auth or quota.
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/upstream"
)

// Request describes a single non-streaming completion call.
type Request struct {
	Model       string
	System      string
	User        string
	MaxTokens   int
	Temperature float64
}

// Client performs internal completions against configured upstream providers.
type Client struct {
	registry *providers.Registry
	creds    *secrets.CredentialStore
	upstream *upstream.Client
}

// New creates an internal LLM client.
func New(registry *providers.Registry, creds *secrets.CredentialStore, up *upstream.Client) *Client {
	return &Client{registry: registry, creds: creds, upstream: up}
}

// Complete runs a non-streaming completion and returns the assistant text.
func (c *Client) Complete(ctx context.Context, req Request) (string, error) {
	if req.Model == "" {
		return "", fmt.Errorf("llmclient: model is required")
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 2048
	}

	route, ok := c.registry.ResolveModelRoute("", "", "", req.Model)
	if !ok {
		return "", fmt.Errorf("llmclient: no provider for model %q", req.Model)
	}
	provider := route.Provider
	req.Model = route.UpstreamModel

	body, path, err := buildRequestBody(provider.Type, req)
	if err != nil {
		return "", err
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if provider.CredentialID != "" {
		if c.creds == nil {
			return "", fmt.Errorf("llmclient: credential store not configured")
		}
		cred, err := c.creds.GetCredential(ctx, provider.CredentialID)
		if err != nil {
			return "", fmt.Errorf("llmclient: credential %s: %w", provider.CredentialID, err)
		}
		secret := cred.SecretValue()
		if secret == "" {
			return "", fmt.Errorf("llmclient: credential %s has no usable secret", provider.CredentialID)
		}
		rewrite.InjectCredentialHeaders(headers, provider.Type, secret)
	}

	url := strings.TrimRight(provider.UpstreamBaseURL, "/") + path
	resp, err := c.upstream.DoForward(ctx, http.MethodPost, url, headers, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llmclient: upstream call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return "", fmt.Errorf("llmclient: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llmclient: upstream HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 512))
	}

	return extractText(provider.Type, respBody)
}

// buildRequestBody constructs the provider-family-specific JSON body and API path.
func buildRequestBody(pt providers.ProviderType, req Request) ([]byte, string, error) {
	switch pt {
	case providers.ProviderAnthropic:
		payload := map[string]any{
			"model":      req.Model,
			"max_tokens": req.MaxTokens,
			"messages": []map[string]any{
				{"role": "user", "content": req.User},
			},
		}
		if req.System != "" {
			payload["system"] = req.System
		}
		if req.Temperature > 0 {
			payload["temperature"] = req.Temperature
		}
		body, err := json.Marshal(payload)
		return body, "/v1/messages", err
	default:
		// OpenAI-compatible chat completions (OpenAI, DeepSeek, ...).
		messages := []map[string]any{}
		if req.System != "" {
			messages = append(messages, map[string]any{"role": "system", "content": req.System})
		}
		messages = append(messages, map[string]any{"role": "user", "content": req.User})
		payload := map[string]any{
			"model":      req.Model,
			"max_tokens": req.MaxTokens,
			"messages":   messages,
		}
		if req.Temperature > 0 {
			payload["temperature"] = req.Temperature
		}
		body, err := json.Marshal(payload)
		return body, "/v1/chat/completions", err
	}
}

// extractText pulls the assistant text out of a provider response.
func extractText(pt providers.ProviderType, body []byte) (string, error) {
	switch pt {
	case providers.ProviderAnthropic:
		var resp struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return "", fmt.Errorf("llmclient: parse anthropic response: %w", err)
		}
		var sb strings.Builder
		for _, block := range resp.Content {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
		if sb.Len() == 0 {
			return "", fmt.Errorf("llmclient: anthropic response contained no text")
		}
		return sb.String(), nil
	default:
		var resp struct {
			Choices []struct {
				Message struct {
					Content          any    `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return "", fmt.Errorf("llmclient: parse openai response: %w", err)
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("llmclient: openai response contained no text")
		}
		text := contentToText(resp.Choices[0].Message.Content)
		if text == "" {
			text = strings.TrimSpace(resp.Choices[0].Message.ReasoningContent)
		}
		if text == "" {
			return "", fmt.Errorf("llmclient: openai response contained no text")
		}
		return text, nil
	}
}

func contentToText(v any) string {
	switch c := v.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var sb strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				sb.WriteString(text)
			}
		}
		return strings.TrimSpace(sb.String())
	default:
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
