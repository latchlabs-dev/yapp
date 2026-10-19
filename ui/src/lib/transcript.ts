import type {
  SessionArchiveSession,
  SessionArchiveTurn,
  SessionDisplayItem,
  SessionTranscriptPage,
  SessionTranscriptTurn,
} from "./types";

// Pure helpers for the session transcript view. Kept free of React and path
// aliases so node --test can exercise them directly.

export interface TranscriptFilter {
  showTools: boolean;
  showReasoning: boolean;
  // Echoes repeat the previous turn's output inside the next request; the
  // output is already shown, so they are hidden unless asked for.
  showEchoes: boolean;
}

export const DEFAULT_TRANSCRIPT_FILTER: TranscriptFilter = {
  showTools: true,
  showReasoning: true,
  showEchoes: false,
};

const TOOL_KINDS = new Set(["tool_call", "tool_result"]);

export function isVisibleItem(item: SessionDisplayItem, filter: TranscriptFilter): boolean {
  if (item.echo && !filter.showEchoes) return false;
  if (!filter.showTools && TOOL_KINDS.has(item.kind)) return false;
  if (!filter.showReasoning && item.kind === "reasoning") return false;
  return true;
}

export function visibleItems(items: SessionDisplayItem[], filter: TranscriptFilter): SessionDisplayItem[] {
  return items.filter((item) => isVisibleItem(item, filter));
}

// countHidden reports how many items of each hideable category the current
// filter suppresses across the loaded turns, for the toggle labels.
export function countHidden(turns: SessionTranscriptTurn[], filter: TranscriptFilter): { tools: number; reasoning: number; echoes: number } {
  const counts = { tools: 0, reasoning: 0, echoes: 0 };
  for (const turn of turns) {
    for (const item of [...turn.items, ...turn.output]) {
      if (item.echo) {
        if (!filter.showEchoes) counts.echoes++;
        continue;
      }
      if (!filter.showTools && TOOL_KINDS.has(item.kind)) counts.tools++;
      if (!filter.showReasoning && item.kind === "reasoning") counts.reasoning++;
    }
  }
  return counts;
}

// turnNotice explains turns whose request did not simply extend the history.
export function turnNotice(turn: Pick<SessionTranscriptTurn, "kind" | "base" | "request_truncated">): string {
  switch (turn.kind) {
    case "reset":
      return turn.base === 0
        ? "History rewritten from the start (compaction, branch, or a side request)"
        : `History rewritten after item ${turn.base} (compaction, branch, or a side request)`;
    case "prefix":
      return "Resent an earlier point of the conversation (out-of-order or replayed request)";
    case "append":
      return "Chained request: only new items were sent";
    case "unparsed":
      return turn.request_truncated
        ? "Request body was truncated at capture and could not be parsed"
        : "Request body could not be parsed";
    default:
      return "";
  }
}

// mergeTranscriptPages flattens infinite-query pages into one seq-ordered
// list, dropping duplicates that appear when a page is refetched.
export function mergeTranscriptPages(pages: Pick<SessionTranscriptPage, "turns">[]): SessionTranscriptTurn[] {
  const bySeq = new Map<number, SessionTranscriptTurn>();
  for (const page of pages) {
    for (const turn of page.turns) bySeq.set(turn.seq, turn);
  }
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

export function formatBytes(bytes: number | undefined | null): string {
  if (bytes === undefined || bytes === null || !Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

// sessionTitle prefers an explicit session name, then the first line of the
// opening prompt, then the conversation id.
export function sessionTitle(session: Pick<SessionArchiveSession, "opening_prompt" | "conversation_id"> | null | undefined, sessionName?: string, maxLength = 120): string {
  if (sessionName?.trim()) return sessionName.trim();
  const firstLine = session?.opening_prompt?.split("\n").find((line) => line.trim() !== "")?.trim();
  if (firstLine) return firstLine.length > maxLength ? `${firstLine.slice(0, maxLength - 1)}…` : firstLine;
  return session?.conversation_id ?? "Session";
}

// toolCallSummary counts tool usage across turn summaries, most used first.
export function toolCallSummary(turns: Pick<SessionArchiveTurn, "tool_names">[]): Array<{ name: string; count: number }> {
  const counts = new Map<string, number>();
  for (const turn of turns) {
    for (const name of turn.tool_names ?? []) {
      const key = name || "(unnamed)";
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
  }
  return [...counts.entries()].map(([name, count]) => ({ name, count })).sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
}
