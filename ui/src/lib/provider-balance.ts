import type { ProviderAccount, ProviderBalanceSnapshot, ProviderBalanceWindow } from "@/lib/types";

export function providerBillingMode(account?: Pick<ProviderAccount, "provider" | "config" | "balance">): "subscription" | "pay_as_you_go" {
  const billing = (account?.config?.billing ?? {}) as Record<string, unknown>;
  if (billing.mode === "subscription" || account?.balance?.billing_mode === "subscription" || account?.provider === "openai-codex-responses") {
    return "subscription";
  }
  return "pay_as_you_go";
}

export function providerBillingLabel(account?: Pick<ProviderAccount, "provider" | "config" | "balance">): string {
  return providerBillingMode(account) === "subscription" ? "Subscription" : "Pay as you go";
}

export function usageWindowLabel(window: ProviderBalanceWindow): string {
  if (window.name === "monthly") return "monthly";
  const seconds = Number(window.window_seconds || 0);
  if (seconds === 5 * 60 * 60) return "5h";
  if (seconds === 7 * 24 * 60 * 60) return "7d";
  if (seconds > 0 && seconds % (24 * 60 * 60) === 0) return `${seconds / (24 * 60 * 60)}d`;
  if (seconds > 0 && seconds % (60 * 60) === 0) return `${seconds / (60 * 60)}h`;
  return window.name;
}

export function formatProviderBalance(balance?: ProviderBalanceSnapshot): string {
  if (!balance) return "Not checked";
  if (balance.status === "error") return "Unavailable";
  if (!balance.balance_kind && balance.normalization_error) {
    return balance.normalization_error.startsWith("ZAI token is valid")
      ? "Valid · usage unavailable"
      : "Checked · usage unavailable";
  }
  if (balance.balance_kind === "usage") {
    const windows = balance.usage_windows ?? [];
    if (balance.unit && balance.unit !== "percent" && typeof balance.amount_remaining === "number") {
      return `${balance.amount_remaining.toLocaleString()} ${balance.unit} left`;
    }
    if (windows.length > 0) {
      return windows
        .map((window) => `${Math.max(0, window.remaining_percent).toFixed(0)}% ${usageWindowLabel(window)}`)
        .join(" · ");
    }
    return typeof balance.amount_remaining === "number" ? `${balance.amount_remaining.toFixed(0)}% left` : "—";
  }
  if (balance.balance_kind === "monetary" && typeof balance.amount_remaining === "number") {
    try {
      return new Intl.NumberFormat(undefined, {
        style: "currency",
        currency: balance.currency || "USD",
        minimumFractionDigits: 2,
      }).format(balance.amount_remaining);
    } catch {
      return `${balance.currency || ""} ${balance.amount_remaining.toFixed(2)}`.trim();
    }
  }
  return "—";
}
