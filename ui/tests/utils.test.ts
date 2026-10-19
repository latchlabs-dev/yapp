import test from "node:test";
import assert from "node:assert/strict";
import { encodeSessionRef, formatCents, formatTokens, timeAgo } from "../src/lib/utils.ts";

test("formatTokens keeps small values exact and abbreviates large values", () => {
  assert.equal(formatTokens(999), "999");
  assert.equal(formatTokens(1_200), "1.2K");
  assert.equal(formatTokens(2_500_000), "2.5M");
});

test("timeAgo handles invalid and recent values", () => {
  assert.equal(timeAgo(null), "—");
  assert.equal(timeAgo("not-a-date"), "—");

  const previousNow = Date.now;
  Date.now = () => new Date("2026-06-30T12:00:00Z").getTime();
  try {
    assert.equal(timeAgo("2026-06-30T11:59:45Z"), "15s ago");
    assert.equal(timeAgo("2026-06-30T11:30:00Z"), "30m ago");
    assert.equal(timeAgo("2026-06-30T09:00:00Z"), "3h ago");
    assert.equal(timeAgo("2026-06-28T12:00:00Z"), "2d ago");
    assert.equal(timeAgo("2026-06-30T12:30:00Z"), "in 30m");
    assert.equal(timeAgo("2026-07-04T12:00:00Z"), "in 4d");
  } finally {
    Date.now = previousNow;
  }
});

test("encodeSessionRef emits URL-safe base64 without padding", () => {
  const encoded = encodeSessionRef({
    conversation_id: "conv/a+b",
    tenant_id: "tenant_1",
    user_id: "user_1",
    from: "2026-06-30T10:00:00Z",
    to: "2026-06-30T10:30:00Z",
  });

  assert.doesNotMatch(encoded, /[+/=]/);

  const padded = encoded.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(encoded.length / 4) * 4, "=");
  assert.deepEqual(JSON.parse(atob(padded)), {
    conversation_id: "conv/a+b",
    tenant_id: "tenant_1",
    user_id: "user_1",
    from: "2026-06-30T10:00:00Z",
    to: "2026-06-30T10:30:00Z",
  });
});

test("formatCents reads as money above a dollar and keeps precision below", () => {
  assert.equal(formatCents(6160.99), "$61.61");
  assert.equal(formatCents(1234567), "$12,345.67");
  assert.equal(formatCents(100), "$1.00");
  assert.equal(formatCents(4.5), "$0.0450");
  assert.equal(formatCents(0), "$0.0000");
  assert.equal(formatCents(-250), "-$2.50");
});
