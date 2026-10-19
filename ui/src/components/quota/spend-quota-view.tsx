import { useMemo, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import {
  quotaPolicies as qpApi,
  users as usersApi,
  usage as usageApi,
  spend as spendApi,
} from "@/lib/api";
import type { QuotaPolicy, User } from "@/lib/types";
import { RefreshCw, Wallet, Building2, UserRound, Pencil } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription,
  DialogFooter, DialogClose,
} from "@/components/ui/dialog";
import { cn, monthWindow } from "@/lib/utils";
import { SpendBar } from "@/components/quota/spend-bar";
import { toast } from "sonner";

const MONTHLY = "monthly_spend_cents";
const DAILY = "daily_spend_cents";

interface BudgetTarget {
  scopeType: "tenant" | "user";
  scopeId: string;
  label: string;
  existing?: QuotaPolicy;
}

export function SpendQuotaView({ tenantId }: { tenantId: string }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const month = useMemo(monthWindow, []);

  const policiesQ = useQuery({
    queryKey: ["quota-policies", tenantId],
    queryFn: () => qpApi.list(tenantId),
    enabled: !!tenantId,
  });

  const usersQ = useQuery({
    queryKey: ["users", tenantId],
    queryFn: () => usersApi.list(tenantId),
    enabled: !!tenantId,
  });

  // Live spend straight from the enforcement counters (Redis). This is the exact
  // state the quota enforcer checks against, auto-refreshed every 15s.
  const liveQ = useQuery({
    queryKey: ["tenant-spend", tenantId],
    queryFn: () => spendApi.tenant(tenantId),
    enabled: !!tenantId,
    refetchInterval: 15000,
    retry: false,
  });

  // Fallback: recorded cost from usage_events, when Redis/the live endpoint is
  // unavailable (e.g. an older backend, or Redis down).
  const usageQ = useQuery({
    queryKey: ["month-spend", tenantId, month.key],
    queryFn: () => usageApi.reports({ tenant_id: tenantId, granularity: "monthly", from: month.startISO }),
    enabled: !!tenantId,
    refetchInterval: 15000,
  });

  const policies = policiesQ.data?.quota_policies ?? [];
  const users = usersQ.data?.users ?? [];

  const live = liveQ.data;
  const useLive = !!live && live.redis_available && !liveQ.isError;

  const { tenantSpendCents, perUserSpend, liveTenantBudgetCents, perUserBudget } = useMemo(() => {
    const m = new Map<string, number>();
    const budgets = new Map<string, number>();
    if (useLive && live) {
      let tenant = 0;
      let tenantBudget = 0;
      for (const s of live.scopes ?? []) {
        if (s.scope_type === "tenant") {
          tenant = s.monthly_spend_cents;
          tenantBudget = s.monthly_limit_cents;
        } else if (s.scope_type === "user") {
          m.set(s.scope_id, s.monthly_spend_cents);
          budgets.set(s.scope_id, s.monthly_limit_cents);
        }
      }
      return {
        tenantSpendCents: tenant,
        perUserSpend: m,
        liveTenantBudgetCents: tenantBudget,
        perUserBudget: budgets,
      };
    }
    const reports = usageQ.data?.usage_reports ?? [];
    let tenant = 0;
    for (const r of reports) {
      tenant += r.cost_total_cents;
      if (r.user_id) m.set(r.user_id, (m.get(r.user_id) ?? 0) + r.cost_total_cents);
    }
    return {
      tenantSpendCents: tenant,
      perUserSpend: m,
      liveTenantBudgetCents: 0,
      perUserBudget: budgets,
    };
  }, [useLive, live, usageQ.data]);

  const isFetching = liveQ.isFetching || usageQ.isFetching;
  const tenantDaily = useLive ? live?.scopes?.find((s) => s.scope_type === "tenant") : undefined;

  const tenantPolicy = policies.find(
    (p) => p.enabled && p.scope_type === "tenant" && (p.scope_id === tenantId || p.scope_id === "*"),
  );
  const userPolicy = (userId: string) =>
    policies.find((p) => p.enabled && p.scope_type === "user" && (p.scope_id === userId || p.scope_id === "*"));

  const budgetOf = (p?: QuotaPolicy) => (p?.limits?.[MONTHLY] as number | undefined);
  const pctOf = (spent: number, budget?: number) => (budget && budget > 0 ? (spent / budget) * 100 : 0);
  const tenantBudgetCents = useLive ? liveTenantBudgetCents : budgetOf(tenantPolicy);
  const userRows = useMemo(() => users.map((u: User) => {
    const pol = userPolicy(u.id);
    const spentCents = perUserSpend.get(u.id) ?? 0;
    const explicitBudgetCents = budgetOf(pol);
    const budgetCents = useLive ? perUserBudget.get(u.id) : explicitBudgetCents;
    const userPct = pctOf(spentCents, budgetCents);
    const tenantPct = pctOf(spentCents, tenantBudgetCents);
    return { user: u, policy: pol, spentCents, budgetCents, explicitBudgetCents, userPct, tenantPct };
  }).sort((a, b) => {
    const utilDelta = b.userPct - a.userPct;
    if (utilDelta !== 0) return utilDelta;
    return b.tenantPct - a.tenantPct;
  }), [users, policies, perUserSpend, perUserBudget, useLive, tenantBudgetCents]);

  // --- Edit budget dialog ---
  const [target, setTarget] = useState<BudgetTarget | null>(null);
  const [monthlyDollars, setMonthlyDollars] = useState("");
  const [dailyDollars, setDailyDollars] = useState("");

  const openBudget = (t: BudgetTarget) => {
    const m = budgetOf(t.existing);
    const scope = useLive ? live?.scopes?.find((s) => s.scope_type === t.scopeType && s.scope_id === t.scopeId) : undefined;
    const d = (t.existing?.limits?.[DAILY] as number | undefined) ?? (scope?.daily_limit_cents || undefined);
    setMonthlyDollars(m != null ? String(m / 100) : "");
    setDailyDollars(d != null ? String(d / 100) : "");
    setTarget(t);
  };

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["quota-policies", tenantId] });
    qc.invalidateQueries({ queryKey: ["tenant-spend", tenantId] });
    qc.invalidateQueries({ queryKey: ["month-spend", tenantId] });
  };

  const saveBudget = useMutation({
    mutationFn: () => {
      if (!target) throw new Error("no target");
      const monthlyCents = Math.round(parseFloat(monthlyDollars) * 100);
      if (!Number.isFinite(monthlyCents) || monthlyCents <= 0) {
        throw new Error("Enter a monthly budget greater than $0");
      }
      const dailyNum = Number(dailyDollars);
      if (dailyDollars.trim() && (!Number.isFinite(dailyNum) || Math.round(dailyNum * 100) <= 0)) {
        throw new Error("Enter a daily cap of at least $0.01, or leave blank to inherit the default");
      }
      const limits: Record<string, number> = { ...(target.existing?.limits ?? {}) };
      limits[MONTHLY] = monthlyCents;
      if (Number.isFinite(dailyNum) && dailyNum > 0) limits[DAILY] = Math.round(dailyNum * 100);
      else delete limits[DAILY];

      return target.existing
        ? qpApi.update(tenantId, target.existing.id, { limits })
        : qpApi.create(tenantId, {
            scope_type: target.scopeType,
            scope_id: target.scopeId,
            priority: target.scopeType === "user" ? 20 : 10,
            enabled: true,
            limits,
          });
    },
    onSuccess: () => {
      invalidate();
      setTarget(null);
      toast.success("Budget saved");
    },
    onError: (err) => toast.error("Failed", { description: (err as Error).message }),
  });

  const refreshAll = () => {
    liveQ.refetch();
    usageQ.refetch();
    policiesQ.refetch();
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">
          Spend resets {month.reset.toLocaleDateString("en-US", { month: "long", day: "numeric" })} ·
          updates automatically ·{" "}
          <span title={useLive
            ? "Live spend read from the Redis enforcement counters"
            : "Recorded spend from usage events (live counters unavailable)"}>
            {useLive ? "live enforcement counters" : "recorded usage"}
          </span>
        </p>
        <Button size="sm" variant="outline" onClick={refreshAll} disabled={isFetching}>
          <RefreshCw className={cn("h-4 w-4", isFetching && "animate-spin")} />
          Refresh
        </Button>
      </div>

      {/* Tenant monthly budget — the headline number */}
      <Card className="card-hover">
        <CardContent className="p-6">
          <div className="flex items-start justify-between gap-4">
            <div className="flex items-center gap-3">
              <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-primary/10 text-primary ring-1 ring-inset ring-primary/15">
                <Building2 className="h-5 w-5" />
              </span>
              <div>
                <div className="text-sm font-medium">Tenant spend limits</div>
                <div className="text-xs text-muted-foreground">Tenant and user limits apply independently</div>
              </div>
            </div>
            <Button size="sm" variant="outline" onClick={() => openBudget({
              scopeType: "tenant", scopeId: tenantId, label: "tenant", existing: tenantPolicy,
            })}>
              <Pencil className="h-3.5 w-3.5" /> {budgetOf(tenantPolicy) ? "Change" : "Set budget"}
            </Button>
          </div>
          <div className="mt-5">
            <div className="mb-2 text-sm font-medium">Monthly budget</div>
            <SpendBar spentCents={tenantSpendCents} budgetCents={tenantBudgetCents} />
            {tenantDaily && (
              <div className="mt-5">
                <div className="mb-2 text-sm font-medium">Daily cap · resets at midnight UTC</div>
                <SpendBar spentCents={tenantDaily.daily_spend_cents} budgetCents={tenantDaily.daily_limit_cents} />
              </div>
            )}

          </div>
        </CardContent>
      </Card>

      {/* Per-user budgets */}
      <div>
        <div className="mb-3 flex items-center gap-2">
          <Wallet className="h-4 w-4 text-muted-foreground" />
          <h2 className="text-sm font-semibold">User budgets</h2>
          <span className="text-xs text-muted-foreground">({users.length})</span>
        </div>

        {users.length === 0 ? (
          <div className="rounded-xl border border-border/70 bg-card p-8 text-center text-sm text-muted-foreground">
            No users in this tenant yet.
          </div>
        ) : (
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {userRows.map(({ user: u, policy: pol, spentCents, budgetCents, explicitBudgetCents, userPct, tenantPct }) => {
              return (
                <Card
                  key={u.id}
                  className="card-hover cursor-pointer"
                  onClick={() => navigate(`/quotas/${tenantId}/${u.id}`)}
                >
                  <CardContent className="p-5">
                    <div className="flex items-start justify-between gap-2">
                      <div className="flex min-w-0 items-center gap-2.5">
                        <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-muted text-muted-foreground">
                          <UserRound className="h-4 w-4" />
                        </span>
                        <div className="min-w-0">
                          <div className="truncate text-sm font-medium">{u.display_name || u.email}</div>
                          <div className="truncate text-xs text-muted-foreground">{u.email}</div>
                        </div>
                      </div>
                      <Button
                        size="icon"
                        variant="ghost"
                        title={budgetOf(pol) ? "Change budget" : "Assign budget"}
                        aria-label={budgetOf(pol) ? "Change budget" : "Assign budget"}
                        onClick={(e) => { e.stopPropagation(); openBudget({ scopeType: "user", scopeId: u.id, label: u.email, existing: pol }); }}
                      >
                        <Pencil className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                    <div className="mt-4">
                      <SpendBar spentCents={spentCents} budgetCents={budgetCents} />
                    </div>
                    <div className="mt-2 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                      <span>{explicitBudgetCents ? "User limit" : "No user monthly cap"}</span>
                      <span>
                        {userPct.toFixed(userPct >= 10 ? 0 : 1)}% user · {tenantPct.toFixed(tenantPct >= 10 ? 0 : 1)}% tenant
                      </span>
                    </div>
                  </CardContent>
                </Card>
              );
            })}
          </div>
        )}
      </div>

      {/* Budget dialog */}
      <Dialog open={!!target} onOpenChange={(o) => !o && setTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {target?.existing ? "Change budget" : "Set budget"}
            </DialogTitle>
            <DialogDescription>
              {target?.scopeType === "tenant"
                ? "Monthly and daily spend limits for the whole tenant."
                : <>Monthly spend limit for <span className="font-medium text-foreground">{target?.label}</span>.</>}
              {" "}Resets on the 1st.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4 py-2">
            <div className="space-y-2">
              <label className="text-sm font-medium">Monthly budget (USD)</label>
              <div className="relative">
                <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground">$</span>
                <Input
                  type="number" min="0" step="0.01" autoFocus
                  className="pl-7 font-mono"
                  placeholder="500.00"
                  value={monthlyDollars}
                  onChange={(e) => setMonthlyDollars(e.target.value)}
                />
              </div>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium">Daily cap (USD) <span className="font-normal text-muted-foreground">— blank inherits default</span></label>
              <div className="relative">
                <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground">$</span>
                <Input
                  type="number" min="0" step="0.01"
                  className="pl-7 font-mono"
                  placeholder="Inherited default"
                  value={dailyDollars}
                  onChange={(e) => setDailyDollars(e.target.value)}
                />
              </div>
            </div>
          </div>
          <DialogFooter>
            <DialogClose asChild><Button variant="outline">Cancel</Button></DialogClose>
            <Button onClick={() => saveBudget.mutate()} disabled={saveBudget.isPending}>
              {saveBudget.isPending ? "Saving…" : "Save budget"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
