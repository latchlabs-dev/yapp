import test from "node:test";
import assert from "node:assert/strict";
import { announceTurn } from "../src/lib/announce.ts";
import type { LiveTurn } from "../src/lib/turn.ts";

function turn(over: Partial<LiveTurn> = {}): LiveTurn {
  return {
    conversationId: "wcv_1",
    token: 1,
    input: { content: "hello", attachments: [] },
    status: "streaming",
    accepted: true,
    text: "",
    reasoning: "",
    notices: [],
    annotations: {},
    ...over,
  };
}

test("sending and receiving are announced as distinct states", () => {
  assert.deepEqual(announceTurn(turn(), false), {
    text: "Message sent. Waiting for the reply.",
    assertive: false,
  });
  assert.deepEqual(announceTurn(turn({ text: "Because" }), false), {
    text: "The reply is arriving.",
    assertive: false,
  });
});

// A failure must interrupt: a reader who has moved on would otherwise never
// learn the turn never happened.
test("failures and refusals are assertive", () => {
  const failed = announceTurn(turn({ status: "failed", error: "upstream down" }), false);
  assert.equal(failed.assertive, true);
  assert.match(failed.text, /upstream down/);

  const blocked = announceTurn(turn({ status: "failed", blocked: "blocked by policy" }), false);
  assert.equal(blocked.assertive, true);
  assert.match(blocked.text, /blocked by policy/);
});

test("stopping and completing are announced politely", () => {
  assert.deepEqual(announceTurn(turn({ status: "stopped", text: "partial" }), false), {
    text: "You stopped the reply.",
    assertive: false,
  });
  assert.deepEqual(announceTurn(turn({ status: "done", text: "all of it" }), false), {
    text: "The reply is complete.",
    assertive: false,
  });
});

// The region must fall silent when there is nothing happening, or a screen
// reader repeats the last thing it was told.
test("an idle thread says nothing", () => {
  assert.deepEqual(announceTurn(null, false), { text: "", assertive: false });
  assert.equal(announceTurn(null, true).text, "Loading the conversation.");
});
