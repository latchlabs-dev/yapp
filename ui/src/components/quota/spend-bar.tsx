import { cn, formatUSD } from "@/lib/utils";
import { Progress } from "@/components/ui/progress";

export function pctClass(pct: number): string {
  if (pct >= 100) return "bg-destructive";
  if (pct >= 80) return "bg-warning";
  return "bg-success";
}

export function SpendBar({
  spentCents,
  budgetCents,
  compact,
}: {
  spentCents: number;
  budgetCents?: number;
  compact?: boolean;
}) {
  const hasBudget = typeof budgetCents === "number" && budgetCents > 0;
  const pct = hasBudget ? Math.min(100, (spentCents / budgetCents!) * 100) : 0;
  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline justify-between gap-2">
        <span className={cn("font-mono font-semibold tracking-tight", compact ? "text-base" : "text-lg")}>
          {formatUSD(spentCents)}
        </span>
        <span className="text-sm text-muted-foreground">
          {hasBudget ? `of ${formatUSD(budgetCents!)}` : "no budget set"}
        </span>
      </div>
      <Progress
        value={hasBudget ? pct : 0}
        className="h-2"
        indicatorClassName={hasBudget ? pctClass(pct) : undefined}
      />
      {hasBudget && (
        <div className="flex justify-between text-xs text-muted-foreground">
          <span className={cn(pct >= 100 && "text-destructive", pct >= 80 && pct < 100 && "text-warning")}>
            {pct.toFixed(pct >= 10 ? 0 : 1)}% used
          </span>
          <span>{formatUSD(Math.max(0, budgetCents! - spentCents))} left</span>
        </div>
      )}
    </div>
  );
}
