import test from "node:test";
import assert from "node:assert/strict";
import {
  bindingScope,
  effectiveViews,
  formatCents,
  hasAnyLimit,
  scopeViews,
} from "../src/lib/quota.ts";
import type { QuotaPeriod, QuotaResponse } from "../src/lib/types.ts";

function period(over: Partial<QuotaPeriod> = {}): QuotaPeriod {
  return {
    used_millicents: 0,
    used_cents: 0,
    used_percent: null,
    limit_cents: 0,
    remaining_millicents: null,
    remaining_cents: null,
    remaining_percent: null,
    resets_at: "2026-09-12T00:00:00Z",
    unlimited: true,
    exhausted: false,
    ...over,
  };
}

// The exhausted $10 daily tenant limit that the console used to render as
// "No spend limits apply to your account." The response shape is the handler's:
// scopes[] with nested daily/monthly, not a flat balances[].
const exhausted: QuotaResponse = {
  currency: "USD",
  as_of: "2026-09-11T12:00:00Z",
  daily: {
    ...period({
      used_cents: 1000,
      used_percent: 100,
      limit_cents: 1000,
      remaining_cents: 0,
      remaining_percent: 0,
      unlimited: false,
      exhausted: true,
    }),
    binding_scope_type: "tenant",
    binding_scope_id: "tnt_1",
  },
  monthly: period(),
  scopes: [
    {
      scope_type: "tenant",
      scope_id: "tnt_1",
      daily: period({
        used_cents: 1000,
        limit_cents: 1000,
        remaining_cents: 0,
        remaining_percent: 0,
        unlimited: false,
        exhausted: true,
      }),
      monthly: period(),
    },
  ],
};

test("an exhausted limit is not reported as no limit", () => {
  assert.equal(hasAnyLimit(exhausted), true);

  const [today] = effectiveViews(exhausted);
  assert.equal(today!.unlimited, false);
  assert.equal(today!.exhausted, true);
  assert.equal(formatCents(today!.remainingCents), "$0.00");
  assert.equal(formatCents(today!.limitCents), "$10.00");
  assert.equal(bindingScope(exhausted, "daily"), "Organisation");
});

test("scope balances keep their scope identity and per-period limits", () => {
  const [scope] = scopeViews(exhausted);
  assert.equal(scope!.label, "Organisation");
  assert.equal(scope!.scopeId, "tnt_1");
  assert.equal(scope!.hasLimit, true);
  assert.deepEqual(
    scope!.periods.map((p) => p.label),
    ["Today", "This month"],
  );
  assert.equal(scope!.periods[1]!.unlimited, true);
});

// A failed load has no response at all. Answering "no limits" for it is the bug
// that made an exhausted account look unconstrained.
test("an absent response is never reported as unlimited", () => {
  assert.equal(hasAnyLimit(undefined), false);
  assert.equal(hasAnyLimit(null), false);
  assert.deepEqual(effectiveViews(undefined), []);
  assert.deepEqual(scopeViews(undefined), []);
  assert.equal(bindingScope(undefined, "daily"), "");
});

test("a genuinely unlimited account reports no limits", () => {
  const open: QuotaResponse = {
    currency: "USD",
    as_of: "2026-09-11T12:00:00Z",
    daily: period(),
    monthly: period(),
    scopes: [{ scope_type: "user", scope_id: "usr_1", daily: period(), monthly: period() }],
  };

  assert.equal(hasAnyLimit(open), false);
  assert.equal(bindingScope(open, "monthly"), "");
  assert.equal(scopeViews(open)[0]!.hasLimit, false);
});

test("cent amounts render as dollars", () => {
  assert.equal(formatCents(0), "$0.00");
  assert.equal(formatCents(1234.5), "$12.35");
  assert.equal(formatCents(null), "—");
  assert.equal(formatCents(undefined), "—");
});
