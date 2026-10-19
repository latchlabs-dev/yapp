import assert from "node:assert/strict";
import { test } from "node:test";

import { AuthenticatedBridge, isBlockedRawMethod, parseAuthorizationDuration } from "../src/index.js";

const TOKEN = "test-token-that-is-deliberately-longer-than-thirty-two-chars";

function headers(token = TOKEN) {
  return {
    Authorization: `Bearer ${token}`,
    "Content-Type": "application/json",
    "X-Pi-Terminal-Browser-Version": "0.1.0-test",
  };
}

test("bridge rejects unauthenticated callers and reports paired health", async () => {
  const bridge = new AuthenticatedBridge({ token: TOKEN, port: 0 });
  await bridge.start();
  try {
    const unauthenticated = await fetch(`http://127.0.0.1:${bridge.port}/v1/status`);
    assert.equal(unauthenticated.status, 401);

    const authenticated = await fetch(`http://127.0.0.1:${bridge.port}/v1/status`, { headers: headers() });
    assert.equal(authenticated.status, 200);
    assert.deepEqual(await authenticated.json(), { ok: true, protocolVersion: 1 });
    assert.equal(bridge.status().extensionConnected, true);
    assert.equal(bridge.status().extensionVersion, "0.1.0-test");
  } finally {
    await bridge.close();
  }
});

test("bridge delivers one command and correlates its result", async () => {
  const bridge = new AuthenticatedBridge({ token: TOKEN, port: 0 });
  await bridge.start();
  try {
    const outcome = bridge.send("terminal.write", { text: "printf ok", submit: true }, 2000);
    const poll = await fetch(`http://127.0.0.1:${bridge.port}/v1/poll`, { headers: headers() });
    assert.equal(poll.status, 200);
    const command = await poll.json() as { id: string; action: string; params: Record<string, unknown> };
    assert.equal(command.action, "terminal.write");
    assert.deepEqual(command.params, { text: "printf ok", submit: true });

    const result = await fetch(`http://127.0.0.1:${bridge.port}/v1/result`, {
      method: "POST",
      headers: headers(),
      body: JSON.stringify({ id: command.id, ok: true, result: { insertedChars: 9 } }),
    });
    assert.equal(result.status, 200);
    assert.deepEqual(await outcome, { insertedChars: 9 });

    const empty = await fetch(`http://127.0.0.1:${bridge.port}/v1/poll`, { headers: headers() });
    assert.equal(empty.status, 204);
  } finally {
    await bridge.close();
  }
});

test("bridge never redelivers a terminal mutation with an unknown outcome", async () => {
  const bridge = new AuthenticatedBridge({ token: TOKEN, port: 0 });
  await bridge.start();
  try {
    const outcome = bridge.send("terminal.write", { text: "danger", submit: true }, 100);
    const first = await fetch(`http://127.0.0.1:${bridge.port}/v1/poll`, { headers: headers() });
    assert.equal(first.status, 200);
    const second = await fetch(`http://127.0.0.1:${bridge.port}/v1/poll`, { headers: headers() });
    assert.equal(second.status, 204);
    await assert.rejects(outcome, /timed out/);
  } finally {
    await bridge.close();
  }
});

test("rotating the token invalidates the old pairing immediately", async () => {
  const bridge = new AuthenticatedBridge({ token: TOKEN, port: 0 });
  await bridge.start();
  try {
    const next = await bridge.rotateToken();
    assert.notEqual(next, TOKEN);
    assert.ok(next.length >= 32);
    const oldResponse = await fetch(`http://127.0.0.1:${bridge.port}/v1/status`, { headers: headers() });
    assert.equal(oldResponse.status, 401);
    const newResponse = await fetch(`http://127.0.0.1:${bridge.port}/v1/status`, { headers: headers(next) });
    assert.equal(newResponse.status, 200);
  } finally {
    await bridge.close();
  }
});

test("authorization durations are bounded", () => {
  assert.deepEqual(parseAuthorizationDuration("30m", 1000), { label: "30m", until: 1_801_000 });
  assert.equal(parseAuthorizationDuration("0m", 1000), null);
  assert.equal(parseAuthorizationDuration("481m", 1000), null);
  assert.equal(parseAuthorizationDuration("indefinite", 1000), null);
});

test("raw CDP denylist covers destructive browser-wide commands", () => {
  assert.equal(isBlockedRawMethod("Browser.close"), true);
  assert.equal(isBlockedRawMethod("Storage.clearDataForOrigin"), true);
  assert.equal(isBlockedRawMethod("Network.clearBrowserCookies"), true);
  assert.equal(isBlockedRawMethod("Runtime.evaluate"), false);
  assert.equal(isBlockedRawMethod("Input.insertText"), false);
});
