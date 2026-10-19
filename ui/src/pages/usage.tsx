import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { usage as usageApi, quotaPolicies as qpApi, conversationPayloads as payloadsApi } from "@/lib/api";
import { useTenantNames } from "@/hooks/use-queries";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { formatTokens, formatCents, monthWindow } from "@/lib/utils";
import {
  formatUsagePeriod,
  getUsageRangePreset,
  isGranularityAvailable,
  USAGE_RANGE_PRESETS,
  usageRangeWindow,
  type UsageGranularity,
  type UsageRangeKey,
} from "@/lib/usage-range";
import { PageContainer, PageHeading } from "@/components/page";
import { BarChart3, Activity, DollarSign, Cpu } from "lucide-react";
import { UsagePanel } from "@/components/usage-panel";

export function UsagePage() {
  const [rangeKey, setRangeKey] = useState<UsageRangeKey>("30d");
  const [granularity, setGranularity] = useState<UsageGranularity>("daily");
  const [rangeAnchor, setRangeAnchor] = useState(() => new Date());
  const [tenantFilter, setTenantFilter] = useState("");
  const [modelFilter, setModelFilter] = useState("");
  const { tenants: tenantList, tenantName } = useTenantNames();
  const currentMonth = useMemo(monthWindow, []);
  const selectedRange = getUsageRangePreset(rangeKey);
  const reportWindow = useMemo(
    () => usageRangeWindow(rangeKey, rangeAnchor),
    [rangeKey, rangeAnchor],
  );

  const handleRangeChange = (value: string) => {
    const nextKey = value as UsageRangeKey;
    const nextRange = getUsageRangePreset(nextKey);
    setRangeKey(nextKey);
    setGranularity(nextRange.defaultGranularity);
    setRangeAnchor(new Date());
  };

  const { data, isLoading, isFetching } = useQuery({
    queryKey: ["usage-reports", {
      granularity,
      tenant_id: tenantFilter,
      model: modelFilter,
      from: reportWindow.from,
      to: reportWindow.to,
    }],
    queryFn: () =>
      usageApi.reports({
        granularity,
        tenant_id: tenantFilter || undefined,
        model: modelFilter || undefined,
        from: reportWindow.from,
        to: reportWindow.to,
        limit: 10_000,
      }),
    placeholderData: (previousData) => previousData,
  });

  const projectionQ = useQuery({
    queryKey: ["usage-reports", {
      granularity: "daily",
      tenant_id: tenantFilter,
      model: modelFilter,
      from: currentMonth.startISO,
      scope: "projection",
    }],
    queryFn: () =>
      usageApi.reports({
        granularity: "daily",
        tenant_id: tenantFilter || undefined,
        model: modelFilter || undefined,
        from: currentMonth.startISO,
        limit: 10_000,
      }),
    staleTime: 60_000,
  });

  // Quota policies for the selected tenant — drives the dashed budget line.
  const policiesQ = useQuery({
    queryKey: ["quota-policies", tenantFilter],
    queryFn: () => qpApi.list(tenantFilter),
    enabled: !!tenantFilter,
  });

  // Recent payloads for latency chart.
  const payloadsQ = useQuery({
    queryKey: ["conversation-payloads-latency", tenantFilter, modelFilter],
    queryFn: () =>
      payloadsApi.list({
        tenant_id: tenantFilter || undefined,
        limit: 500,
      }),
    staleTime: 60_000,
  });

  const reports = data?.usage_reports ?? [];
  const projectionReports = projectionQ.data?.usage_reports ?? reports;
  const payloads = payloadsQ.data?.conversation_payloads ?? [];

  const totalReq = reports.reduce((s, r) => s + r.request_count, 0);
  const totalInput = reports.reduce((s, r) => s + r.input_tokens, 0);
  const totalOutput = reports.reduce((s, r) => s + r.output_tokens, 0);
  const totalReasoning = reports.reduce((s, r) => s + r.reasoning_tokens, 0);
  const totalCost = reports.reduce((s, r) => s + r.cost_total_cents, 0);

  // Monthly budget for the selected tenant (drives dashed line on cost chart).
  const monthlyBudgetCents = useMemo(() => {
    if (!tenantFilter) return undefined;
    const policy = (policiesQ.data?.quota_policies ?? []).find(
      (p) => p.scope_type === "tenant" && (p.scope_id === tenantFilter || p.scope_id === "*"),
    );
    return policy?.limits?.monthly_spend_cents as number | undefined;
  }, [tenantFilter, policiesQ.data]);

  return (
    <PageContainer>
      <PageHeading
        title="Usage Reports"
        description="Explore token consumption and cost from hours to years"
      />

      {/* Summary cards */}
      <div className="grid gap-4 sm:grid-cols-5">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Activity className="h-3 w-3" /> Requests
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{totalReq.toLocaleString()}</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Cpu className="h-3 w-3" /> Input Tokens
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{formatTokens(totalInput)}</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Cpu className="h-3 w-3" /> Output Tokens
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{formatTokens(totalOutput)}</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Cpu className="h-3 w-3" /> Reasoning Tokens
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{formatTokens(totalReasoning)}</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <DollarSign className="h-3 w-3" /> Total Cost
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="font-mono text-xl font-bold">{formatCents(totalCost)}</p>
          </CardContent>
        </Card>
      </div>

      {/* Filters */}
      <div className="flex flex-wrap items-end gap-4">
        <div className="space-y-1.5">
          <Label className="text-xs">Time range</Label>
          <Select value={rangeKey} onValueChange={handleRangeChange}>
            <SelectTrigger className="w-44">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {USAGE_RANGE_PRESETS.map((preset) => (
                <SelectItem key={preset.key} value={preset.key}>{preset.label}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1.5">
          <Label className="text-xs">Group by</Label>
          <Select
            value={granularity}
            onValueChange={(value) => setGranularity(value as UsageGranularity)}
          >
            <SelectTrigger className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem
                value="hourly"
                disabled={!isGranularityAvailable(rangeKey, "hourly")}
              >
                Hourly
              </SelectItem>
              <SelectItem
                value="daily"
                disabled={!isGranularityAvailable(rangeKey, "daily")}
              >
                Daily
              </SelectItem>
              <SelectItem value="monthly">Monthly</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1.5">
          <Label className="text-xs">Tenant</Label>
          <Select
            value={tenantFilter || "__all__"}
            onValueChange={(v) => setTenantFilter(v === "__all__" ? "" : v)}
          >
            <SelectTrigger className="w-56">
              <SelectValue placeholder="All tenants" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="__all__">All tenants</SelectItem>
              {tenantList.map((t) => (
                <SelectItem key={t.id} value={t.id}>{t.name}</SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1.5">
          <Label className="text-xs">Model</Label>
          <Input
            placeholder="deepseek-v4-pro"
            value={modelFilter}
            onChange={(e) => setModelFilter(e.target.value)}
            className="w-40 font-mono text-xs"
          />
        </div>
      </div>

      {/* Charts */}
      {reports.length > 0 && (
        <UsagePanel
          reports={reports}
          projectionReports={projectionReports}
          payloads={payloads}
          monthlyBudgetCents={monthlyBudgetCents}
          showTenantBreakdown={!tenantFilter}
          getName={tenantName}
          granularity={granularity}
        />
      )}

      {/* Detail table */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {selectedRange.label} detail ({reports.length} rows)
            {isFetching && !isLoading ? " · Updating…" : ""}
          </CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {/* Long ranges return hundreds of rows; scroll them in place with a
              pinned header rather than making the page tens of screens tall. */}
          <Table containerClassName="max-h-[40rem]">
            <TableHeader className="sticky top-0 z-10 bg-card">
              <TableRow>
                <TableHead>Period</TableHead>
                <TableHead>Tenant</TableHead>
                <TableHead>User</TableHead>
                <TableHead>Model</TableHead>
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">In Tokens</TableHead>
                <TableHead className="text-right">Out Tokens</TableHead>
                <TableHead className="text-right">Reasoning</TableHead>
                <TableHead className="text-right">Total</TableHead>
                <TableHead className="text-right">Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading ? (
                <TableRow>
                  <TableCell colSpan={10} className="py-8 text-center text-muted-foreground">
                    Loading...
                  </TableCell>
                </TableRow>
              ) : reports.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={10} className="py-12 text-center text-muted-foreground">
                    <BarChart3 className="mx-auto mb-2 h-8 w-8 opacity-40" />
                    No usage data found for the selected filters
                  </TableCell>
                </TableRow>
              ) : (
                reports.map((r, i) => (
                  <TableRow key={i} className="data-row">
                    <TableCell className="font-mono text-xs">
                      {formatUsagePeriod(r.period_start, usageReportGranularity(r.granularity))}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground" title={r.tenant_id || ""}>
                      {r.tenant_id ? tenantName(r.tenant_id) : "—"}
                    </TableCell>
                    <TableCell className="text-xs" title={r.user_email || r.user_id || ""}>
                      {displayUser(r)}
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline" className="font-mono text-xs">
                        {r.model || "unknown"}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm">
                      {r.request_count.toLocaleString()}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm text-muted-foreground">
                      {formatTokens(r.input_tokens)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm text-muted-foreground">
                      {formatTokens(r.output_tokens)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm text-muted-foreground">
                      {formatTokens(r.reasoning_tokens)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm font-medium">
                      {formatTokens(r.total_tokens)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-sm">
                      {formatCents(r.cost_total_cents)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </PageContainer>
  );
}

function displayUser(report: {
  user_id?: string;
  user_display_name?: string;
  user_email?: string;
}): string {
  return report.user_display_name || report.user_email || report.user_id || "Unauthenticated";
}

function usageReportGranularity(value: string): UsageGranularity {
  if (value === "hourly" || value === "monthly") return value;
  return "daily";
}
