import { useQuery } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { api } from "@/lib/api";
import {
  bindingScope,
  effectiveViews,
  formatCents,
  formatReset,
  hasAnyLimit,
  scopeViews,
  type PeriodView,
} from "@/lib/quota";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

/**
 * UsagePage shows the caller's own remaining spend. The endpoint takes no
 * tenant or user parameter — it reads them from the session — so this page
 * cannot be pointed at anyone else's balance.
 *
 * The three states it must keep apart are "no limit applies", "a limit applies
 * and here is what is left", and "we could not find out". Collapsing the third
 * into the first is how a user with an exhausted limit is told they have none.
 */
export function UsagePage() {
  const quota = useQuery({ queryKey: ["quota"], queryFn: () => api.quota(), retry: false });

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-4xl space-y-5 px-4 py-8 sm:px-6">
        <header>
          <h1 className="font-display text-xl font-semibold">Your usage</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Remaining spend across every limit that applies to you. The tightest one is what
            actually constrains a request.
          </p>
        </header>

        {quota.isLoading ? (
          <Skeleton className="h-40 w-full" />
        ) : quota.isError ? (
          <Card className="border-warning/40">
            <CardContent className="flex items-start gap-2.5 py-6 text-sm">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-warning" />
              <div>
                <p className="font-medium">Your balances could not be read.</p>
                <p className="mt-1 text-muted-foreground">
                  {quota.error instanceof Error ? quota.error.message : "The quota service did not answer."}{" "}
                  This does not mean no limits apply — requests are still enforced against them.
                </p>
              </div>
            </CardContent>
          </Card>
        ) : !hasAnyLimit(quota.data) ? (
          <Card>
            <CardContent className="py-10 text-center text-sm text-muted-foreground">
              No spend limits apply to your account.
            </CardContent>
          </Card>
        ) : (
          <>
            <section className="space-y-2">
              <h2 className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground/80">
                What you can spend now
              </h2>
              <div className="grid gap-3 sm:grid-cols-2">
                {effectiveViews(quota.data).map((view, i) => (
                  <Card key={view.label}>
                    <CardHeader className="flex-row items-baseline justify-between space-y-0 pb-3">
                      <CardTitle className="text-sm font-medium">{view.label}</CardTitle>
                      <CardDescription className="text-xs">
                        {view.unlimited
                          ? "No limit"
                          : `Limit: ${bindingScope(quota.data, i === 0 ? "daily" : "monthly") || "Your account"}`}
                      </CardDescription>
                    </CardHeader>
                    <CardContent>
                      <PeriodBalance view={view} prominent />
                    </CardContent>
                  </Card>
                ))}
              </div>
            </section>

            <section className="space-y-2">
              <h2 className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground/80">
                Every limit that applies
              </h2>
              <div className="grid gap-3 sm:grid-cols-2">
                {scopeViews(quota.data).map((scope) => (
                  <Card key={scope.key}>
                    <CardHeader className="pb-2">
                      <CardTitle className="text-sm font-medium">{scope.label}</CardTitle>
                      {/* Only a key's id tells the reader something: "your
                          account" and "organisation" are already unambiguous. */}
                      {scope.key.startsWith("key:") && (
                        <CardDescription className="truncate font-mono text-xs">
                          {scope.scopeId}
                        </CardDescription>
                      )}
                    </CardHeader>
                    <CardContent className="space-y-3">
                      {scope.periods.map((view) => (
                        <PeriodBalance key={view.label} view={view} showLabel />
                      ))}
                    </CardContent>
                  </Card>
                ))}
              </div>
            </section>
          </>
        )}
      </div>
    </div>
  );
}

/** PeriodBalance renders one period's remaining spend, or says it is unlimited. */
function PeriodBalance({
  view,
  showLabel = false,
  prominent = false,
}: {
  view: PeriodView;
  showLabel?: boolean;
  /** Headline form: the remaining amount large, the limit beneath it. */
  prominent?: boolean;
}) {
  if (view.unlimited) {
    return (
      <div className="flex items-baseline justify-between text-sm">
        {showLabel && <span className="text-muted-foreground">{view.label}</span>}
        <span className="text-muted-foreground">
          No limit{view.usedCents > 0 ? ` — ${formatCents(view.usedCents)} spent` : ""}
        </span>
      </div>
    );
  }

  const percent = Math.max(0, Math.min(100, view.remainingPercent ?? 0));
  if (prominent) {
    return (
      <div className="space-y-3">
        <div className="flex items-baseline gap-2">
          <span
            className={cn(
              "font-mono text-3xl font-medium tabular-nums tracking-tight",
              view.exhausted && "text-destructive",
            )}
          >
            {formatCents(view.remainingCents)}
          </span>
          <span className="text-sm text-muted-foreground">
            left of {formatCents(view.limitCents)}
          </span>
        </div>
        <Meter percent={view.exhausted ? 100 : percent} exhausted={view.exhausted} />
        <p className="text-xs text-muted-foreground">
          {view.exhausted ? "Exhausted — " : ""}
          {formatCents(view.usedCents)} spent
          {view.resetsAt ? ` · resets ${formatReset(view.resetsAt)}` : ""}
        </p>
      </div>
    );
  }
  return (
    <div className="space-y-1">
      <div className="flex items-baseline justify-between gap-2 text-sm">
        {showLabel && <span className="text-muted-foreground">{view.label}</span>}
        <span className={cn("font-mono tabular-nums", view.exhausted && "text-destructive")}>
          {formatCents(view.remainingCents)}
          <span className="text-muted-foreground"> / {formatCents(view.limitCents)}</span>
        </span>
      </div>
      <Meter percent={view.exhausted ? 100 : percent} exhausted={view.exhausted} />
      <p className="text-[11px] text-muted-foreground">
        {view.exhausted ? "Exhausted — " : ""}
        {formatCents(view.usedCents)} spent
        {view.resetsAt ? ` · resets ${formatReset(view.resetsAt)}` : ""}
      </p>
    </div>
  );
}

/** Meter shows how much of a limit remains. */
function Meter({ percent, exhausted }: { percent: number; exhausted: boolean }) {
  return (
    <div className="h-1.5 overflow-hidden rounded-full bg-muted" role="presentation">
      <div
        className={cn(
          "h-full rounded-full transition-[width] duration-500",
          exhausted ? "bg-destructive" : percent < 15 ? "bg-warning" : "bg-primary",
        )}
        style={{ width: `${percent}%` }}
      />
    </div>
  );
}
