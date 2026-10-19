import test from "node:test";
import assert from "node:assert/strict";
import { calculateDecayedMonthlyCostProjection } from "../src/lib/usage-projection.ts";
import type { UsageReport } from "../src/lib/types.ts";

function report(day: string, cents: number): UsageReport {
  return {
    period_start: `${day}T00:00:00Z`,
    period_end: `${day}T23:59:59Z`,
    granularity: "daily",
    request_count: 1,
    input_tokens: 0,
    output_tokens: 0,
    reasoning_tokens: 0,
    total_tokens: 0,
    cost_total_cents: cents,
  };
}

test("decayed projection includes zero-spend elapsed days", () => {
  const projection = calculateDecayedMonthlyCostProjection(
    [report("2026-07-01", 1000)],
    new Date("2026-07-08T12:00:00Z"),
    7,
  );

  assert.equal(projection.monthToDateCents, 1000);
  assert.ok(projection.weightedDailyCents < 200, `weighted daily was ${projection.weightedDailyCents}`);
  assert.ok(projection.projectedEndOfMonthCents > 1000);
});

test("today is normalized for elapsed fraction and downweighted", () => {
  const noon = new Date("2026-07-10T12:00:00Z");
  const projection = calculateDecayedMonthlyCostProjection(
    [report("2026-07-10", 500)],
    noon,
    7,
  );

  assert.equal(projection.monthToDateCents, 500);
  assert.ok(projection.weightedDailyCents > 0, `weighted daily was ${projection.weightedDailyCents}`);
  assert.ok(projection.weightedDailyCents < 1000, `weighted daily was ${projection.weightedDailyCents}`);
  assert.ok(projection.projectedEndOfMonthCents > projection.monthToDateCents);
});

test("recent spend carries more forecast weight than older spend", () => {
  const now = new Date("2026-07-15T12:00:00Z");
  const recent = calculateDecayedMonthlyCostProjection([report("2026-07-15", 1000)], now, 3);
  const old = calculateDecayedMonthlyCostProjection([report("2026-07-01", 1000)], now, 3);

  assert.equal(recent.monthToDateCents, old.monthToDateCents);
  assert.ok(recent.weightedDailyCents > old.weightedDailyCents);
});
