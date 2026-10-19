import assert from "node:assert/strict";
import test from "node:test";
import { safeLoginReturn } from "../src/lib/login.ts";
import { api, setCsrfToken } from "../src/lib/api.ts";

test("login return remains inside the portal", () => {
  for (const value of [null, "", "https://evil.test", "//evil.test", "/\\evil.test", "/\n/evil.test", "javascript:alert(1)"]) {
    assert.equal(safeLoginReturn(value), "/");
  }
  assert.equal(safeLoginReturn("/c/example?tab=memory#end"), "/c/example?tab=memory#end");
});

test("key login submits JSON, uses cookies and never places the key in the URL or auth headers", async () => {
  const original = globalThis.fetch;
  const key = "test-proxy-credential";
  let called = false;
  globalThis.fetch = async (url, options) => {
    called = true;
    assert.equal(url, "/web/v1/auth/key");
    assert.equal(options?.method, "POST");
    assert.equal(options?.credentials, "same-origin");
    assert.equal(options?.body, JSON.stringify({ key }));
    const headers = new Headers(options?.headers);
    assert.equal(headers.get("Content-Type"), "application/json");
    assert.equal(headers.get("Authorization"), null);
    return new Response(JSON.stringify({ status: "signed_in" }), { status: 200 });
  };
  try {
    setCsrfToken("");
    assert.equal((await api.loginWithKey(key)).status, "signed_in");
    assert.equal(called, true);
  } finally {
    globalThis.fetch = original;
  }
});
