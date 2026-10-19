import test from "node:test";
import assert from "node:assert/strict";
import { parseSSEChunk, readEventStream } from "../src/lib/stream.ts";

function frame(name: string, payload: unknown): string {
  return `event: ${name}\ndata: ${JSON.stringify(payload)}\n\n`;
}

test("parseSSEChunk returns complete frames and keeps the remainder", () => {
  const buffer = frame("delta", { text: "hello" }) + "event: delta\ndata: {\"text\":\"par";

  const { events, rest } = parseSSEChunk(buffer);

  assert.equal(events.length, 1);
  assert.equal(events[0]!.type, "delta");
  assert.deepEqual(events[0]!.data, { text: "hello" });
  assert.equal(rest, "event: delta\ndata: {\"text\":\"par");
});

// A network read can split a frame anywhere, including mid-JSON. Losing the
// remainder would silently drop tokens from the middle of a reply.
test("a frame split across reads is reassembled", async () => {
  const whole = frame("delta", { text: "hello world" });
  const split = Math.floor(whole.length / 2);

  const received: string[] = [];
  await readEventStream(streamOf([whole.slice(0, split), whole.slice(split)]), (event) => {
    if (event.type === "delta") received.push(event.data.text);
  });

  assert.deepEqual(received, ["hello world"]);
});

test("events arrive in order across a multi-event stream", async () => {
  const chunks = [
    frame("start", { user_message_id: "m1", model: "x", provider_id: "p", buffered: false, memory_degraded: false }),
    frame("delta", { text: "one " }),
    frame("delta", { text: "two" }),
    frame("usage", { input_tokens: 10, output_tokens: 4, total_tokens: 14 }),
    frame("done", { status: "ok", message_id: "m2" }),
  ];

  const names: string[] = [];
  let text = "";
  await readEventStream(streamOf(chunks), (event) => {
    names.push(event.type);
    if (event.type === "delta") text += event.data.text;
  });

  assert.deepEqual(names, ["start", "delta", "delta", "usage", "done"]);
  assert.equal(text, "one two");
});

// One malformed frame must not abort a turn that is otherwise streaming fine.
test("an undecodable frame is skipped rather than throwing", () => {
  const buffer = "event: delta\ndata: {not json}\n\n" + frame("delta", { text: "ok" });

  const { events } = parseSSEChunk(buffer);

  assert.equal(events.length, 1);
  assert.deepEqual(events[0]!.data, { text: "ok" });
});

test("unknown event names are ignored", () => {
  const { events } = parseSSEChunk(frame("something-new", { a: 1 }) + frame("delta", { text: "x" }));

  assert.equal(events.length, 1);
  assert.equal(events[0]!.type, "delta");
});

// A blocked turn is a policy decision, not a fault, and the UI renders the two
// differently — so they must stay distinguishable in the parsed stream.
test("blocked and error are separate event types", () => {
  const { events } = parseSSEChunk(
    frame("blocked", { type: "dlp_blocked", message: "refused" }) +
      frame("error", { type: "upstream_error", message: "boom" }),
  );

  assert.deepEqual(
    events.map((e) => e.type),
    ["blocked", "error"],
  );
});

test("a final frame without a trailing blank line is still delivered", async () => {
  const partial = "event: done\ndata: {\"status\":\"ok\"}";

  const names: string[] = [];
  await readEventStream(streamOf([partial]), (event) => names.push(event.type));

  assert.deepEqual(names, ["done"]);
});

function streamOf(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}
