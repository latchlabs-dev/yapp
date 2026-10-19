package usage_test

import (
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/usage"
)

func TestExtractOpenAIUsage(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	input, output, total, err := usage.ExtractOpenAIUsage(body)
	if err != nil {
		t.Fatalf("ExtractOpenAIUsage failed: %v", err)
	}
	if input != 10 {
		t.Errorf("expected 10 input tokens, got %d", input)
	}
	if output != 20 {
		t.Errorf("expected 20 output tokens, got %d", output)
	}
	if total != 30 {
		t.Errorf("expected 30 total tokens, got %d", total)
	}
}

func TestRecorderCloseConcurrentRecord(t *testing.T) {
	recorder := usage.NewRecorder(usage.NewCostTable(), nil, nil, 8, zerolog.Nop())

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 2000; j++ {
				recorder.Record(&usage.Event{
					RequestID:        "req_close_race",
					UpstreamProvider: "openai",
					UpstreamModel:    "gpt-test",
				})
			}
		}()
	}

	close(start)
	recorder.Close()
	wg.Wait()
}

func TestExtractOpenAIUsageNoUsage(t *testing.T) {
	body := []byte(`{"id":"123","object":"chat.completion"}`)
	input, output, total, err := usage.ExtractOpenAIUsage(body)
	if err != nil {
		t.Fatalf("ExtractOpenAIUsage failed: %v", err)
	}
	if input != 0 || output != 0 || total != 0 {
		t.Errorf("expected all 0, got %d/%d/%d", input, output, total)
	}
}

func TestExtractOpenAIUsageDetailsDeepSeekCache(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":3000,"prompt_cache_hit_tokens":2000,"prompt_cache_miss_tokens":1000,"completion_tokens":500,"total_tokens":3500}}`)
	got, err := usage.ExtractOpenAIUsageDetails(body)
	if err != nil {
		t.Fatalf("ExtractOpenAIUsageDetails failed: %v", err)
	}
	if got.InputTokens != 1000 {
		t.Errorf("expected cache miss input tokens 1000, got %d", got.InputTokens)
	}
	if got.CacheReadTokens != 2000 {
		t.Errorf("expected cache hit tokens 2000, got %d", got.CacheReadTokens)
	}
	if got.OutputTokens != 500 {
		t.Errorf("expected output tokens 500, got %d", got.OutputTokens)
	}
	if got.TotalTokens != 3500 {
		t.Errorf("expected total tokens 3500, got %d", got.TotalTokens)
	}
}

func TestExtractOpenAIUsageDetailsPromptTokensDetailsCache(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":3000,"prompt_tokens_details":{"cached_tokens":2000},"completion_tokens":500,"total_tokens":3500}}`)
	got, err := usage.ExtractOpenAIUsageDetails(body)
	if err != nil {
		t.Fatalf("ExtractOpenAIUsageDetails failed: %v", err)
	}
	if got.InputTokens != 1000 {
		t.Errorf("expected uncached input tokens 1000, got %d", got.InputTokens)
	}
	if got.CacheReadTokens != 2000 {
		t.Errorf("expected cached input tokens 2000, got %d", got.CacheReadTokens)
	}
	if got.OutputTokens != 500 {
		t.Errorf("expected output tokens 500, got %d", got.OutputTokens)
	}
	if got.TotalTokens != 3500 {
		t.Errorf("expected total tokens 3500, got %d", got.TotalTokens)
	}
}

func TestExtractOpenAIUsageDetailsReasoningTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":80,"total_tokens":180,"completion_tokens_details":{"reasoning_tokens":45}}}`)
	got, err := usage.ExtractOpenAIUsageDetails(body)
	if err != nil {
		t.Fatalf("ExtractOpenAIUsageDetails failed: %v", err)
	}
	if got.OutputTokens != 80 {
		t.Errorf("expected completion output tokens 80, got %d", got.OutputTokens)
	}
	if got.ReasoningTokens != 45 {
		t.Errorf("expected reasoning tokens 45, got %d", got.ReasoningTokens)
	}
	if got.TotalTokens != 180 {
		t.Errorf("expected total tokens 180, got %d", got.TotalTokens)
	}
}

func TestExtractOpenAIUsageDetailsSeparatelyReportedReasoning(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":80,"reasoning_tokens":45,"total_tokens":225}}`)
	got, err := usage.ExtractOpenAIUsageDetails(body)
	if err != nil {
		t.Fatalf("ExtractOpenAIUsageDetails failed: %v", err)
	}
	if got.OutputTokens != 125 {
		t.Errorf("expected implied billable output tokens 125, got %d", got.OutputTokens)
	}
	if got.ReasoningTokens != 45 {
		t.Errorf("expected reasoning tokens 45, got %d", got.ReasoningTokens)
	}
	if got.TotalTokens != 225 {
		t.Errorf("expected total tokens 225, got %d", got.TotalTokens)
	}
}

func TestExtractAnthropicUsage(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":15,"output_tokens":25}}`)
	input, output, total, err := usage.ExtractAnthropicUsage(body)
	if err != nil {
		t.Fatalf("ExtractAnthropicUsage failed: %v", err)
	}
	if input != 15 {
		t.Errorf("expected 15 input tokens, got %d", input)
	}
	if output != 25 {
		t.Errorf("expected 25 output tokens, got %d", output)
	}
	if total != 40 {
		t.Errorf("expected 40 total tokens, got %d", total)
	}
}

func TestExtractStreamTokens(t *testing.T) {
	input, output, ok := usage.ExtractStreamTokens(`{"usage":{"prompt_tokens":12,"completion_tokens":34}}`)
	if !ok {
		t.Fatal("expected OpenAI stream usage")
	}
	if input != 12 || output != 34 {
		t.Fatalf("expected 12/34 tokens, got %d/%d", input, output)
	}
}

func TestExtractStreamTokenUsageDeepSeekCache(t *testing.T) {
	got, ok := usage.ExtractStreamTokenUsage(`{"usage":{"prompt_tokens":30,"prompt_cache_hit_tokens":20,"prompt_cache_miss_tokens":10,"completion_tokens":5,"total_tokens":35}}`)
	if !ok {
		t.Fatal("expected OpenAI-compatible stream usage")
	}
	if got.InputTokens != 10 || got.CacheReadTokens != 20 || got.OutputTokens != 5 || got.TotalTokens != 35 {
		t.Fatalf("unexpected usage: %+v", got)
	}
}

func TestExtractStreamTokenUsagePromptTokensDetailsCache(t *testing.T) {
	got, ok := usage.ExtractStreamTokenUsage(`{"usage":{"prompt_tokens":30,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens":5,"total_tokens":35}}`)
	if !ok {
		t.Fatal("expected OpenAI-compatible stream usage")
	}
	if got.InputTokens != 10 || got.CacheReadTokens != 20 || got.OutputTokens != 5 || got.TotalTokens != 35 {
		t.Fatalf("unexpected usage: %+v", got)
	}
}

func TestExtractStreamTokenUsageReasoningTokens(t *testing.T) {
	got, ok := usage.ExtractStreamTokenUsage(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"completion_tokens_details":{"reasoning_tokens":7}}}`)
	if !ok {
		t.Fatal("expected OpenAI-compatible stream usage")
	}
	if got.OutputTokens != 20 || got.ReasoningTokens != 7 || got.TotalTokens != 30 {
		t.Fatalf("unexpected usage: %+v", got)
	}
}

func TestExtractAnthropicStreamTokens(t *testing.T) {
	input, output, ok := usage.ExtractAnthropicStreamTokens("message_start", `{"message":{"usage":{"input_tokens":15}}}`)
	if !ok {
		t.Fatal("expected Anthropic message_start usage")
	}
	if input != 15 || output != 0 {
		t.Fatalf("expected 15/0 tokens, got %d/%d", input, output)
	}

	input, output, ok = usage.ExtractAnthropicStreamTokens("message_delta", `{"usage":{"output_tokens":25}}`)
	if !ok {
		t.Fatal("expected Anthropic message_delta usage")
	}
	if input != 0 || output != 25 {
		t.Fatalf("expected 0/25 tokens, got %d/%d", input, output)
	}
}

func TestExtractAnthropicStreamTokensUnknownEvent(t *testing.T) {
	input, output, ok := usage.ExtractAnthropicStreamTokens("content_block_delta", `{"delta":{"text":"hello"}}`)
	if ok {
		t.Fatalf("expected no usage for content delta, got %d/%d", input, output)
	}
}

func TestCalculateCost(t *testing.T) {
	costTable := usage.CostTable{
		InputPricePerMillion:      3.00,
		OutputPricePerMillion:     15.00,
		CacheReadPricePerMillion:  0.30,
		CacheWritePricePerMillion: 3.75,
	}

	cost := usage.CalculateCost(1000, 500, 0, 0, costTable)
	expected := (1000.0/1_000_000*3.00 + 500.0/1_000_000*15.00) * 100
	if abs(cost-expected) > 1e-9 {
		t.Errorf("expected %f, got %f (diff: %e)", expected, cost, cost-expected)
	}

	// Test with cache tokens.
	costWithCache := usage.CalculateCost(1000, 500, 2000, 1000, costTable)
	expectedWithCache := expected +
		(2000.0 / 1_000_000 * 0.30 * 100) +
		(1000.0 / 1_000_000 * 3.75 * 100)
	if abs(costWithCache-expectedWithCache) > 1e-9 {
		t.Errorf("expected %f, got %f (diff: %e)", expectedWithCache, costWithCache, costWithCache-expectedWithCache)
	}
}

func TestCalculateCostDeepSeekCacheCents(t *testing.T) {
	cost := usage.CalculateCost(1000, 500, 2000, 0, usage.DefaultCosts["deepseek-v4-flash"])
	expected := (1000.0/1_000_000*0.14 + 2000.0/1_000_000*0.0028 + 500.0/1_000_000*0.28) * 100
	if abs(cost-expected) > 1e-9 {
		t.Errorf("expected %f, got %f (diff: %e)", expected, cost, cost-expected)
	}
}

func TestCalculateCostOpenAICacheCents(t *testing.T) {
	cost := usage.CalculateCost(1000, 500, 2000, 0, usage.DefaultCosts["gpt-4o"])
	expected := (1000.0/1_000_000*2.50 + 2000.0/1_000_000*1.25 + 500.0/1_000_000*10.00) * 100
	if abs(cost-expected) > 1e-9 {
		t.Errorf("expected %f, got %f (diff: %e)", expected, cost, cost-expected)
	}
}

func TestCalculateCostZAIGLMCents(t *testing.T) {
	cost := usage.CalculateCost(1_000_000, 500_000, 250_000, 0, usage.DefaultCosts["glm-5.2"])
	expected := (1.40 + 0.5*4.40 + 0.25*0.26) * 100
	if abs(cost-expected) > 1e-9 {
		t.Errorf("expected %f, got %f (diff: %e)", expected, cost, cost-expected)
	}
}

func TestDeepSeekCostsReconcileToOfficialBillingExport(t *testing.T) {
	// Official DeepSeek billing export for 2026-06-01 through 2026-06-03.
	// Provider cost CSV is in dollars; CalculateCost returns cents.
	rows := []struct {
		date                string
		model               string
		cacheMissTokens     int64
		cacheHitTokens      int64
		outputTokens        int64
		officialCostDollars float64
	}{
		{"2026-06-01", "deepseek-v4-flash", 2082665, 206373376, 695399, 1.0641302728},
		{"2026-06-01", "deepseek-v4-pro", 313034, 15805312, 82199, 0.2649771760},
		{"2026-06-02", "deepseek-v4-flash", 404684, 9862528, 48734, 0.0979163584},
		{"2026-06-02", "deepseek-v4-pro", 13988, 13678848, 30209, 0.0819524340},
		{"2026-06-03", "deepseek-v4-flash", 391763, 10485504, 116979, 0.1169603512},
		{"2026-06-03", "deepseek-v4-pro", 175475, 4400768, 13472, 0.1040050490},
	}

	var gotTotalCents float64
	var wantTotalDollars float64
	for _, row := range rows {
		t.Run(row.date+"/"+row.model, func(t *testing.T) {
			gotCents := usage.CalculateCost(
				row.cacheMissTokens,
				row.outputTokens,
				row.cacheHitTokens,
				0,
				usage.DefaultCosts[row.model],
			)
			wantCents := row.officialCostDollars * 100
			if abs(gotCents-wantCents) > 1e-9 {
				t.Fatalf("expected %.12f cents, got %.12f cents (diff: %.12f)", wantCents, gotCents, gotCents-wantCents)
			}
		})
		gotTotalCents += usage.CalculateCost(
			row.cacheMissTokens,
			row.outputTokens,
			row.cacheHitTokens,
			0,
			usage.DefaultCosts[row.model],
		)
		wantTotalDollars += row.officialCostDollars
	}

	if abs(gotTotalCents-(wantTotalDollars*100)) > 1e-9 {
		t.Fatalf("expected total %.12f cents, got %.12f cents", wantTotalDollars*100, gotTotalCents)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestGetCost(t *testing.T) {
	table := usage.NewCostTable()
	cost := table.GetCost("gpt-4o")
	if abs(cost.InputPricePerMillion-0.25) > 1e-12 {
		t.Errorf("expected discounted 0.25 for gpt-4o input, got %f", cost.InputPricePerMillion)
	}
	if abs(cost.CacheReadPricePerMillion-0.125) > 1e-12 {
		t.Errorf("expected discounted 0.125 for gpt-4o cached input, got %f", cost.CacheReadPricePerMillion)
	}

	mini := table.GetCost("gpt-5.4-mini")
	if abs(mini.InputPricePerMillion-0.075) > 1e-12 || abs(mini.CacheReadPricePerMillion-0.0075) > 1e-12 || abs(mini.OutputPricePerMillion-0.45) > 1e-12 {
		t.Errorf("unexpected gpt-5.4-mini pricing: %+v", mini)
	}

	snapshot := table.GetCost("gpt-4.1-mini-2025-04-14")
	if abs(snapshot.InputPricePerMillion-0.04) > 1e-12 || abs(snapshot.CacheReadPricePerMillion-0.01) > 1e-12 || abs(snapshot.OutputPricePerMillion-0.16) > 1e-12 {
		t.Errorf("unexpected gpt-4.1-mini snapshot pricing: %+v", snapshot)
	}

	unknownGPT := table.GetCost("gpt-new-codex")
	if abs(unknownGPT.InputPricePerMillion-0.30) > 1e-12 || abs(unknownGPT.OutputPricePerMillion-1.50) > 1e-12 {
		t.Errorf("unexpected discounted fallback pricing: %+v", unknownGPT)
	}

	// Unknown model should return default.
	unknown := table.GetCost("unknown-model")
	if unknown.InputPricePerMillion <= 0 {
		t.Errorf("expected non-zero default cost, got %f", unknown.InputPricePerMillion)
	}
}

func TestSetCost(t *testing.T) {
	table := usage.NewCostTable()
	table.SetCost("custom-model", usage.CostTable{
		InputPricePerMillion:  1.00,
		OutputPricePerMillion: 2.00,
	})
	cost := table.GetCost("custom-model")
	if cost.InputPricePerMillion != 1.00 {
		t.Errorf("expected 1.00, got %f", cost.InputPricePerMillion)
	}
}

func TestEstimateTextTokens(t *testing.T) {
	tokens := usage.EstimateTextTokens("hello world, this is a test")
	if tokens <= 0 {
		t.Errorf("expected positive token count, got %d", tokens)
	}
	// 32 chars / 4 = 8 tokens (approx)
	if tokens < 5 || tokens > 15 {
		t.Errorf("expected ~8 tokens, got %d", tokens)
	}
}

func TestDefaultCosts(t *testing.T) {
	if len(usage.DefaultCosts) == 0 {
		t.Error("expected non-empty default costs")
	}
	if _, ok := usage.DefaultCosts["gpt-4o"]; !ok {
		t.Error("expected gpt-4o in default costs")
	}
	if _, ok := usage.DefaultCosts["claude-sonnet-4"]; !ok {
		t.Error("expected claude-sonnet-4 in default costs")
	}
}
