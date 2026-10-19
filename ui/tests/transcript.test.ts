import test from "node:test";
import assert from "node:assert/strict";
import {
  DEFAULT_TRANSCRIPT_FILTER,
  countHidden,
  formatBytes,
  mergeTranscriptPages,
  sessionTitle,
  toolCallSummary,
  turnNotice,
  visibleItems,
} from "../src/lib/transcript.ts";
import type { SessionDisplayItem, SessionTranscriptTurn } from "../src/lib/types.ts";

function turn(seq: number, items: SessionDisplayItem[], output: SessionDisplayItem[] = []): SessionTranscriptTurn {
  return {
    seq,
    request_id: `req_${seq}`,
    created_at: "2026-09-11T08:00:00Z",
    provider: "openai-codex-responses",
    model: "gpt",
    status: "success",
    api: "responses",
    kind: "extend",
    base: 0,
    input_count: items.length,
    items,
    output,
  };
}

test("visibleItems hides echoes by default and honors tool/reasoning toggles", () => {
  const items: SessionDisplayItem[] = [
    { kind: "assistant", text: "repeat", echo: true },
    { kind: "tool_result", text: "out" },
    { kind: "reasoning", text: "why" },
    { kind: "user", text: "next" },
  ];
  assert.deepEqual(visibleItems(items, DEFAULT_TRANSCRIPT_FILTER).map((i) => i.kind), ["tool_result", "reasoning", "user"]);
  assert.deepEqual(
    visibleItems(items, { showTools: false, showReasoning: false, showEchoes: true }).map((i) => i.kind),
    ["assistant", "user"],
  );
});

test("countHidden counts each suppressed category once", () => {
  const turns = [
    turn(1, [{ kind: "user", text: "go" }], [{ kind: "tool_call", name: "bash" }, { kind: "reasoning", text: "r" }]),
    turn(2, [{ kind: "tool_call", name: "bash", echo: true }, { kind: "tool_result", text: "ok" }]),
  ];
  assert.deepEqual(countHidden(turns, DEFAULT_TRANSCRIPT_FILTER), { tools: 0, reasoning: 0, echoes: 1 });
  assert.deepEqual(countHidden(turns, { showTools: false, showReasoning: false, showEchoes: true }), { tools: 2, reasoning: 1, echoes: 0 });
});

test("turnNotice explains non-extending turns", () => {
  assert.equal(turnNotice({ kind: "extend", base: 4 }), "");
  assert.match(turnNotice({ kind: "reset", base: 0 }), /from the start/);
  assert.match(turnNotice({ kind: "reset", base: 33 }), /after item 33/);
  assert.match(turnNotice({ kind: "prefix", base: 2 }), /earlier point/);
  assert.match(turnNotice({ kind: "unparsed", base: 2, request_truncated: true }), /truncated/);
});

test("mergeTranscriptPages orders by seq and drops refetched duplicates", () => {
  const merged = mergeTranscriptPages([{ turns: [turn(3, []), turn(1, [])] }, { turns: [turn(2, []), turn(3, [{ kind: "user", text: "fresh" }])] }]);
  assert.deepEqual(merged.map((t) => t.seq), [1, 2, 3]);
  assert.equal(merged[2]?.items[0]?.text, "fresh");
});

test("formatBytes and sessionTitle", () => {
  assert.equal(formatBytes(512), "512 B");
  assert.equal(formatBytes(1536), "1.5 KB");
  assert.equal(formatBytes(79 * 1024 * 1024), "79 MB");
  assert.equal(formatBytes(undefined), "—");
  assert.equal(sessionTitle({ opening_prompt: "\n  fix the parser\nmore", conversation_id: "c" }), "fix the parser");
  assert.equal(sessionTitle({ opening_prompt: "", conversation_id: "conv-1" }), "conv-1");
  assert.equal(sessionTitle({ opening_prompt: "x", conversation_id: "c" }, "Named"), "Named");
  assert.equal(sessionTitle({ opening_prompt: "a".repeat(200), conversation_id: "c" }, undefined, 10), `${"a".repeat(9)}…`);
});

test("toolCallSummary ranks tools by use", () => {
  assert.deepEqual(toolCallSummary([{ tool_names: ["read", "bash"] }, { tool_names: ["bash"] }, { tool_names: [] }]), [
    { name: "bash", count: 2 },
    { name: "read", count: 1 },
  ]);
});
