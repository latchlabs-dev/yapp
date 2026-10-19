// Parsing for the console's normalized event stream. The server collapses
// OpenAI's and Anthropic's very different SSE shapes into one vocabulary, so
// this file only has to understand that one.
//
// Kept free of React and of fetch so it can be unit-tested directly.

export type TurnEvent =
  | { type: "start"; data: StartEvent }
  | { type: "delta"; data: { text: string } }
  | { type: "reasoning"; data: { text: string } }
  | { type: "usage"; data: TokenUsage }
  | { type: "notice"; data: { message?: string; annotations?: Record<string, string> } }
  | { type: "blocked"; data: { type: string; message: string } }
  | { type: "error"; data: { type: string; message: string } }
  | { type: "done"; data: DoneEvent };

export interface StartEvent {
  user_message_id: string;
  model: string;
  provider_id: string;
  project_id?: string;
  /** True when a mutating hook forced the reply to be withheld until reviewed. */
  buffered: boolean;
  /** True when project memory could not be read; the turn ran without it. */
  memory_degraded: boolean;
  memory_reason?: string;
}

export interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
}

export interface DoneEvent {
  status: "ok" | "error";
  message_id?: string;
  usage?: TokenUsage;
}

/**
 * parseSSEChunk splits a buffer into complete SSE frames and returns the
 * parsed events plus whatever partial frame is left over.
 *
 * A network read can split a frame anywhere, including mid-JSON, so the
 * remainder must be carried into the next call rather than discarded.
 */
export function parseSSEChunk(buffer: string): { events: TurnEvent[]; rest: string } {
  const events: TurnEvent[] = [];
  let rest = buffer;

  for (;;) {
    const boundary = rest.indexOf("\n\n");
    if (boundary < 0) break;
    const frame = rest.slice(0, boundary);
    rest = rest.slice(boundary + 2);
    const parsed = parseFrame(frame);
    if (parsed) events.push(parsed);
  }
  return { events, rest };
}

function parseFrame(frame: string): TurnEvent | null {
  let name = "";
  const dataLines: string[] = [];

  for (const rawLine of frame.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.startsWith("event:")) {
      name = line.slice("event:".length).trim();
    } else if (line.startsWith("data:")) {
      dataLines.push(line.slice("data:".length).trim());
    }
  }
  if (!name || dataLines.length === 0) return null;

  let data: unknown;
  try {
    data = JSON.parse(dataLines.join("\n"));
  } catch {
    // A frame we cannot decode is dropped rather than thrown: one malformed
    // event must not abort a turn that is otherwise streaming fine.
    return null;
  }

  switch (name) {
    case "start":
    case "delta":
    case "reasoning":
    case "usage":
    case "notice":
    case "blocked":
    case "error":
    case "done":
      return { type: name, data } as TurnEvent;
    default:
      return null;
  }
}

/**
 * readEventStream pumps a fetch response body through the parser, invoking
 * onEvent for each event in order. It resolves when the stream ends.
 */
export async function readEventStream(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: TurnEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  try {
    for (;;) {
      if (signal?.aborted) return;
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const { events, rest } = parseSSEChunk(buffer);
      buffer = rest;
      for (const event of events) onEvent(event);
    }
    // Flush a final frame that arrived without its trailing blank line.
    const { events } = parseSSEChunk(buffer + "\n\n");
    for (const event of events) onEvent(event);
  } finally {
    reader.releaseLock();
  }
}
