import test from "node:test";
import assert from "node:assert/strict";
import { applyTurnEvent, type LiveTurn } from "../src/lib/turn.ts";
import type { TurnEvent } from "../src/lib/stream.ts";

function blank(): LiveTurn {
  return {
    conversationId: "wcv_1",
    token: 1,
    input: { content: "why is the build red?", attachments: [] },
    status: "streaming",
    accepted: true,
    text: "",
    reasoning: "",
    notices: [],
    annotations: {},
  };
}

function fold(events: TurnEvent[], from: LiveTurn = blank()): LiveTurn {
  return events.reduce(applyTurnEvent, from);
}

test("deltas accumulate into the reply and reasoning stays separate", () => {
  const turn = fold([
    { type: "delta", data: { text: "Because " } },
    { type: "reasoning", data: { text: "checking CI" } },
    { type: "delta", data: { text: "the migration failed." } },
  ]);

  assert.equal(turn.text, "Because the migration failed.");
  assert.equal(turn.reasoning, "checking CI");
});

test("the start event is kept so degraded memory can be reported", () => {
  const turn = fold([
    {
      type: "start",
      data: {
        user_message_id: "wmsg_1",
        model: "corp-a",
        provider_id: "acct_1",
        buffered: false,
        memory_degraded: true,
        memory_reason: "duffel unreachable",
      },
    },
  ]);

  assert.equal(turn.start!.user_message_id, "wmsg_1");
  assert.equal(turn.start!.memory_degraded, true);
});

test("notices and annotations accumulate without losing earlier ones", () => {
  const turn = fold([
    { type: "notice", data: { message: "big.pdf was not sent" } },
    { type: "notice", data: { annotations: { review: "rewritten" } } },
    { type: "notice", data: { message: "scan.pdf has no extractable text", annotations: { pii: "redacted" } } },
  ]);

  assert.deepEqual(turn.notices, [
    "big.pdf was not sent",
    "scan.pdf has no extractable text",
  ]);
  assert.deepEqual(turn.annotations, { review: "rewritten", pii: "redacted" });
});

// A hook refusal and an upstream failure read differently to a user, so they
// stay separate fields rather than collapsing into one error string.
test("blocked and error are recorded separately", () => {
  const blocked = fold([{ type: "blocked", data: { type: "hook_blocked", message: "blocked by policy" } }]);
  assert.equal(blocked.blocked, "blocked by policy");
  assert.equal(blocked.error, undefined);

  const failed = fold([{ type: "error", data: { type: "provider_unavailable", message: "upstream down" } }]);
  assert.equal(failed.error, "upstream down");
  assert.equal(failed.blocked, undefined);
});

// Whatever streamed before the failure is kept: it is the only copy the user
// has of a reply that was paid for.
test("an error after partial text keeps the text", () => {
  const turn = fold([
    { type: "delta", data: { text: "Here is what I foun" } },
    { type: "error", data: { type: "upstream_error", message: "connection reset" } },
  ]);

  assert.equal(turn.text, "Here is what I foun");
  assert.equal(turn.error, "connection reset");
});

// The submitted input rides along on the turn, which is what makes retry
// possible without asking the user to retype anything.
test("the submitted input survives every event", () => {
  const input = {
    content: "summarise the attached log",
    attachments: [{ id: "watt_1", filename: "build.log" }],
    model: "corp-a",
    providerId: "acct_2",
  };
  const turn = fold(
    [
      { type: "delta", data: { text: "..." } },
      { type: "error", data: { type: "x", message: "boom" } },
    ],
    { ...blank(), input },
  );

  assert.deepEqual(turn.input, input);
});

test("folding an event never mutates the turn it was given", () => {
  const before = blank();
  const after = applyTurnEvent(before, { type: "delta", data: { text: "hi" } });

  assert.equal(before.text, "");
  assert.equal(after.text, "hi");
  assert.notEqual(before, after);
});

test("usage and done are folded without disturbing the reply", () => {
  const turn = fold([
    { type: "delta", data: { text: "done" } },
    { type: "usage", data: { input_tokens: 11, output_tokens: 3, total_tokens: 14 } },
    { type: "done", data: { status: "ok", message_id: "wmsg_2" } },
  ]);

  assert.equal(turn.usage!.total_tokens, 14);
  assert.equal(turn.text, "done");
});
