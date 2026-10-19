import type { UsageReport } from "./types.ts";

export interface ModelAggregate {
  model: string;
  inputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  tokens: number;
  requests: number;
  cost: number;
  costPerRequest: number;
  costPerMillionTokens: number;
}

export function aggregateModels(
  reports: UsageReport[],
  sortBy: "tokens" | "cost" = "tokens",
  limit?: number,
): ModelAggregate[] {
  const map = new Map<
    string,
    Omit<ModelAggregate, "costPerRequest" | "costPerMillionTokens">
  >();

  for (const r of reports) {
    const k = r.model || "unknown";
    const e = map.get(k);
    if (e) {
      e.inputTokens += r.input_tokens;
      e.outputTokens += r.output_tokens;
      e.reasoningTokens += r.reasoning_tokens;
      e.tokens += r.total_tokens;
      e.requests += r.request_count;
      e.cost += r.cost_total_cents;
      continue;
    }

    map.set(k, {
      model: k,
      inputTokens: r.input_tokens,
      outputTokens: r.output_tokens,
      reasoningTokens: r.reasoning_tokens,
      tokens: r.total_tokens,
      requests: r.request_count,
      cost: r.cost_total_cents,
    });
  }

  const rows = Array.from(map.values()).map((m) => ({
    ...m,
    costPerRequest: m.requests > 0 ? m.cost / m.requests : 0,
    costPerMillionTokens: m.tokens > 0 ? (m.cost / m.tokens) * 1_000_000 : 0,
  }));

  rows.sort((a, b) => {
    const delta = sortBy === "cost" ? b.cost - a.cost : b.tokens - a.tokens;
    return delta || b.requests - a.requests || a.model.localeCompare(b.model);
  });

  return limit == null ? rows : rows.slice(0, limit);
}
