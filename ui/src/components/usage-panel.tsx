import { useMemo } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  TokenTrendChart,
  CostTrendChart,
  RequestsTrendChart,
  ModelBreakdownChart,
  ModelCostBreakdownChart,
  TenantCostChart,
  LatencyChart,
} from "@/components/charts/token-chart";
import type { UsageReport } from "@/lib/types";
import { aggregateModels } from "@/lib/model-usage";
import { calculateDecayedMonthlyCostProjection } from "@/lib/usage-projection";
import { TrendingUp, DollarSign, Activity, BarChart3, Clock, Building2 } from "lucide-react";
import { formatCents, formatTokens, formatUSD } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";

interface UsagePanelProps {
  reports: UsageReport[];
  projectionReports?: UsageReport[];
  payloads?: Array<{ created_at: string; completed_at?: string }>;
  monthlyBudgetCents?: number;
  showTenantBreakdown?: boolean;
  getName?: (id: string) => string;
  granularity?: string;
}

export function UsagePanel({
  reports,
  projectionReports,
  payloads,
  monthlyBudgetCents,
  showTenantBreakdown = false,
  getName = (id) => id,
  granularity = "daily",
}: UsagePanelProps) {
  // Budget reference line: daily pace for daily charts, total for monthly.
  const budgetForChart = useMemo(() => {
    if (monthlyBudgetCents == null) return undefined;
    return granularity === "monthly"
      ? monthlyBudgetCents
      : Math.round(monthlyBudgetCents / 30.5);
  }, [monthlyBudgetCents, granularity]);

  const budgetLabel = granularity === "monthly" ? "monthly budget" : "budget pace";
  const projection = useMemo(
    () => calculateDecayedMonthlyCostProjection(projectionReports ?? reports),
    [projectionReports, reports],
  );
  const projectedDeltaCents =
    monthlyBudgetCents == null ? undefined : projection.projectedEndOfMonthCents - monthlyBudgetCents;
  const modelCostRows = useMemo(() => aggregateModels(reports, "cost"), [reports]);

  if (!reports.length) return null;

  return (
    <div className="space-y-6">
      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <DollarSign className="h-3 w-3" /> MTD Cost
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{formatUSD(projection.monthToDateCents)}</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <TrendingUp className="h-3 w-3" /> Projected EOM
              <span className="ml-auto text-[11px] text-muted-foreground/70">
                {projection.halfLifeDays}d half-life
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex items-end justify-between gap-3">
              <p className="font-mono text-xl font-bold">{formatUSD(projection.projectedEndOfMonthCents)}</p>
              {projectedDeltaCents !== undefined && (
                <Badge variant={projectedDeltaCents > 0 ? "destructive" : "success"}>
                  {projectedDeltaCents > 0 ? "+" : ""}
                  {formatUSD(projectedDeltaCents)}
                </Badge>
              )}
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Activity className="h-3 w-3" /> Weighted Daily Pace
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{formatUSD(projection.weightedDailyCents)}</p>
            <p className="mt-1 text-xs text-muted-foreground">
              {projection.remainingMonthDays.toFixed(1)} days remaining
            </p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <TrendingUp className="h-4 w-4" />
            Token usage ({granularity})
          </CardTitle>
        </CardHeader>
        <CardContent>
          <TokenTrendChart reports={reports} />
        </CardContent>
      </Card>

      {showTenantBreakdown && (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <Building2 className="h-4 w-4" />
              Cost by tenant
            </CardTitle>
          </CardHeader>
          <CardContent>
            <TenantCostChart reports={reports} getName={getName} />
          </CardContent>
        </Card>
      )}

      <div className="grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <DollarSign className="h-4 w-4" />
              Cost trend
              {monthlyBudgetCents != null && (
                <span className="ml-auto text-xs text-muted-foreground/60">
                  {formatUSD(monthlyBudgetCents)}/mo budget
                </span>
              )}
            </CardTitle>
          </CardHeader>
          <CardContent>
            <CostTrendChart
              reports={reports}
              budgetCents={budgetForChart}
              budgetLabel={budgetLabel}
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <Activity className="h-4 w-4" />
              Request volume
            </CardTitle>
          </CardHeader>
          <CardContent>
            <RequestsTrendChart reports={reports} />
          </CardContent>
        </Card>
      </div>

      {payloads && payloads.some((p) => p.completed_at) && (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <Clock className="h-4 w-4" />
              Request latency (end-to-end)
            </CardTitle>
          </CardHeader>
          <CardContent>
            <LatencyChart payloads={payloads} />
          </CardContent>
        </Card>
      )}

      <div className="grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <DollarSign className="h-4 w-4" />
              Cost by model
            </CardTitle>
          </CardHeader>
          <CardContent>
            <ModelCostBreakdownChart reports={reports} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
              <BarChart3 className="h-4 w-4" />
              Model volume
            </CardTitle>
          </CardHeader>
          <CardContent>
            <ModelBreakdownChart reports={reports} />
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <DollarSign className="h-4 w-4" />
            Per-model cost report
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Model</TableHead>
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">Input</TableHead>
                <TableHead className="text-right">Output</TableHead>
                <TableHead className="text-right">Reasoning</TableHead>
                <TableHead className="text-right">Total Tokens</TableHead>
                <TableHead className="text-right">Cost</TableHead>
                <TableHead className="text-right">Cost / 1M</TableHead>
                <TableHead className="text-right">Cost / Req</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {modelCostRows.map((row) => (
                <TableRow key={row.model} className="data-row">
                  <TableCell>
                    <Badge variant="outline" className="max-w-[240px] truncate font-mono text-xs">
                      {row.model}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm">
                    {row.requests.toLocaleString()}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm text-muted-foreground">
                    {formatTokens(row.inputTokens)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm text-muted-foreground">
                    {formatTokens(row.outputTokens)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm text-muted-foreground">
                    {formatTokens(row.reasoningTokens)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm font-medium">
                    {formatTokens(row.tokens)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm">
                    {formatCents(row.cost)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm text-muted-foreground">
                    {formatCents(row.costPerMillionTokens)}
                  </TableCell>
                  <TableCell className="text-right font-mono text-sm text-muted-foreground">
                    {formatCents(row.costPerRequest)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
