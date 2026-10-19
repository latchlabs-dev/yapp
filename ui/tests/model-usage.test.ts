import test from "node:test";
import assert from "node:assert/strict";
import { aggregateModels } from "../src/lib/model-usage.ts";
import type { UsageReport } from "../src/lib/types.ts";

function report(model: string | undefined, cost: number, tokens: number, requests = 1): UsageReport {
  return {
    period_start: "2026-07-01T00:00:00Z",
    period_end: "2026-07-01T23:59:59Z",
    granularity: "daily",
    model,
    request_count: requests,
    input_tokens: Math.floor(tokens / 2),
    output_tokens: Math.ceil(tokens / 2),
    reasoning_tokens: 3,
    total_tokens: tokens,
    cost_total_cents: cost,
  };
}

test("aggregateModels totals costs and sorts by model cost", () => {
  const rows = aggregateModels([
    report("cheap", 20, 1_000, 2),
    report("expensive", 120, 2_000, 1),
    report("cheap", 30, 3_000, 3),
    report(undefined, 10, 500, 1),
  ], "cost");

  assert.deepEqual(rows.map((r) => r.model), ["expensive", "cheap", "unknown"]);
  assert.equal(rows[1]?.cost, 50);
  assert.equal(rows[1]?.requests, 5);
  assert.equal(rows[1]?.tokens, 4_000);
  assert.equal(rows[1]?.costPerRequest, 10);
  assert.equal(rows[1]?.costPerMillionTokens, 12_500);
});

test("aggregateModels can limit token-volume rankings", () => {
  const rows = aggregateModels([
    report("low-cost-high-volume", 1, 10_000),
    report("high-cost-low-volume", 100, 100),
  ], "tokens", 1);

  assert.deepEqual(rows.map((r) => r.model), ["low-cost-high-volume"]);
});
