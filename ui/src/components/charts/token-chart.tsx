import {
  BarChart,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
  AreaChart,
  Area,
  LabelList,
  ReferenceLine,
} from "recharts";
import { useMemo } from "react";
import type { UsageReport } from "@/lib/types";
import { aggregateModels } from "@/lib/model-usage";
import { formatTokens, formatCents } from "@/lib/utils";
import { formatUsagePeriodTick, type UsageGranularity } from "@/lib/usage-range";

// ─── Layered palette ───────────────────────────────────────────────────────
const C = {
  input: "hsl(198 90% 54%)",
  output: "hsl(258 88% 68%)",
  reasoning: "hsl(326 78% 62%)",
  total: "hsl(168 70% 48%)",
  cost: "hsl(38 95% 58%)",
  requests: "hsl(231 64% 64%)",
  latencyP50: "hsl(198 90% 54%)",
  latencyP90: "hsl(326 78% 62%)",
  quota: "hsl(38 95% 58%)",
  grid: "hsl(216 20% 100% / 0.05)",
  text: "hsl(214 14% 58%)",
  textFaint: "hsl(214 12% 46%)",
  tooltipBg: "hsl(222 22% 12%)",
  tooltipBorder: "hsl(220 15% 22%)",
};

// Per-tenant palette — 8 hues that stay distinct when stacked.
const TENANT_COLORS = [
  "hsl(198 90% 54%)",
  "hsl(38 95% 58%)",
  "hsl(168 70% 48%)",
  "hsl(231 64% 64%)",
  "hsl(326 78% 62%)",
  "hsl(258 88% 68%)",
  "hsl(142 70% 48%)",
  "hsl(14 85% 60%)",
];

const FONT = "JetBrains Mono";
const tick = { fill: C.text, fontSize: 10, fontFamily: FONT } as const;
const stableTooltipProps = {
  isAnimationActive: false,
  animationDuration: 0,
  wrapperStyle: { transition: "none" },
} as const;

interface DataPoint {
  period: string;
  label: string;
  input_tokens: number;
  output_tokens: number;
  output_non_reasoning_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
  requests: number;
  cost: number;
}

function reportGranularity(report: UsageReport | undefined): UsageGranularity {
  if (report?.granularity === "hourly" || report?.granularity === "monthly") {
    return report.granularity;
  }
  return "daily";
}

function reportsSpanYears(reports: UsageReport[]): boolean {
  const years = new Set(
    reports.map((report) => new Date(report.period_start).getUTCFullYear()),
  );
  return years.size > 1;
}

// Collapse raw rows into one point per reported period, summed across tenants/models.
export function aggregateDaily(reports: UsageReport[]): DataPoint[] {
  const map = new Map<string, DataPoint>();
  const granularity = reportGranularity(reports[0]);
  const includeYear = reportsSpanYears(reports);
  for (const r of reports) {
    const k = r.period_start;
    const e = map.get(k);
    if (e) {
      e.input_tokens += r.input_tokens;
      e.output_tokens += r.output_tokens;
      e.output_non_reasoning_tokens += Math.max(r.output_tokens - r.reasoning_tokens, 0);
      e.reasoning_tokens += r.reasoning_tokens;
      e.total_tokens += r.total_tokens;
      e.requests += r.request_count;
      e.cost += r.cost_total_cents;
    } else {
      map.set(k, {
        period: k,
        label: formatUsagePeriodTick(r.period_start, granularity, includeYear),
        input_tokens: r.input_tokens,
        output_tokens: r.output_tokens,
        output_non_reasoning_tokens: Math.max(r.output_tokens - r.reasoning_tokens, 0),
        reasoning_tokens: r.reasoning_tokens,
        total_tokens: r.total_tokens,
        requests: r.request_count,
        cost: r.cost_total_cents,
      });
    }
  }
  return Array.from(map.values()).sort(
    (a, b) => new Date(a.period).getTime() - new Date(b.period).getTime(),
  );
}

// Pivot rows into per-tenant series; returns top-N tenants by total cost.
function aggregateTenants(reports: UsageReport[], getName: (id: string) => string, maxTenants = 8) {
  const byPeriodTenant = new Map<string, Map<string, number>>();
  const tenantTotals = new Map<string, number>();
  const granularity = reportGranularity(reports[0]);
  const includeYear = reportsSpanYears(reports);

  for (const r of reports) {
    const tid = r.tenant_id ?? "unknown";
    const period = r.period_start;
    if (!byPeriodTenant.has(period)) byPeriodTenant.set(period, new Map());
    const row = byPeriodTenant.get(period)!;
    row.set(tid, (row.get(tid) ?? 0) + r.cost_total_cents);
    tenantTotals.set(tid, (tenantTotals.get(tid) ?? 0) + r.cost_total_cents);
  }

  const topTenants = Array.from(tenantTotals.entries())
    .sort((a, b) => b[1] - a[1])
    .slice(0, maxTenants)
    .map(([tid, total]) => ({
      id: tid,
      label: getName(tid) || tid.slice(0, 8),
      total,
      color: TENANT_COLORS[0] ?? "",
    }))
    .map((t, i) => ({ ...t, color: TENANT_COLORS[i % TENANT_COLORS.length] ?? "" }));

  const data = Array.from(byPeriodTenant.entries())
    .sort(([a], [b]) => new Date(a).getTime() - new Date(b).getTime())
    .map(([period, row]) => {
      const point: Record<string, unknown> = {
        period,
        label: formatUsagePeriodTick(period, granularity, includeYear),
      };
      for (const t of topTenants) point[t.id] = row.get(t.id) ?? 0;
      return point;
    });

  return { data, tenants: topTenants };
}

interface LatencyPoint {
  label: string;
  period: string;
  p50: number;
  p90: number;
  count: number;
}

function aggregateLatency(
  payloads: Array<{ created_at: string; completed_at?: string }>,
): LatencyPoint[] {
  const byDay = new Map<string, number[]>();

  for (const p of payloads) {
    if (!p.completed_at) continue;
    const duration = new Date(p.completed_at).getTime() - new Date(p.created_at).getTime();
    if (duration <= 0 || duration > 300_000) continue;
    const day = p.created_at.slice(0, 10);
    if (!byDay.has(day)) byDay.set(day, []);
    byDay.get(day)!.push(duration);
  }

  return Array.from(byDay.entries())
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([day, durations]) => {
      const sorted = [...durations].sort((a, b) => a - b);
      return {
        label: new Date(day + "T12:00:00").toLocaleDateString("en-US", {
          month: "short",
          day: "numeric",
        }),
        period: day,
        p50: sorted[Math.floor(sorted.length * 0.5)] ?? 0,
        p90: sorted[Math.floor(sorted.length * 0.9)] ?? 0,
        count: sorted.length,
      };
    })
    .filter((d) => d.count >= 2);
}

function formatMs(ms: number): string {
  if (ms >= 60_000) return `${(ms / 60_000).toFixed(1)}m`;
  if (ms >= 1_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.round(ms)}ms`;
}

function formatModelTick(model: string): string {
  if (model.length <= 26) return model;
  return `${model.slice(0, 11)}...${model.slice(-12)}`;
}

// ─── Direct labeling ───────────────────────────────────────────────────────
function Key({
  items,
}: {
  items: Array<{ color: string; label: string; value: string }>;
}) {
  return (
    <div className="mb-2 flex flex-wrap items-baseline gap-x-4 gap-y-1">
      {items.map((it) => (
        <span key={it.label} className="flex items-baseline gap-1.5 text-xs">
          <span
            className="inline-block h-2 w-2 translate-y-[1px] rounded-[1px]"
            style={{ background: it.color }}
          />
          <span className="text-muted-foreground">{it.label}</span>
          <span className="tnum font-medium text-foreground">{it.value}</span>
        </span>
      ))}
    </div>
  );
}

// ─── Tooltip ───
function TooltipContent({
  active,
  payload,
  label,
  fmt,
}: {
  active?: boolean;
  payload?: Array<{ name: string; value: number; color: string }>;
  label?: string;
  fmt?: (v: number) => string;
}) {
  if (!active || !payload?.length) return null;
  return (
    <div
      className="rounded-lg border p-3 text-sm font-mono shadow-xl"
      style={{ background: C.tooltipBg, borderColor: C.tooltipBorder }}
    >
      <p className="mb-1.5 font-sans text-xs font-medium" style={{ color: C.text }}>
        {label}
      </p>
      {payload.map((e, i) => (
        <div key={i} className="flex items-center gap-2 py-0.5">
          <span className="h-2.5 w-2.5 shrink-0 rounded-full" style={{ background: e.color }} />
          <span style={{ color: C.text }}>{e.name}:</span>
          <span className="font-semibold text-foreground">
            {fmt ? fmt(e.value) : e.value.toLocaleString()}
          </span>
        </div>
      ))}
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  TokenTrendChart — Stacked bar: Input vs Output over time
// ══════════════════════════════════════════════════════════
export function TokenTrendChart({ reports }: { reports: UsageReport[] }) {
  const data = useMemo(() => aggregateDaily(reports), [reports]);
  if (!data.length) return null;

  const totIn = data.reduce((s, d) => s + d.input_tokens, 0);
  const totOut = data.reduce((s, d) => s + d.output_tokens, 0);
  const totReasoning = data.reduce((s, d) => s + d.reasoning_tokens, 0);

  return (
    <div className="w-full">
      <Key
        items={[
          { color: C.input, label: "Input", value: formatTokens(totIn) },
          { color: C.output, label: "Output", value: formatTokens(totOut) },
          { color: C.reasoning, label: "Reasoning", value: formatTokens(totReasoning) },
        ]}
      />
      <div className="h-64 w-full">
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
            <CartesianGrid stroke={C.grid} vertical={false} />
            <XAxis
              dataKey="label"
              tick={tick}
              tickLine={false}
              axisLine={false}
              interval="preserveStartEnd"
              minTickGap={24}
            />
            <YAxis
              tick={tick}
              tickLine={false}
              axisLine={false}
              width={52}
              tickFormatter={(v: number) => formatTokens(v)}
            />
            <Tooltip
              {...stableTooltipProps}
              cursor={{ fill: "hsl(216 20% 100% / 0.04)" }}
              content={<TooltipContent fmt={(v: number) => formatTokens(v)} />}
            />
            <Bar dataKey="input_tokens" name="Input" fill={C.input} stackId="t" maxBarSize={28} />
            <Bar
              dataKey="output_non_reasoning_tokens"
              name="Output"
              fill={C.output}
              stackId="t"
              maxBarSize={28}
            />
            <Bar
              dataKey="reasoning_tokens"
              name="Reasoning"
              fill={C.reasoning}
              stackId="t"
              radius={[2, 2, 0, 0]}
              maxBarSize={28}
            />
          </BarChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  CostTrendChart — Range-framed cost line + optional quota line
// ══════════════════════════════════════════════════════════
export function CostTrendChart({
  reports,
  budgetCents,
  budgetLabel = "budget pace",
}: {
  reports: UsageReport[];
  budgetCents?: number;
  budgetLabel?: string;
}) {
  const data = useMemo(() => aggregateDaily(reports), [reports]);
  if (!data.length) return null;

  const lastIdx = data.length - 1;
  const lastCost = data[lastIdx]?.cost ?? 0;

  const renderDot = (props: { cx?: number; cy?: number; index?: number }) => {
    const { cx, cy, index } = props;
    if (cx == null || cy == null || index !== lastIdx) return <g key={index} />;
    return (
      <g key="endpoint">
        <circle cx={cx} cy={cy} r={3.5} fill={C.cost} />
        <text
          x={cx - 7}
          y={cy - 7}
          textAnchor="end"
          fontFamily={FONT}
          fontSize={11}
          fontWeight={600}
          fill={C.cost}
        >
          {formatCents(lastCost)}
        </text>
      </g>
    );
  };

  return (
    <div className="h-56 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 12, right: 16, left: 0, bottom: 0 }}>
          <CartesianGrid stroke={C.grid} vertical={false} />
          <XAxis
            dataKey="label"
            tick={tick}
            tickLine={false}
            axisLine={false}
            interval="preserveStartEnd"
            minTickGap={24}
          />
          <YAxis
            tick={tick}
            tickLine={false}
            axisLine={false}
            width={56}
            domain={["dataMin", budgetCents != null ? (dm: number) => Math.max(dm, budgetCents * 1.1) : "dataMax"]}
            tickFormatter={(v: number) => `$${(v / 100).toFixed(2)}`}
          />
          <Tooltip
            {...stableTooltipProps}
            cursor={{ stroke: C.cost, strokeOpacity: 0.4, strokeWidth: 1 }}
            content={<TooltipContent fmt={(v: number) => formatCents(v)} />}
          />
          {budgetCents != null && (
            <ReferenceLine
              y={budgetCents}
              stroke={C.quota}
              strokeWidth={1.5}
              strokeDasharray="5 3"
              label={{
                value: budgetLabel,
                position: "insideTopRight",
                fontSize: 10,
                fontFamily: FONT,
                fill: C.quota,
                dy: -4,
              }}
            />
          )}
          <Area
            type="monotone"
            dataKey="cost"
            name="Cost"
            stroke={C.cost}
            strokeWidth={2}
            fill={C.cost}
            fillOpacity={0.08}
            dot={renderDot}
            activeDot={{ r: 4, fill: C.cost, strokeWidth: 0 }}
            isAnimationActive={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  RequestsTrendChart — Bar chart of request volume
// ══════════════════════════════════════════════════════════
export function RequestsTrendChart({ reports }: { reports: UsageReport[] }) {
  const data = useMemo(() => aggregateDaily(reports), [reports]);
  if (!data.length) return null;

  return (
    <div className="h-56 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
          <CartesianGrid stroke={C.grid} vertical={false} />
          <XAxis
            dataKey="label"
            tick={tick}
            tickLine={false}
            axisLine={false}
            interval="preserveStartEnd"
            minTickGap={24}
          />
          <YAxis
            tick={tick}
            tickLine={false}
            axisLine={false}
            width={48}
            tickFormatter={(v: number) => formatTokens(v)}
          />
          <Tooltip
            {...stableTooltipProps}
            cursor={{ fill: "hsl(216 20% 100% / 0.04)" }}
            content={<TooltipContent />}
          />
          <Bar
            dataKey="requests"
            name="Requests"
            fill={C.requests}
            radius={[2, 2, 0, 0]}
            maxBarSize={22}
          />
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  ModelBreakdownChart — Horizontal bar of top models
// ══════════════════════════════════════════════════════════
export function ModelBreakdownChart({ reports, onModelSelect }: { reports: UsageReport[]; onModelSelect?: (model: string) => void }) {
  const data = useMemo(() => aggregateModels(reports, "tokens", 12), [reports]);
  if (!data.length) return null;

  return (
    <div className="h-80 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <BarChart
          data={data}
          layout="vertical"
          margin={{ top: 4, right: 52, left: 4, bottom: 4 }}
        >
          <XAxis type="number" hide />
          <YAxis
            type="category"
            dataKey="model"
            tick={{ ...tick, fill: C.text }}
            tickFormatter={formatModelTick}
            tickLine={false}
            axisLine={false}
            width={156}
          />
          <Tooltip
            {...stableTooltipProps}
            cursor={{ fill: "hsl(216 20% 100% / 0.04)" }}
            content={<TooltipContent fmt={(v: number) => formatTokens(v)} />}
          />
          <Bar
            dataKey="tokens"
            name="Total Tokens"
            fill={C.total}
            radius={[0, 2, 2, 0]}
            maxBarSize={18}
            className={onModelSelect ? "cursor-pointer" : undefined}
            onClick={(entry) => onModelSelect?.(String(entry.model))}
          >
            <LabelList
              dataKey="tokens"
              position="right"
              formatter={(v: number) => formatTokens(v)}
              style={{ fill: C.textFaint, fontSize: 10, fontFamily: FONT }}
            />
          </Bar>
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  ModelCostBreakdownChart — Horizontal bar of top models by cost
// ══════════════════════════════════════════════════════════
export function ModelCostBreakdownChart({ reports, onModelSelect }: { reports: UsageReport[]; onModelSelect?: (model: string) => void }) {
  const data = useMemo(() => aggregateModels(reports, "cost", 12), [reports]);
  if (!data.length) return null;

  return (
    <div className="h-80 w-full">
      <ResponsiveContainer width="100%" height="100%">
        <BarChart
          data={data}
          layout="vertical"
          margin={{ top: 4, right: 58, left: 4, bottom: 4 }}
        >
          <XAxis type="number" hide />
          <YAxis
            type="category"
            dataKey="model"
            tick={{ ...tick, fill: C.text }}
            tickFormatter={formatModelTick}
            tickLine={false}
            axisLine={false}
            width={156}
          />
          <Tooltip
            {...stableTooltipProps}
            cursor={{ fill: "hsl(216 20% 100% / 0.04)" }}
            content={<TooltipContent fmt={(v: number) => formatCents(v)} />}
          />
          <Bar
            dataKey="cost"
            name="Cost"
            fill={C.cost}
            radius={[0, 2, 2, 0]}
            maxBarSize={18}
            className={onModelSelect ? "cursor-pointer" : undefined}
            onClick={(entry) => onModelSelect?.(String(entry.model))}
          >
            <LabelList
              dataKey="cost"
              position="right"
              formatter={(v: number) => formatCents(v)}
              style={{ fill: C.textFaint, fontSize: 10, fontFamily: FONT }}
            />
          </Bar>
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  TenantCostChart — Stacked area of cost per tenant
// ══════════════════════════════════════════════════════════
export function TenantCostChart({
  reports,
  getName,
}: {
  reports: UsageReport[];
  getName: (id: string) => string;
}) {
  const { data, tenants } = useMemo(
    () => aggregateTenants(reports, getName),
    [reports, getName],
  );
  if (!data.length || !tenants.length) return null;

  return (
    <div className="w-full">
      <Key
        items={tenants.map((t) => ({
          color: t.color,
          label: t.label,
          value: formatCents(t.total),
        }))}
      />
      <div className="h-64 w-full">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
            <CartesianGrid stroke={C.grid} vertical={false} />
            <XAxis
              dataKey="label"
              tick={tick}
              tickLine={false}
              axisLine={false}
              interval="preserveStartEnd"
              minTickGap={24}
            />
            <YAxis
              tick={tick}
              tickLine={false}
              axisLine={false}
              width={56}
              tickFormatter={(v: number) => `$${(v / 100).toFixed(2)}`}
            />
            <Tooltip
              {...stableTooltipProps}
              cursor={{ stroke: "hsl(216 20% 100% / 0.1)", strokeWidth: 1 }}
              content={<TooltipContent fmt={(v: number) => formatCents(v)} />}
            />
            {tenants.map((t) => (
              <Area
                key={t.id}
                type="monotone"
                dataKey={t.id}
                name={t.label}
                stackId="c"
                stroke={t.color}
                strokeWidth={1.5}
                fill={t.color}
                fillOpacity={0.18}
                isAnimationActive={false}
              />
            ))}
          </AreaChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}

// ══════════════════════════════════════════════════════════
//  LatencyChart — P50 / P90 request duration by day
// ══════════════════════════════════════════════════════════
export function LatencyChart({
  payloads,
}: {
  payloads: Array<{ created_at: string; completed_at?: string }>;
}) {
  const data = useMemo(() => aggregateLatency(payloads), [payloads]);
  if (!data.length) return null;

  const avgP50 = Math.round(data.reduce((s, d) => s + d.p50, 0) / data.length);
  const avgP90 = Math.round(data.reduce((s, d) => s + d.p90, 0) / data.length);

  return (
    <div className="w-full">
      <Key
        items={[
          { color: C.latencyP50, label: "P50 (median)", value: formatMs(avgP50) },
          { color: C.latencyP90, label: "P90", value: formatMs(avgP90) },
        ]}
      />
      <div className="h-56 w-full">
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={data} margin={{ top: 4, right: 8, left: 0, bottom: 0 }}>
            <CartesianGrid stroke={C.grid} vertical={false} />
            <XAxis
              dataKey="label"
              tick={tick}
              tickLine={false}
              axisLine={false}
              interval="preserveStartEnd"
              minTickGap={24}
            />
            <YAxis
              tick={tick}
              tickLine={false}
              axisLine={false}
              width={52}
              tickFormatter={(v: number) => formatMs(v)}
            />
            <Tooltip
              {...stableTooltipProps}
              cursor={{ fill: "hsl(216 20% 100% / 0.04)" }}
              content={<TooltipContent fmt={(v: number) => formatMs(v)} />}
            />
            <Bar dataKey="p50" name="P50" fill={C.latencyP50} maxBarSize={18} fillOpacity={0.7} />
            <Bar
              dataKey="p90"
              name="P90"
              fill={C.latencyP90}
              maxBarSize={18}
              radius={[2, 2, 0, 0]}
            />
          </BarChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}
