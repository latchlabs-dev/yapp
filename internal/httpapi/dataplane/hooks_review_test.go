package dataplane

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/usage"
)

func TestPostSendDelegatesAsyncPublicationOnlyForOutputReview(t *testing.T) {
	manager := hooks.NewManager(hooks.Config{}, zerolog.Nop())
	var calls atomic.Int64
	manager.Append(hooks.HandlerFunc{HandlerName: "post", EventTypes: []hooks.EventType{hooks.EventUserMessagePostSend},
		Fn: func(context.Context, *hooks.Event) (*hooks.Decision, error) { calls.Add(1); return nil, nil }})
	services := &Services{Hooks: manager}
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	request := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(string(body)))
	reviewed := request.WithContext(hooks.WithOutputReview(request.Context()))
	started := time.Now()
	EmitUserPostSendHook(reviewed, services, &providers.Provider{Type: providers.ProviderOpenAI}, "model", "success", "", 200, true, body, usage.TokenUsage{}, false, started, time.Now())
	if event := hooks.OutputReviewEvent(reviewed.Context()); event == nil || event.Stats.StartedAt == nil || event.Stats.CompletedAt == nil || event.Provider != string(providers.ProviderOpenAI) {
		t.Fatalf("review lost delegated metadata: %+v", event)
	}
	// The ordinary CLI request keeps its existing async post-send path.
	EmitUserPostSendHook(request, services, &providers.Provider{Type: providers.ProviderOpenAI}, "model", "success", "", 200, true, body, usage.TokenUsage{}, false, started, time.Now())
	manager.Close()
	if calls.Load() != 1 {
		t.Fatalf("post handler called %d times, want only the ordinary CLI request", calls.Load())
	}
}
