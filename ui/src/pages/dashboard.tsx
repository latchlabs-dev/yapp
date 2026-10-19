import { useMemo } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { providers as providersApi, tenants, usage } from "@/lib/api";
import {
  Building2,
  Activity,
  DollarSign,
  Cpu,
  TrendingUp,
  BarChart3,
  Wallet,
} from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { PageContainer, PageHeading } from "@/components/page";
import { formatTokens, formatCents, formatUSD, cn, timeAgo } from "@/lib/utils";
import { formatProviderBalance, providerBillingLabel } from "@/lib/provider-balance";
import {
  TokenTrendChart,
  CostTrendChart,
  RequestsTrendChart,
  ModelBreakdownChart,
  ModelCostBreakdownChart,
  aggregateDaily,
} from "@/components/charts/token-chart";
import { Sparkline, SparkBars } from "@/components/charts/sparkline";

// A single instrument readout cell: channel label, large tabular value, a
// sub-line, and — at the right — either a word-sized sparkline of the channel's
// recent history (macro: shape) or, when there is no series, the channel icon.
// The big number answers "how much"; the sparkline answers "compared to what".
function Readout({
  label,
  value,
  subtitle,
  icon: Icon,
  series,
  spark,
  sparkColor,
}: {
  label: string;
  value: string;
  subtitle?: string;
  icon: React.ComponentType<{ className?: string }>;
  series?: number[];
  spark?: "line" | "bars";
  sparkColor?: string;
}) {
  const hasSeries = series != null && series.length > 1;
  return (
    <div className="flex items-start justify-between gap-3 bg-card px-5 py-4">
      <div className="flex flex-col gap-1.5">
        <span className="eyebrow">{label}</span>
        <span className="tnum text-[28px] font-semibold leading-none tracking-tight text-foreground">
          {value}
        </span>
        {subtitle && (
          <span className="text-xs text-muted-foreground">{subtitle}</span>
        )}
      </div>
      {hasSeries ? (
        spark === "bars" ? (
          <SparkBars
            data={series}
            color={sparkColor}
            ariaLabel={`${label} trend`}
            className="mt-1 shrink-0"
          />
        ) : (
          <Sparkline
            data={series}
            color={sparkColor}
            ariaLabel={`${label} trend`}
            className="mt-1 shrink-0"
          />
        )
      ) : (
        <Icon className="h-4 w-4 shrink-0 text-muted-foreground/50" />
      )}
    </div>
  );
}

export function DashboardPage() {
  const navigate = useNavigate();
  const openModel = (model: string) => navigate(`/models/${encodeURIComponent(model)}`);
  const { data: tenantsData } = useQuery({
    queryKey: ["tenants"],
    queryFn: () => tenants.list(),
  });

  const { data: usageData } = useQuery({
    queryKey: ["usage-reports", { granularity: "daily" }],
    queryFn: () => usage.reports({ granularity: "daily" }),
  });

  const { data: monthlyData } = useQuery({
    queryKey: ["usage-reports", { granularity: "monthly" }],
    queryFn: () => usage.reports({ granularity: "monthly" }),
  });

  const { data: providerData } = useQuery({
    queryKey: ["provider-accounts"],
    queryFn: () => providersApi.list(),
    refetchInterval: 60_000,
  });
  const providerAccounts = providerData?.provider_accounts ?? [];
  const balanceHistoryQueries = useQueries({
    queries: providerAccounts.map((account) => ({
      queryKey: ["provider-balance-history", account.id],
      queryFn: () => providersApi.balanceHistory(account.id, 96),
      refetchInterval: 60_000,
    })),
  });

  // Month-to-date spend (from the 1st), the primary business metric.
  const monthStartISO = useMemo(() => {
    const n = new Date();
    return new Date(Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), 1)).toISOString();
  }, []);
  const { data: mtdData } = useQuery({
    queryKey: ["usage-reports", { granularity: "monthly", scope: "mtd" }],
    queryFn: () => usage.reports({ granularity: "monthly", from: monthStartISO }),
    refetchInterval: 30000,
  });

  const tenantCount = tenantsData?.tenants?.length ?? 0;
  const reports = usageData?.usage_reports ?? [];

  // Month-to-date spend per tenant.
  const mtdReports = mtdData?.usage_reports ?? [];
  const mtdTotalCents = mtdReports.reduce((s, r) => s + r.cost_total_cents, 0);
  const spendByTenant = useMemo(() => {
    const m = new Map<string, number>();
    for (const r of mtdReports) {
      const id = r.tenant_id || "—";
      m.set(id, (m.get(id) ?? 0) + r.cost_total_cents);
    }
    return [...m.entries()].sort((a, b) => b[1] - a[1]);
  }, [mtdReports]);
  const maxTenantSpend = spendByTenant.reduce((mx, [, c]) => Math.max(mx, c), 0);
  const tenantName = (id: string) =>
    tenantsData?.tenants?.find((t) => t.id === id)?.name ?? id;
  const monthlyReports = monthlyData?.usage_reports ?? [];

  const totalRequests = reports.reduce((s, r) => s + r.request_count, 0);
  const totalInput = reports.reduce((s, r) => s + r.input_tokens, 0);
  const totalOutput = reports.reduce((s, r) => s + r.output_tokens, 0);
  const totalTokens = reports.reduce((s, r) => s + r.total_tokens, 0);
  const totalCost = reports.reduce((s, r) => s + r.cost_total_cents, 0);

  // One point per day, shared with the charts — drives the readout sparklines.
  const daily = useMemo(() => aggregateDaily(reports), [reports]);
  const requestsSeries = daily.map((d) => d.requests);
  const tokensSeries = daily.map((d) => d.total_tokens);
  const costSeries = daily.map((d) => d.cost);
  const activeTenants =
    tenantsData?.tenants?.filter((t) => t.status === "active").length ?? 0;

  // Token delta: compare recent half vs prior half
  const sorted = [...reports].sort((a, b) =>
    a.period_start.localeCompare(b.period_start),
  );
  const mid = Math.floor(sorted.length / 2);
  const recentPeriod = sorted.slice(mid);
  const priorPeriod = sorted.slice(0, mid);
  const recentTokens = recentPeriod.reduce((s, r) => s + r.total_tokens, 0);
  const priorTokens = priorPeriod.reduce((s, r) => s + r.total_tokens, 0);
  const tokenDelta =
    priorTokens > 0
      ? ((recentTokens - priorTokens) / priorTokens) * 100
      : null;

  return (
    <PageContainer>
      <PageHeading
        title="Overview"
        description="Live telemetry across the proxy fleet"
      />

      {/* Live telemetry — the instrument readout strip */}
      <div className="overflow-hidden rounded-xl border border-border bg-card shadow-sm shadow-black/20">
        <div className="flex items-center justify-between border-b border-border bg-secondary/40 px-5 py-2.5">
          <div className="flex items-center gap-2">
            <span className="led" />
            <span className="eyebrow text-foreground/80">Live Telemetry</span>
          </div>
          <span className="eyebrow">7-day window</span>
        </div>
        <div className="grid grid-cols-1 gap-px bg-border sm:grid-cols-2 lg:grid-cols-4">
          <Readout
            label="Active Tenants"
            value={String(activeTenants)}
            subtitle={`${tenantCount} total`}
            icon={Building2}
          />
          <Readout
            label="Requests"
            value={totalRequests.toLocaleString()}
            subtitle="across all tenants"
            icon={Activity}
            series={requestsSeries}
            spark="bars"
            sparkColor="hsl(231 64% 64%)"
          />
          <Readout
            label="Tokens"
            value={formatTokens(totalTokens)}
            subtitle={`${formatTokens(totalInput)} in / ${formatTokens(totalOutput)} out`}
            icon={Cpu}
            series={tokensSeries}
            spark="line"
            sparkColor="hsl(198 90% 54%)"
          />
          <Readout
            label="Cost"
            value={formatCents(totalCost)}
            subtitle={
              tokenDelta !== null
                ? `${tokenDelta >= 0 ? "+" : ""}${tokenDelta.toFixed(1)}% vs prior period`
                : "this period"
            }
            icon={DollarSign}
            series={costSeries}
            spark="line"
            sparkColor="hsl(38 95% 58%)"
          />
        </div>
      </div>

      {/* Provider balances — compact readouts backed by persisted polling history. */}
      {providerAccounts.length > 0 && (
        <Card>
          <CardHeader className="flex flex-row items-center justify-between pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <Wallet className="h-4 w-4" />
              Provider Availability
            </CardTitle>
            <Link to="/providers" className="text-xs text-primary hover:underline">
              Manage providers
            </Link>
          </CardHeader>
          <CardContent className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
            {providerAccounts.map((account, index) => {
              const history = balanceHistoryQueries[index]?.data?.provider_balances ?? [];
              const series = history
                .filter((snapshot) => snapshot.status === "success" && typeof snapshot.amount_remaining === "number")
                .map((snapshot) => snapshot.amount_remaining as number);
              const resetAt = account.balance?.usage_windows?.find((window) => window.resets_at)?.resets_at;
              return (
                <Link
                  key={account.id}
                  to={`/providers/${account.id}`}
                  className="flex items-center justify-between gap-3 rounded-lg border border-border bg-secondary/20 px-4 py-3 transition-colors hover:bg-secondary/40"
                >
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium">{account.name}</p>
                    <p className="text-xs text-muted-foreground">{providerBillingLabel(account)}</p>
                    <p className="mt-1 tnum text-lg font-semibold">{formatProviderBalance(account.balance)}</p>
                    <p className="text-xs text-muted-foreground">
                      {resetAt ? `Resets ${timeAgo(resetAt)}` : `${history.length} saved observation${history.length === 1 ? "" : "s"}`}
                    </p>
                  </div>
                  <Sparkline
                    data={series}
                    width={112}
                    height={36}
                    color={account.balance?.status === "error" ? "hsl(0 72% 56%)" : "hsl(198 90% 54%)"}
                    ariaLabel={`${account.name} available balance history`}
                    className="shrink-0"
                  />
                </Link>
              );
            })}
          </CardContent>
        </Card>
      )}

      {/* Month-to-date spend by tenant — the primary business metric */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between pb-3">
          <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
            <Wallet className="h-4 w-4" />
            Monthly Spend by Tenant
          </CardTitle>
          <div className="flex items-center gap-3">
            <span className="font-mono text-lg font-semibold tracking-tight">
              {formatUSD(mtdTotalCents)}
            </span>
            <Link to="/quotas" className="text-xs text-primary hover:underline">
              Manage budgets
            </Link>
          </div>
        </CardHeader>
        <CardContent>
          {spendByTenant.length === 0 ? (
            <div className="py-6 text-center text-sm text-muted-foreground">
              No spend recorded this month yet.
            </div>
          ) : (
            <div className="space-y-3">
              {spendByTenant.map(([id, cents]) => (
                <div key={id} className="space-y-1">
                  <div className="flex items-baseline justify-between gap-2 text-sm">
                    <span className="truncate font-medium">{tenantName(id)}</span>
                    <span className="font-mono">{formatUSD(cents)}</span>
                  </div>
                  <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
                    <div
                      className={cn("h-full rounded-full bg-primary transition-all")}
                      style={{ width: `${maxTenantSpend > 0 ? Math.max(2, (cents / maxTenantSpend) * 100) : 0}%` }}
                    />
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Charts row 1: Token trend + Model breakdown */}
      <div className="grid gap-6 lg:grid-cols-2">
        <Card className="lg:col-span-1">
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <TrendingUp className="h-4 w-4" />
              Token Usage Trend (7d)
            </CardTitle>
          </CardHeader>
          <CardContent>
            {reports.length > 0 ? (
              <TokenTrendChart reports={reports} />
            ) : (
              <EmptyChart message="No usage data yet" />
            )}
          </CardContent>
        </Card>
        <Card className="lg:col-span-1">
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <BarChart3 className="h-4 w-4" />
              Top Models by Volume
            </CardTitle>
          </CardHeader>
          <CardContent>
            {reports.length > 0 ? (
              <ModelBreakdownChart reports={reports} onModelSelect={openModel} />
            ) : (
              <EmptyChart message="No usage data yet" />
            )}
          </CardContent>
        </Card>
      </div>

      {/* Charts row 2: Cost + per-model cost */}
      <div className="grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <DollarSign className="h-4 w-4" />
              Cost Trend (7d)
            </CardTitle>
          </CardHeader>
          <CardContent>
            {reports.length > 0 ? (
              <CostTrendChart reports={reports} />
            ) : (
              <EmptyChart message="No cost data yet" />
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <DollarSign className="h-4 w-4" />
              Top Models by Cost
            </CardTitle>
          </CardHeader>
          <CardContent>
            {reports.length > 0 ? (
              <ModelCostBreakdownChart reports={reports} onModelSelect={openModel} />
            ) : (
              <EmptyChart message="No model cost data yet" />
            )}
          </CardContent>
        </Card>
      </div>

      {/* Charts row 3: Requests */}
      <div className="grid gap-6">
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <Activity className="h-4 w-4" />
              Request Volume (7d)
            </CardTitle>
          </CardHeader>
          <CardContent>
            {reports.length > 0 ? (
              <RequestsTrendChart reports={reports} />
            ) : (
              <EmptyChart message="No request data yet" />
            )}
          </CardContent>
        </Card>
      </div>

      {/* Monthly snapshot */}
      {monthlyReports.length > 0 && (
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
              <TrendingUp className="h-4 w-4" />
              Monthly Token Usage
            </CardTitle>
          </CardHeader>
          <CardContent>
            <TokenTrendChart reports={monthlyReports} />
          </CardContent>
        </Card>
      )}
    </PageContainer>
  );
}

function EmptyChart({ message }: { message: string }) {
  return (
    <div className="flex h-56 items-center justify-center text-muted-foreground">
      <div className="text-center">
        <BarChart3 className="mx-auto h-8 w-8 mb-2 opacity-40" />
        <p className="text-sm">{message}</p>
      </div>
    </div>
  );
}
