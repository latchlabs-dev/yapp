import test from "node:test";
import assert from "node:assert/strict";
import { formatProviderBalance } from "../src/lib/provider-balance.ts";

test("valid ZAI tokens remain visible when the quota service is unavailable", () => {
  assert.equal(
    formatProviderBalance({
      provider_account_id: "zai",
      billing_mode: "subscription",
      status: "success",
      normalization_version: 1,
      normalization_error: "ZAI token is valid, but upstream quota usage is unavailable",
      observed_at: "2026-09-02T00:00:00Z",
    }),
    "Valid · usage unavailable",
  );
});
