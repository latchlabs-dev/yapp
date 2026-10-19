import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { usage as usageApi } from "@/lib/api";
import type { UsageReport } from "@/lib/types";
import { DetailPageHeader, PageContainer } from "@/components/page";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { TokenTrendChart, CostTrendChart, RequestsTrendChart } from "@/components/charts/token-chart";
import { formatCents, formatTokens } from "@/lib/utils";
import { getUsageRangePreset, USAGE_RANGE_PRESETS, usageRangeWindow, type UsageRangeKey } from "@/lib/usage-range";
import { Activity, Cpu, DollarSign, FolderKanban, Users } from "lucide-react";

type RankedUsage = {
  id: string;
  name: string;
  subtitle?: string;
  requests: number;
  tokens: number;
  cost: number;
};

export function ModelDetailPage() {
  const { modelId = "" } = useParams();
  const model = decodeURIComponent(modelId);
  const [rangeKey, setRangeKey] = useState<UsageRangeKey>("90d");
  const [rangeAnchor, setRangeAnchor] = useState(() => new Date());
  const preset = getUsageRangePreset(rangeKey);
  const window = useMemo(() => usageRangeWindow(rangeKey, rangeAnchor), [rangeKey, rangeAnchor]);

  const { data, isLoading, isFetching } = useQuery({
    queryKey: ["model-usage", model, rangeKey, window.from, window.to],
    queryFn: () => usageApi.reports({
      model,
      granularity: preset.defaultGranularity,
      from: window.from,
      to: window.to,
      limit: 10_000,
    }),
    placeholderData: (previous) => previous,
  });

  const reports = data?.usage_reports ?? [];
  const totals = summarize(reports);
  const projects = useMemo(() => rankUsage(reports, "project"), [reports]);
  const users = useMemo(() => rankUsage(reports, "user"), [reports]);

  const changeRange = (value: string) => {
    setRangeKey(value as UsageRangeKey);
    setRangeAnchor(new Date());
  };

  return (
    <PageContainer>
      <DetailPageHeader title={model || "Unknown model"} backTo="/" backLabel="Back to overview">
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">Period</span>
          <Select value={rangeKey} onValueChange={changeRange}>
            <SelectTrigger className="w-44"><SelectValue /></SelectTrigger>
            <SelectContent>
              {USAGE_RANGE_PRESETS.map((range) => (
                <SelectItem key={range.key} value={range.key}>{range.label}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </DetailPageHeader>

      <p className="text-sm text-muted-foreground">
        Long-term usage, cost, projects, and users for this model
        {isFetching && !isLoading ? " · Updating…" : ""}
      </p>

      <div className="grid gap-4 sm:grid-cols-3">
        <MetricCard icon={Activity} label="Requests" value={totals.requests.toLocaleString()} />
        <MetricCard icon={Cpu} label="Tokens" value={formatTokens(totals.tokens)} />
        <MetricCard icon={DollarSign} label="Cost" value={formatCents(totals.cost)} />
      </div>

      {isLoading ? (
        <Card><CardContent className="py-16 text-center text-sm text-muted-foreground">Loading model analytics…</CardContent></Card>
      ) : reports.length === 0 ? (
        <Card><CardContent className="py-16 text-center text-sm text-muted-foreground">No usage found for this model and period.</CardContent></Card>
      ) : (
        <>
          <Card>
            <CardHeader><CardTitle className="text-sm text-muted-foreground">Token trend</CardTitle></CardHeader>
            <CardContent><TokenTrendChart reports={reports} /></CardContent>
          </Card>
          <div className="grid gap-6 lg:grid-cols-2">
            <Card>
              <CardHeader><CardTitle className="text-sm text-muted-foreground">Cost trend</CardTitle></CardHeader>
              <CardContent><CostTrendChart reports={reports} /></CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle className="text-sm text-muted-foreground">Request trend</CardTitle></CardHeader>
              <CardContent><RequestsTrendChart reports={reports} /></CardContent>
            </Card>
          </div>
          <div className="grid gap-6 xl:grid-cols-2">
            <RankingCard icon={FolderKanban} title="Major projects" rows={projects} empty="No project attribution in this period" />
            <RankingCard icon={Users} title="Major users" rows={users} empty="No authenticated users in this period" />
          </div>
        </>
      )}
    </PageContainer>
  );
}

function MetricCard({ icon: Icon, label, value }: { icon: typeof Activity; label: string; value: string }) {
  return <Card><CardHeader className="pb-2"><CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground"><Icon className="h-3 w-3" />{label}</CardTitle></CardHeader><CardContent><p className="font-mono text-xl font-bold">{value}</p></CardContent></Card>;
}

function summarize(reports: UsageReport[]) {
  return reports.reduce((total, report) => ({
    requests: total.requests + report.request_count,
    tokens: total.tokens + report.total_tokens,
    cost: total.cost + report.cost_total_cents,
  }), { requests: 0, tokens: 0, cost: 0 });
}

function rankUsage(reports: UsageReport[], by: "project" | "user"): RankedUsage[] {
  const rows = new Map<string, RankedUsage>();
  for (const report of reports) {
    const id = by === "project" ? report.project_id : report.user_id;
    if (!id) continue;
    const name = by === "project"
      ? report.project_name || id
      : report.user_display_name || report.user_email || id;
    const subtitle = by === "project" ? undefined : report.user_email && report.user_email !== name ? report.user_email : undefined;
    const existing = rows.get(id) ?? { id, name, subtitle, requests: 0, tokens: 0, cost: 0 };
    existing.requests += report.request_count;
    existing.tokens += report.total_tokens;
    existing.cost += report.cost_total_cents;
    rows.set(id, existing);
  }
  return [...rows.values()].sort((a, b) => b.tokens - a.tokens || b.requests - a.requests).slice(0, 12);
}

function RankingCard({ icon: Icon, title, rows, empty }: { icon: typeof Users; title: string; rows: RankedUsage[]; empty: string }) {
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2 text-sm text-muted-foreground"><Icon className="h-4 w-4" />{title}</CardTitle></CardHeader>
      <CardContent className="p-0">
        <Table>
          <TableHeader><TableRow><TableHead>Name</TableHead><TableHead className="text-right">Requests</TableHead><TableHead className="text-right">Tokens</TableHead><TableHead className="text-right">Cost</TableHead></TableRow></TableHeader>
          <TableBody>
            {rows.length === 0 ? <TableRow><TableCell colSpan={4} className="py-8 text-center text-muted-foreground">{empty}</TableCell></TableRow> : rows.map((row) => (
              <TableRow key={row.id} className="data-row">
                <TableCell><div className="max-w-[240px] truncate font-medium" title={row.name}>{row.name}</div>{row.subtitle && <div className="max-w-[240px] truncate text-xs text-muted-foreground">{row.subtitle}</div>}</TableCell>
                <TableCell className="text-right font-mono">{row.requests.toLocaleString()}</TableCell>
                <TableCell className="text-right font-mono">{formatTokens(row.tokens)}</TableCell>
                <TableCell className="text-right font-mono">{formatCents(row.cost)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
