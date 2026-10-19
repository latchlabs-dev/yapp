// Package usage provides token usage extraction and cost calculation.
package usage

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/metrics"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

// usagePipeline labels usage-recorder metrics in the shared recorder metric set.
const usagePipeline = "usage"

// GPTDiscountMultiplier is the effective rate paid for GPT/Codex usage after
// the production account's 90% discount.
const GPTDiscountMultiplier = 0.10

// Event represents a single usage event for a proxied request.
type Event struct {
	ID                string `json:"id"`
	RequestID         string `json:"request_id"`
	TenantID          string `json:"tenant_id"`
	UserID            string `json:"user_id"`
	KeyID             string `json:"key_id"`
	ProjectID         string `json:"project_id,omitempty"`
	ExtensionVersion  string `json:"extension_version,omitempty"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	UpstreamProvider  string `json:"upstream_provider"`
	UpstreamModel     string `json:"upstream_model"`
	ProviderAccountID string `json:"provider_account_id,omitempty"`
	Status            string `json:"status"`
	ErrorClass        string `json:"error_class,omitempty"`

	// ClientIP is the source IP the proxy observed (post trusted-proxy
	// resolution). Stored for abuse detection; empty when undetermined.
	ClientIP string `json:"client_ip,omitempty"`

	// Token usage
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	EstimatedUsage   bool  `json:"estimated_usage"`

	// Cost
	CostInputCents         float64 `json:"cost_input_cents"`
	CostOutputCents        float64 `json:"cost_output_cents"`
	CostCacheReadCents     float64 `json:"cost_cache_read_cents"`
	CostCacheWriteCents    float64 `json:"cost_cache_write_cents"`
	CostTotalCents         float64 `json:"cost_total_cents"`
	CostCalculated         bool    `json:"-"`
	SubscriptionRateCardID int64   `json:"subscription_rate_card_id,omitempty"`
	CostBasis              string  `json:"cost_basis,omitempty"`

	// Timing
	StartedAt        time.Time  `json:"started_at"`
	FirstByteAt      *time.Time `json:"first_byte_at,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	DurationMs       int64      `json:"duration_ms"`
	StreamDurationMs int64      `json:"stream_duration_ms,omitempty"`
}

// CostTable maps model pricing. Values are USD per million tokens.
type CostTable struct {
	InputPricePerMillion      float64 `json:"input"`
	OutputPricePerMillion     float64 `json:"output"`
	CacheReadPricePerMillion  float64 `json:"cache_read"`
	CacheWritePricePerMillion float64 `json:"cache_write"`
}

// TokenUsage captures billable token classes from upstream usage payloads.
type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
}

// DefaultCosts returns standard cost tables for common models.
var DefaultCosts = map[string]CostTable{
	"chat-latest":         {InputPricePerMillion: 5.00, OutputPricePerMillion: 30.00, CacheReadPricePerMillion: 0.50},
	"gpt-4.1":             {InputPricePerMillion: 2.00, OutputPricePerMillion: 8.00, CacheReadPricePerMillion: 0.50},
	"gpt-4.1-mini":        {InputPricePerMillion: 0.40, OutputPricePerMillion: 1.60, CacheReadPricePerMillion: 0.10},
	"gpt-4.1-nano":        {InputPricePerMillion: 0.10, OutputPricePerMillion: 0.40, CacheReadPricePerMillion: 0.025},
	"gpt-4o":              {InputPricePerMillion: 2.50, OutputPricePerMillion: 10.00, CacheReadPricePerMillion: 1.25},
	"gpt-4o-mini":         {InputPricePerMillion: 0.15, OutputPricePerMillion: 0.60, CacheReadPricePerMillion: 0.075},
	"gpt-5.1":             {InputPricePerMillion: 1.25, OutputPricePerMillion: 10.00, CacheReadPricePerMillion: 0.125},
	"gpt-5.1-chat-latest": {InputPricePerMillion: 1.25, OutputPricePerMillion: 10.00, CacheReadPricePerMillion: 0.125},
	"gpt-5.2":             {InputPricePerMillion: 1.75, OutputPricePerMillion: 14.00, CacheReadPricePerMillion: 0.175},
	"gpt-5.2-chat-latest": {InputPricePerMillion: 1.75, OutputPricePerMillion: 14.00, CacheReadPricePerMillion: 0.175},
	"gpt-5.2-pro":         {InputPricePerMillion: 21.00, OutputPricePerMillion: 168.00},
	"gpt-5.3-codex":       {InputPricePerMillion: 1.75, OutputPricePerMillion: 14.00, CacheReadPricePerMillion: 0.175},
	"gpt-5.4":             {InputPricePerMillion: 2.50, OutputPricePerMillion: 15.00, CacheReadPricePerMillion: 0.25},
	"gpt-5.4-mini":        {InputPricePerMillion: 0.75, OutputPricePerMillion: 4.50, CacheReadPricePerMillion: 0.075},
	"gpt-5.4-nano":        {InputPricePerMillion: 0.20, OutputPricePerMillion: 1.25, CacheReadPricePerMillion: 0.02},
	"gpt-5.4-pro":         {InputPricePerMillion: 30.00, OutputPricePerMillion: 180.00},
	"gpt-5.5":             {InputPricePerMillion: 5.00, OutputPricePerMillion: 30.00, CacheReadPricePerMillion: 0.50},
	"gpt-5.5-pro":         {InputPricePerMillion: 30.00, OutputPricePerMillion: 180.00},
	"gpt-5-pro":           {InputPricePerMillion: 15.00, OutputPricePerMillion: 120.00},
	"claude-sonnet-4":     {InputPricePerMillion: 3.00, OutputPricePerMillion: 15.00, CacheReadPricePerMillion: 0.30, CacheWritePricePerMillion: 3.75},
	"claude-haiku-3-5":    {InputPricePerMillion: 0.80, OutputPricePerMillion: 4.00, CacheReadPricePerMillion: 0.08, CacheWritePricePerMillion: 1.00},
	"claude-opus-4":       {InputPricePerMillion: 15.00, OutputPricePerMillion: 75.00, CacheReadPricePerMillion: 1.50, CacheWritePricePerMillion: 18.75},
	"deepseek-flash":      {InputPricePerMillion: 0.30, OutputPricePerMillion: 1.20, CacheReadPricePerMillion: 0.006},
	"deepseek-v4-flash":   {InputPricePerMillion: 0.14, OutputPricePerMillion: 0.28, CacheReadPricePerMillion: 0.0028, CacheWritePricePerMillion: 0.14},
	"deepseek-v4-pro":     {InputPricePerMillion: 0.435, OutputPricePerMillion: 0.87, CacheReadPricePerMillion: 0.003625, CacheWritePricePerMillion: 0.435},
	"deepseek-chat":       {InputPricePerMillion: 0.14, OutputPricePerMillion: 0.28, CacheReadPricePerMillion: 0.0028, CacheWritePricePerMillion: 0.14},
	"deepseek-reasoner":   {InputPricePerMillion: 0.14, OutputPricePerMillion: 0.28, CacheReadPricePerMillion: 0.0028, CacheWritePricePerMillion: 0.14},
	"glm-4.5-air":         {InputPricePerMillion: 0.20, OutputPricePerMillion: 1.10, CacheReadPricePerMillion: 0.03},
	"glm-4.7":             {InputPricePerMillion: 0.60, OutputPricePerMillion: 2.20, CacheReadPricePerMillion: 0.11},
	"glm-5-turbo":         {InputPricePerMillion: 1.20, OutputPricePerMillion: 4.00, CacheReadPricePerMillion: 0.24},
	"glm-5.1":             {InputPricePerMillion: 1.40, OutputPricePerMillion: 4.40, CacheReadPricePerMillion: 0.26},
	"glm-5.2":             {InputPricePerMillion: 1.40, OutputPricePerMillion: 4.40, CacheReadPricePerMillion: 0.26},
	"glm-5v-turbo":        {InputPricePerMillion: 1.20, OutputPricePerMillion: 4.00, CacheReadPricePerMillion: 0.24},
}

// KnownCostTable is a configurable cost table that can be updated at runtime.
type KnownCostTable struct {
	models map[string]CostTable
}

// NewCostTable creates a new cost table with default values.
func NewCostTable() *KnownCostTable {
	models := make(map[string]CostTable)
	for k, v := range DefaultCosts {
		if hasGPTDiscount(k) {
			v = applyCostMultiplier(v, GPTDiscountMultiplier)
		}
		models[k] = v
	}
	return &KnownCostTable{models: models}
}

// GetCost returns the cost table for a given model, or a default if unknown.
func (ct *KnownCostTable) GetCost(model string) CostTable {
	if cost, ok := ct.models[model]; ok {
		return cost
	}
	if base := stripSnapshotDate(model); base != model {
		if cost, ok := ct.models[base]; ok {
			return cost
		}
	}
	// Return a sensible default for unknown models.
	cost := CostTable{
		InputPricePerMillion:  3.00,
		OutputPricePerMillion: 15.00,
	}
	if hasGPTDiscount(model) {
		cost = applyCostMultiplier(cost, GPTDiscountMultiplier)
	}
	return cost
}

func hasGPTDiscount(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gpt-") || strings.EqualFold(model, "chat-latest")
}

func applyCostMultiplier(cost CostTable, multiplier float64) CostTable {
	cost.InputPricePerMillion *= multiplier
	cost.OutputPricePerMillion *= multiplier
	cost.CacheReadPricePerMillion *= multiplier
	cost.CacheWritePricePerMillion *= multiplier
	return cost
}

func stripSnapshotDate(model string) string {
	const suffixLen = len("-2006-01-02")
	if len(model) <= suffixLen {
		return model
	}
	suffix := model[len(model)-suffixLen:]
	if suffix[0] != '-' || !isDateDigits(suffix[1:]) {
		return model
	}
	return model[:len(model)-suffixLen]
}

func isDateDigits(s string) bool {
	return len(s) == len("2006-01-02") &&
		isDigit(s[0]) && isDigit(s[1]) && isDigit(s[2]) && isDigit(s[3]) &&
		s[4] == '-' &&
		isDigit(s[5]) && isDigit(s[6]) &&
		s[7] == '-' &&
		isDigit(s[8]) && isDigit(s[9])
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// SetCost updates the cost for a model.
func (ct *KnownCostTable) SetCost(model string, cost CostTable) {
	ct.models[model] = cost
}

// CalculateCost computes the cost in cents for given token counts.
func CalculateCost(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64, cost CostTable) float64 {
	inputCost := float64(inputTokens) / 1_000_000 * cost.InputPricePerMillion * 100
	outputCost := float64(outputTokens) / 1_000_000 * cost.OutputPricePerMillion * 100
	cacheReadCost := float64(cacheReadTokens) / 1_000_000 * cost.CacheReadPricePerMillion * 100
	cacheWriteCost := float64(cacheWriteTokens) / 1_000_000 * cost.CacheWritePricePerMillion * 100
	return inputCost + outputCost + cacheReadCost + cacheWriteCost
}

// EstimateCostCents computes expected spend for a model in cents. A nil cost
// table falls back to the built-in pricing table.
func EstimateCostCents(table *KnownCostTable, model string, inputTokens, outputTokens int64) float64 {
	if table == nil {
		table = NewCostTable()
	}
	return CalculateCost(inputTokens, outputTokens, 0, 0, table.GetCost(model))
}

// Recorder handles recording usage events.
type Recorder struct {
	events            chan *Event
	table             *KnownCostTable
	usageMetrics      *metrics.UsageMetrics
	recorderMetrics   *metrics.RecorderMetrics
	logger            zerolog.Logger
	subscriptionRates subscriptioncost.RateResolver

	// sendMu serializes Record's non-blocking send with Close's channel close.
	// stopped makes late producers drop events after shutdown begins.
	sendMu    sync.RWMutex
	stopped   atomic.Bool
	closeOnce sync.Once
}

// SetSubscriptionRates attaches the read-only daily rate resolver used by the
// data-plane recorder. It is configured during startup before requests begin.
func (r *Recorder) SetSubscriptionRates(resolver subscriptioncost.RateResolver) {
	if r != nil {
		r.subscriptionRates = resolver
	}
}

func (r *Recorder) SubscriptionRates() subscriptioncost.RateResolver {
	if r == nil {
		return nil
	}
	return r.subscriptionRates
}

// NewRecorder creates a new usage recorder. queueSize falls back to a default
// when non-positive. recorderMetrics may be nil (metric calls become no-ops).
func NewRecorder(table *KnownCostTable, usageMetrics *metrics.UsageMetrics, recorderMetrics *metrics.RecorderMetrics, queueSize int, logger zerolog.Logger) *Recorder {
	if queueSize <= 0 {
		queueSize = 4000
	}
	return &Recorder{
		events:          make(chan *Event, queueSize),
		table:           table,
		usageMetrics:    usageMetrics,
		recorderMetrics: recorderMetrics,
		logger:          logger,
	}
}

// QueueDepth reports the number of buffered, not-yet-persisted usage events.
func (r *Recorder) QueueDepth() int {
	if r == nil {
		return 0
	}
	return len(r.events)
}

// RecorderMetrics exposes the shared pipeline metric set so the consumer
// goroutine can record write successes/failures under the "usage" pipeline.
func (r *Recorder) RecorderMetrics() *metrics.RecorderMetrics {
	if r == nil {
		return nil
	}
	return r.recorderMetrics
}

// Record queues a usage event for async writing.
func (r *Recorder) Record(event *Event) {
	// Calculate cost if not already set.
	if !event.CostCalculated {
		cost := r.table.GetCost(event.UpstreamModel)
		event.CostTotalCents = CalculateCost(
			event.InputTokens,
			event.OutputTokens,
			event.CacheReadTokens,
			event.CacheWriteTokens,
			cost,
		)
		event.CostInputCents = float64(event.InputTokens) / 1_000_000 * cost.InputPricePerMillion * 100
		event.CostOutputCents = float64(event.OutputTokens) / 1_000_000 * cost.OutputPricePerMillion * 100
		event.CostCacheReadCents = float64(event.CacheReadTokens) / 1_000_000 * cost.CacheReadPricePerMillion * 100
		event.CostCacheWriteCents = float64(event.CacheWriteTokens) / 1_000_000 * cost.CacheWritePricePerMillion * 100
	}

	// Reject records once shutdown has begun so we never send on a closed
	// channel. Metrics for this event are skipped because it will not be
	// persisted.
	r.sendMu.RLock()
	defer r.sendMu.RUnlock()
	if r.stopped.Load() {
		return
	}

	// Emit Prometheus metrics.
	if r.usageMetrics != nil {
		r.usageMetrics.ObserveTokens(event.UpstreamModel, "input", int(event.InputTokens))
		r.usageMetrics.ObserveTokens(event.UpstreamModel, "output", int(event.OutputTokens))
		r.usageMetrics.ObserveCost(event.UpstreamModel, event.CostTotalCents/100.0) // cents → dollars
		r.usageMetrics.ObserveRequest(event.UpstreamModel, event.UpstreamProvider)
	}

	select {
	case r.events <- event:
		r.recorderMetrics.SetQueueDepth(usagePipeline, len(r.events))
	default:
		// Channel full: the usage event — our canonical billing/abuse record —
		// is being dropped. This must be observable, unlike the previous silent
		// drop. If this fires, raise PROXY_USAGE_QUEUE_SIZE / PROXY_USAGE_WORKERS
		// or check for a stalled Postgres writer.
		r.recorderMetrics.IncDropped(usagePipeline)
		r.recorderMetrics.SetQueueDepth(usagePipeline, len(r.events))
		r.logger.Error().
			Str("request_id", event.RequestID).
			Int("queue_size", cap(r.events)).
			Msg("usage event queue full; dropping billing/usage event")
	}
}

// Events returns the event channel for processing.
func (r *Recorder) Events() <-chan *Event {
	return r.events
}

// Close stops accepting new events and closes the events channel so consumers
// ranging over Events() drain the remaining buffered events and then exit. It
// is safe to call more than once. Close does not wait for consumers; the
// caller is expected to wait on the consumer goroutines (e.g. via a WaitGroup)
// after calling Close.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		r.stopped.Store(true)
		close(r.events)
	})
}

// ExtractOpenAIUsage parses usage from an OpenAI chat completion response.
func ExtractOpenAIUsage(body []byte) (input, output, total int64, err error) {
	usage, err := ExtractOpenAIUsageDetails(body)
	if err != nil {
		return 0, 0, 0, err
	}
	return usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens, usage.OutputTokens, usage.TotalTokens, nil
}

// ExtractOpenAIUsageDetails parses usage from OpenAI-compatible responses.
// DeepSeek reports prompt_cache_hit_tokens and prompt_cache_miss_tokens; cache
// hits are billed at the cache-read price and misses at the normal input price.
func ExtractOpenAIUsageDetails(body []byte) (TokenUsage, error) {
	var resp struct {
		Usage *struct {
			PromptTokens          int `json:"prompt_tokens"`
			CompletionTokens      int `json:"completion_tokens"`
			TotalTokens           int `json:"total_tokens"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			ReasoningTokens       int `json:"reasoning_tokens"`
			PromptTokensDetails   *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return TokenUsage{}, fmt.Errorf("parse openai usage: %w", err)
	}
	if resp.Usage == nil {
		return TokenUsage{}, nil
	}
	inputTokens := int64(resp.Usage.PromptTokens)
	cacheReadTokens := int64(resp.Usage.PromptCacheHitTokens)
	if resp.Usage.PromptCacheHitTokens > 0 || resp.Usage.PromptCacheMissTokens > 0 {
		inputTokens = int64(resp.Usage.PromptCacheMissTokens)
	} else if resp.Usage.PromptTokensDetails != nil && resp.Usage.PromptTokensDetails.CachedTokens > 0 {
		cacheReadTokens = int64(resp.Usage.PromptTokensDetails.CachedTokens)
		inputTokens -= cacheReadTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
	}
	totalTokens := int64(resp.Usage.TotalTokens)
	outputTokens := int64(resp.Usage.CompletionTokens)
	reasoningTokens := int64(resp.Usage.ReasoningTokens)
	if resp.Usage.CompletionTokensDetails != nil {
		reasoningTokens = int64(resp.Usage.CompletionTokensDetails.ReasoningTokens)
	}
	if totalTokens == 0 {
		if outputTokens < reasoningTokens {
			outputTokens = reasoningTokens
		}
		totalTokens = inputTokens + cacheReadTokens + outputTokens
	} else {
		knownNonOutputTokens := inputTokens + cacheReadTokens
		if impliedOutputTokens := totalTokens - knownNonOutputTokens; impliedOutputTokens > outputTokens {
			outputTokens = impliedOutputTokens
		}
	}
	return TokenUsage{
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		ReasoningTokens: reasoningTokens,
		CacheReadTokens: cacheReadTokens,
		TotalTokens:     totalTokens,
	}, nil
}

// ExtractAnthropicUsage parses usage from an Anthropic messages response.
func ExtractAnthropicUsage(body []byte) (input, output, total int64, err error) {
	var resp struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, 0, fmt.Errorf("parse anthropic usage: %w", err)
	}
	if resp.Usage == nil {
		return 0, 0, 0, nil
	}
	return int64(resp.Usage.InputTokens), int64(resp.Usage.OutputTokens), int64(resp.Usage.InputTokens + resp.Usage.OutputTokens), nil
}

// EstimateTextTokens provides a fast character-based token estimate.
func EstimateTextTokens(text string) int64 {
	return int64(len(text) / 4)
}

// ExtractStreamTokens attempts to extract token usage from a streaming end event.
// OpenAI streams end with `data: [DONE]` and may have a usage event in some API versions.
func ExtractStreamTokens(data string) (input, output int64, ok bool) {
	usage, ok := ExtractStreamTokenUsage(data)
	return usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens, usage.OutputTokens, ok
}

// ExtractStreamTokenUsage extracts OpenAI-compatible usage, including DeepSeek
// cache hit/miss fields, from a streaming usage event.
func ExtractStreamTokenUsage(data string) (TokenUsage, bool) {
	// Look for usage information in the stream data.
	var msg struct {
		Usage *struct {
			PromptTokens          int `json:"prompt_tokens"`
			CompletionTokens      int `json:"completion_tokens"`
			TotalTokens           int `json:"total_tokens"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			ReasoningTokens       int `json:"reasoning_tokens"`
			PromptTokensDetails   *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err == nil && msg.Usage != nil {
		inputTokens := int64(msg.Usage.PromptTokens)
		cacheReadTokens := int64(msg.Usage.PromptCacheHitTokens)
		if msg.Usage.PromptCacheHitTokens > 0 || msg.Usage.PromptCacheMissTokens > 0 {
			inputTokens = int64(msg.Usage.PromptCacheMissTokens)
		} else if msg.Usage.PromptTokensDetails != nil && msg.Usage.PromptTokensDetails.CachedTokens > 0 {
			cacheReadTokens = int64(msg.Usage.PromptTokensDetails.CachedTokens)
			inputTokens -= cacheReadTokens
			if inputTokens < 0 {
				inputTokens = 0
			}
		}
		totalTokens := int64(msg.Usage.TotalTokens)
		outputTokens := int64(msg.Usage.CompletionTokens)
		reasoningTokens := int64(msg.Usage.ReasoningTokens)
		if msg.Usage.CompletionTokensDetails != nil {
			reasoningTokens = int64(msg.Usage.CompletionTokensDetails.ReasoningTokens)
		}
		if totalTokens == 0 {
			if outputTokens < reasoningTokens {
				outputTokens = reasoningTokens
			}
			totalTokens = inputTokens + cacheReadTokens + outputTokens
		} else {
			knownNonOutputTokens := inputTokens + cacheReadTokens
			if impliedOutputTokens := totalTokens - knownNonOutputTokens; impliedOutputTokens > outputTokens {
				outputTokens = impliedOutputTokens
			}
		}
		return TokenUsage{
			InputTokens:     inputTokens,
			OutputTokens:    outputTokens,
			ReasoningTokens: reasoningTokens,
			CacheReadTokens: cacheReadTokens,
			TotalTokens:     totalTokens,
		}, true
	}
	return TokenUsage{}, false
}

// ExtractAnthropicStreamTokens attempts to extract token usage from
// Anthropic stream events. Anthropic splits usage across two event types:
//   - message_start:  {"type":"message_start","message":{"usage":{"input_tokens":10}}}
//   - message_delta:  {"type":"message_delta","usage":{"output_tokens":5}}
func ExtractAnthropicStreamTokens(eventType, data string) (input, output int64, ok bool) {
	switch eventType {
	case "message_start":
		var msg struct {
			Message struct {
				Usage *struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err == nil && msg.Message.Usage != nil {
			return int64(msg.Message.Usage.InputTokens), 0, true
		}
	case "message_delta":
		var delta struct {
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &delta); err == nil && delta.Usage != nil {
			return 0, int64(delta.Usage.OutputTokens), true
		}
	}
	return 0, 0, false
}
