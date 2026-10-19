import type { QuotaPeriod, QuotaResponse, QuotaScope } from "./types";

// Adapters between the quota API's shape and what the usage page renders.
//
// The distinction these exist to preserve is "unlimited" versus "unknown".
// A missing balance is not the same statement as "nothing constrains you", and
// rendering one as the other tells a user with an exhausted limit that they
// have none.

export interface PeriodView {
  label: string;
  unlimited: boolean;
  exhausted: boolean;
  usedCents: number;
  limitCents: number;
  remainingCents: number | null;
  /** 0–100, or null when the period is unlimited. */
  remainingPercent: number | null;
  resetsAt: string;
}

export interface ScopeView {
  key: string;
  /** "Tenant", "User", "API key" — the scope that carries the limit. */
  label: string;
  scopeId: string;
  periods: PeriodView[];
  hasLimit: boolean;
}

const SCOPE_LABELS: Record<string, string> = {
  tenant: "Organisation",
  user: "Your account",
  key: "API key",
};

export function scopeLabel(scopeType: string | undefined): string {
  if (!scopeType) return "Account";
  return SCOPE_LABELS[scopeType] ?? scopeType.charAt(0).toUpperCase() + scopeType.slice(1);
}

export function periodView(label: string, period: QuotaPeriod | undefined): PeriodView {
  // A period the server omitted is treated as unlimited only if it says so;
  // an absent object means we know nothing, which the caller renders as such.
  return {
    label,
    unlimited: period?.unlimited ?? true,
    exhausted: period?.exhausted ?? false,
    usedCents: period?.used_cents ?? 0,
    limitCents: period?.limit_cents ?? 0,
    remainingCents: period?.remaining_cents ?? null,
    remainingPercent: period?.remaining_percent ?? null,
    resetsAt: period?.resets_at ?? "",
  };
}

/** scopeViews renders every scope that constrains the caller, tightest first. */
export function scopeViews(resp: QuotaResponse | undefined | null): ScopeView[] {
  const scopes: QuotaScope[] = resp?.scopes ?? [];
  return scopes.map((scope) => {
    const periods = [periodView("Today", scope.daily), periodView("This month", scope.monthly)];
    return {
      key: `${scope.scope_type}:${scope.scope_id}`,
      label: scopeLabel(scope.scope_type),
      scopeId: scope.scope_id,
      periods,
      hasLimit: periods.some((p) => !p.unlimited),
    };
  });
}

/** effectiveViews are the binding balances: what the next request may spend. */
export function effectiveViews(resp: QuotaResponse | undefined | null): PeriodView[] {
  if (!resp) return [];
  return [periodView("Today", resp.daily), periodView("This month", resp.monthly)];
}

/** bindingScope names the scope whose limit is the tightest for a period. */
export function bindingScope(resp: QuotaResponse | undefined | null, period: "daily" | "monthly"): string {
  const effective = resp?.[period];
  if (!effective || effective.unlimited || !effective.binding_scope_type) return "";
  return scopeLabel(effective.binding_scope_type);
}

/**
 * hasAnyLimit reports whether any limit at all applies. Only a loaded response
 * can answer this; a failed load must not be rendered as "no limits".
 */
export function hasAnyLimit(resp: QuotaResponse | undefined | null): boolean {
  if (!resp) return false;
  if (!resp.daily.unlimited || !resp.monthly.unlimited) return true;
  return scopeViews(resp).some((scope) => scope.hasLimit);
}

/** formatCents renders a cent amount as dollars; the API is cents throughout. */
export function formatCents(cents: number | null | undefined): string {
  if (cents === null || cents === undefined) return "—";
  return `$${(cents / 100).toFixed(2)}`;
}

/** formatReset renders a UTC reset instant in the reader's own timezone. */
export function formatReset(iso: string): string {
  if (!iso) return "";
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return at.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}
