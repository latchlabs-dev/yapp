import { useMemo } from "react";
import { useParams, Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  users as usersApi,
  quotaPolicies as qpApi,
  usage as usageApi,
  spend as spendApi,
  conversationPayloads as payloadsApi,
} from "@/lib/api";
import { PageContainer } from "@/components/page";
import { useTenantNames } from "@/hooks/use-queries";
import { formatUSD, formatTokens, formatCents, timeAgo, encodeSessionRef, monthWindow } from "@/lib/utils";
import { SpendBar } from "@/components/quota/spend-bar";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { UsagePanel } from "@/components/usage-panel";
import {
  ArrowLeft, Wallet, UserRound, Gauge,
  DollarSign, Activity, MessageSquare, Cpu,
} from "lucide-react";
export function UserQuotaDetailPage() {
  const { tenantId, userId } = useParams<{ tenantId: string; userId: string }>();
  const navigate = useNavigate();
  const { tenantName } = useTenantNames();
  const currentMonth = useMemo(monthWindow, []);

  // --- Tenant & User info ---
  const usersQ = useQuery({
    queryKey: ["users", tenantId],
    queryFn: () => usersApi.list(tenantId!),
    enabled: !!tenantId,
  });

  const user = useMemo(
    () => (usersQ.data?.users ?? []).find((u) => u.id === userId),
    [usersQ.data, userId],
  );

  // --- Quota policies ---
  const policiesQ = useQuery({
    queryKey: ["quota-policies", tenantId],
    queryFn: () => qpApi.list(tenantId!),
    enabled: !!tenantId,
  });

  const policies = policiesQ.data?.quota_policies ?? [];

  // User-level policy; fall back to tenant-level if none exists.
  const userPolicy = useMemo(
    () => policies.find((p) => p.enabled && p.scope_type === "user" && (p.scope_id === userId || p.scope_id === "*")),
    [policies, userId],
  );
  const tenantPolicy = useMemo(
    () => policies.find((p) => p.enabled && p.scope_type === "tenant" && (p.scope_id === tenantId || p.scope_id === "*")),
    [policies, tenantId],
  );
  // --- Live spend (enforcement counters) ---
  const liveQ = useQuery({
    queryKey: ["tenant-spend", tenantId],
    queryFn: () => spendApi.tenant(tenantId!),
    enabled: !!tenantId,
    refetchInterval: 15000,
    retry: false,
  });

  const live = liveQ.data;
  const useLive = !!live && live.redis_available && !liveQ.isError;

  // Extract this user's spend from the per-scope response
  const userScope = useMemo(() => {
    if (!useLive || !live) return undefined;
    return (live.scopes ?? []).find((s) => s.scope_type === "user" && s.scope_id === userId);
  }, [useLive, live, userId]);

  const userMonthlySpend = userScope?.monthly_spend_cents ?? 0;
  const userDailySpend = userScope?.daily_spend_cents ?? 0;
  const userBudget = userScope?.monthly_limit_cents || undefined;
  const userDailyBudget = userScope?.daily_limit_cents || undefined;

  // --- Usage reports (last 30 days, user-scoped) ---
  const thirtyDaysAgo = useMemo(() => {
    const d = new Date();
    d.setDate(d.getDate() - 30);
    return d.toISOString();
  }, []);

  const usageQ = useQuery({
    queryKey: ["usage-reports", { tenant_id: tenantId, user_id: userId, granularity: "daily", from: thirtyDaysAgo }],
    queryFn: () => usageApi.reports({ tenant_id: tenantId, user_id: userId, granularity: "daily", from: thirtyDaysAgo }),
    enabled: !!tenantId && !!userId,
  });

  const projectionQ = useQuery({
    queryKey: ["usage-reports", {
      tenant_id: tenantId,
      user_id: userId,
      granularity: "daily",
      from: currentMonth.startISO,
      scope: "projection",
    }],
    queryFn: () => usageApi.reports({
      tenant_id: tenantId,
      user_id: userId,
      granularity: "daily",
      from: currentMonth.startISO,
    }),
    enabled: !!tenantId && !!userId,
    staleTime: 60_000,
  });

  const usageReports = usageQ.data?.usage_reports ?? [];

  // Summary stats from usage reports
  const usageSummary = useMemo(() => {
    let requests = 0, inputTokens = 0, outputTokens = 0, cost = 0;
    for (const r of usageReports) {
      requests += r.request_count;
      inputTokens += r.input_tokens;
      outputTokens += r.output_tokens;
      cost += r.cost_total_cents;
    }
    return { requests, inputTokens, outputTokens, cost };
  }, [usageReports]);

  // --- Recent conversation payloads ---
  const payloadsQ = useQuery({
    queryKey: ["conversation-payloads", { tenant_id: tenantId, user_id: userId, limit: 200 }],
    queryFn: () => payloadsApi.list({ tenant_id: tenantId, user_id: userId, limit: 200, include_prompts: true }),
    enabled: !!tenantId && !!userId,
  });

  const recentPayloads = payloadsQ.data?.conversation_payloads ?? [];

  return (
    <PageContainer>
      {/* Header */}
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="sm" className="-ml-2" onClick={() => navigate("/quotas")}>
            <ArrowLeft className="mr-1.5 h-4 w-4" />
            Back
          </Button>
          <div>
            <h1 className="flex items-center gap-2 text-2xl font-bold tracking-tight">
              <UserRound className="h-6 w-6 text-primary" />
              {user?.display_name || user?.email || "User"}
            </h1>
            <p className="mt-1 text-sm text-muted-foreground">
              <span>{tenantName(tenantId)}</span>
              {user?.email && <span className="ml-2 font-mono text-xs">{user.email}</span>}
            </p>
          </div>
        </div>
      </div>

      {/* Summary cards row */}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Wallet className="h-3 w-3" /> Monthly Spend
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-xl font-bold font-mono">{formatUSD(userMonthlySpend)}</p>
            {userBudget && (
              <p className="text-xs text-muted-foreground mt-1">
                Budget: {formatUSD(userBudget)} · {((userMonthlySpend / userBudget) * 100).toFixed(1)}%
              </p>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Activity className="h-3 w-3" /> Requests
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-xl font-bold font-mono">{usageSummary.requests.toLocaleString()}</p>
            <p className="text-xs text-muted-foreground mt-1">Last 30 days</p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Cpu className="h-3 w-3" /> Tokens
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-xl font-bold font-mono">{formatTokens(usageSummary.inputTokens + usageSummary.outputTokens)}</p>
            <p className="text-xs text-muted-foreground mt-1">
              {formatTokens(usageSummary.inputTokens)} in / {formatTokens(usageSummary.outputTokens)} out
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <DollarSign className="h-3 w-3" /> Cost
            </CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-xl font-bold font-mono">{formatCents(usageSummary.cost)}</p>
            <p className="text-xs text-muted-foreground mt-1">Last 30 days</p>
          </CardContent>
        </Card>
      </div>

      {/* Spend Budget Card */}
      <Card>
        <CardContent className="p-6">
          <div className="flex items-start justify-between gap-4">
            <div className="flex items-center gap-3">
              <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-inset ring-primary/15">
                <Gauge className="h-5 w-5" />
              </span>
              <div>
                <div className="text-sm font-medium">
                  {userPolicy ? "User monthly budget" : "Effective monthly budget"}
                </div>
                <div className="text-xs text-muted-foreground">
                  {userPolicy
                    ? "This user has their own spend limit"
                    : "No user monthly cap; tenant limits still apply"}
                </div>
              </div>
            </div>
            <Link to={`/tenants/${tenantId}?tab=quotas`}>
              <Button size="sm" variant="outline">
                <Gauge className="mr-1.5 h-3.5 w-3.5" /> Manage policies
              </Button>
            </Link>
          </div>
          <div className="mt-5">
            <SpendBar spentCents={userMonthlySpend} budgetCents={userBudget} />
          </div>
          {!userPolicy && !tenantPolicy && (
            <p className="mt-3 text-xs text-muted-foreground">
              No monthly budget policy set. Set one on the <Link to={`/tenants/${tenantId}?tab=quotas`} className="text-primary underline">tenant policies</Link> page.
            </p>
          )}
        </CardContent>
      </Card>

      {/* Daily spend line */}
      {(userDailySpend > 0 || userDailyBudget) && (
        <p className="text-xs text-muted-foreground -mt-4">
          Today's spend: <span className="font-mono font-medium text-foreground">{formatUSD(userDailySpend)}</span>
          {userDailyBudget && <> of <span className="font-mono font-medium text-foreground">{formatUSD(userDailyBudget)}</span></>}
        </p>
      )}

      {/* Active quota policies for this user */}
      {policies.some((p) => p.scope_type === "user" && p.scope_id === userId) && (
        <>
          <Separator />
          <div>
            <h2 className="mb-3 flex items-center gap-2 text-sm font-semibold">
              <Gauge className="h-4 w-4 text-muted-foreground" />
              User quota policies
            </h2>
            <div className="flex flex-wrap gap-2">
              {policies
                .filter((p) => p.scope_type === "user" && p.scope_id === userId)
                .map((p) => (
                  <Card key={p.id} className="flex-1 min-w-[200px]">
                    <CardContent className="p-4">
                      <div className="flex items-center justify-between gap-3">
                        <Badge variant={p.enabled ? "success" : "default"} className="text-[10px]">
                          {p.enabled ? "Enabled" : "Disabled"}
                        </Badge>
                        <span className="text-xs text-muted-foreground">Priority {p.priority}</span>
                      </div>
                      <div className="mt-3 space-y-1">
                        {Object.entries(p.limits ?? {}).map(([k, v]) => (
                          <div key={k} className="flex justify-between text-xs">
                            <span className="text-muted-foreground">{k.replace(/_/g, " ")}</span>
                            <span className="font-mono font-medium">
                              {k.endsWith("_spend_cents")
                                ? formatUSD(v)
                                : v.toLocaleString()}
                            </span>
                          </div>
                        ))}
                      </div>
                    </CardContent>
                  </Card>
                ))}
            </div>
          </div>
        </>
      )}

      {/* Usage Charts */}
      {usageReports.length > 0 && (
        <>
          <Separator />
          <UsagePanel
            reports={usageReports}
            projectionReports={projectionQ.data?.usage_reports ?? usageReports}
            payloads={recentPayloads}
            monthlyBudgetCents={userBudget}
          />
        </>
      )}

      {/* Recent conversation payloads (prompts) */}
      <Separator />
      <div>
        <div className="mb-3 flex items-center justify-between">
          <h2 className="flex items-center gap-2 text-sm font-semibold">
            <MessageSquare className="h-4 w-4 text-muted-foreground" />
            Recent prompts
          </h2>
          {recentPayloads.length > 0 && (
            <Link
              to="/sessions"
              className="text-xs text-primary hover:underline"
            >
              View all
            </Link>
          )}
        </div>

        {payloadsQ.isLoading ? (
          <Card>
            <CardContent className="py-8 text-center text-sm text-muted-foreground">Loading...</CardContent>
          </Card>
        ) : recentPayloads.length === 0 ? (
          <Card>
            <CardContent className="py-8 text-center text-sm text-muted-foreground">
              <MessageSquare className="mx-auto mb-2 h-8 w-8 opacity-40" />
              No conversation payloads yet. Prompts will appear once the user makes requests through the proxy.
            </CardContent>
          </Card>
        ) : (
          <div className="space-y-3">
            {recentPayloads.slice(0, 20).map((payload) => (
              <Link
                key={payload.request_id}
                to={`/sessions/captured/${encodeURIComponent(
                  encodeSessionRef({
                    conversation_id: payload.conversation_id,
                    tenant_id: payload.tenant_id,
                    user_id: payload.user_id,
                    owner_id: payload.user_id ? `user:${payload.user_id}` : payload.key_id ? `key:${payload.key_id}` : "",
                    from: payload.created_at,
                    to: payload.completed_at || payload.created_at,
                  }),
                )}`}
                className="block"
              >
                <Card className="card-hover cursor-pointer transition-colors hover:border-primary/30">
                  <CardContent className="p-4">
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2 text-xs text-muted-foreground">
                          <span className="font-mono">{timeAgo(payload.created_at)}</span>
                          <Badge variant="outline" className="font-mono text-[10px]">
                            {payload.provider}
                          </Badge>
                          {payload.model && (
                            <span className="font-mono truncate max-w-[160px]" title={payload.model}>
                              {payload.model}
                            </span>
                          )}
                          <Badge
                            variant={
                              payload.status === "success" ? "success" :
                              payload.status === "error" ? "destructive" :
                              "outline"
                            }
                            className="font-mono text-[10px]"
                          >
                            {payload.status}
                          </Badge>
                          {payload.cost_total_cents != null && payload.cost_total_cents > 0 && (
                            <span className="font-mono">{formatCents(payload.cost_total_cents)}</span>
                          )}
                        </div>
                        {payload.last_prompt_text ? (
                          <p className="mt-2 line-clamp-2 text-sm text-muted-foreground">
                            {payload.last_prompt_text}
                          </p>
                        ) : (
                          <p className="mt-2 text-sm italic text-muted-foreground/60">
                            No prompt text captured
                          </p>
                        )}
                      </div>
                    </div>
                  </CardContent>
                </Card>
              </Link>
            ))}
          </div>
        )}
      </div>
    </PageContainer>
  );
}
