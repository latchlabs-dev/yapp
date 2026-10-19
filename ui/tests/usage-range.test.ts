import test from "node:test";
import assert from "node:assert/strict";
import {
  formatUsagePeriod,
  getUsageRangePreset,
  isGranularityAvailable,
  usageRangeWindow,
} from "../src/lib/usage-range.ts";

test("long-range presets choose readable default groupings", () => {
  assert.equal(getUsageRangePreset("90d").defaultGranularity, "daily");
  assert.equal(getUsageRangePreset("6mo").defaultGranularity, "daily");
  assert.equal(getUsageRangePreset("12mo").defaultGranularity, "monthly");
  assert.equal(getUsageRangePreset("5y").defaultGranularity, "monthly");
});

test("usage range windows stay anchored and deterministic", () => {
  const window = usageRangeWindow("90d", new Date("2026-08-14T12:30:00Z"));

  assert.equal(window.from, "2026-05-16T12:30:00.000Z");
  assert.equal(window.to, "2026-08-14T12:30:00.000Z");
});

test("granularity availability follows backend range limits", () => {
  assert.equal(isGranularityAvailable("30d", "hourly"), true);
  assert.equal(isGranularityAvailable("90d", "hourly"), false);
  assert.equal(isGranularityAvailable("12mo", "daily"), true);
  assert.equal(isGranularityAvailable("2y", "daily"), false);
  assert.equal(isGranularityAvailable("5y", "monthly"), true);
});

test("usage periods have granularity-aware UTC labels", () => {
  const value = "2026-08-01T00:00:00Z";

  assert.equal(formatUsagePeriod(value, "monthly"), "Aug 2026");
  assert.equal(formatUsagePeriod(value, "daily"), "Aug 1, 2026");
  assert.match(formatUsagePeriod(value, "hourly"), /^Aug 1, 2026, 12 AM$/);
});
