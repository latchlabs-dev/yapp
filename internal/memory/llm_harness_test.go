package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/earlgrey/piproxy/internal/llmclient"
)

type mockLLMResponse struct {
	Text string
	Err  error
}

type mockLLM struct {
	responses []mockLLMResponse
	requests  []llmclient.Request
}

func newMockLLM(responses ...mockLLMResponse) *mockLLM {
	return &mockLLM{responses: responses}
}

func newMockLLMText(text string) *mockLLM {
	return newMockLLM(mockLLMResponse{Text: text})
}

func newMockLLMError(err error) *mockLLM {
	return newMockLLM(mockLLMResponse{Err: err})
}

func (m *mockLLM) Complete(ctx context.Context, req llmclient.Request) (string, error) {
	m.requests = append(m.requests, req)
	if len(m.responses) == 0 {
		return "", fmt.Errorf("mock LLM response queue exhausted")
	}
	next := m.responses[0]
	m.responses = m.responses[1:]
	return next.Text, next.Err
}

func (m *mockLLM) request(t *testing.T, index int) llmclient.Request {
	t.Helper()
	if len(m.requests) <= index {
		t.Fatalf("mock LLM saw %d requests, want request index %d", len(m.requests), index)
	}
	return m.requests[index]
}
