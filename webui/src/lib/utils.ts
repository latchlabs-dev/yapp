import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// Parse a date value, returning null for missing or invalid input so callers
// can render a fallback instead of crashing. `Intl.DateTimeFormat.format`
// throws "Invalid time value" on an Invalid Date, which would take down the
// whole page for a single bad/absent timestamp.
function parseDate(date: string | Date | null | undefined): Date | null {
  if (date == null) return null;
  const d = date instanceof Date ? date : new Date(date);
  return Number.isNaN(d.getTime()) ? null : d;
}

export function formatDate(date: string | Date | null | undefined): string {
  const d = parseDate(date);
  if (!d) return "—";
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(d);
}

/** Formats a date string in the browser's local timezone, date only.
 *  Use this for usage-report period_start where the underlying value
 *  is midnight UTC but should display as the user's local date. */
export function formatLocalDate(date: string | Date | null | undefined): string {
  const d = parseDate(date);
  if (!d) return "—";
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
  }).format(d);
}

export function formatDateFull(date: string | Date | null | undefined): string {
  const d = parseDate(date);
  if (!d) return "—";
  return new Intl.DateTimeFormat("en-US", {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  }).format(d);
}

export function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toLocaleString();
}

export function formatCents(cents: number): string {
  return `$${(cents / 100).toFixed(4)}`;
}

// formatUSD renders a cents value as currency, e.g. 50000 -> "$500.00".
// Pass { whole: true } to drop the decimals for round budgets, e.g. "$500".
export function formatUSD(cents: number, opts?: { whole?: boolean }): string {
  const dollars = cents / 100;
  const fractionDigits = opts?.whole && Number.isInteger(dollars) ? 0 : 2;
  return dollars.toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: 2,
  });
}

export function timeAgo(date: string | Date | null | undefined): string {
  const d = parseDate(date);
  if (!d) return "—";
  const signedSeconds = Math.floor((Date.now() - d.getTime()) / 1000);
  const future = signedSeconds < 0;
  const seconds = Math.abs(signedSeconds);
  const suffix = future ? "" : " ago";
  const prefix = future ? "in " : "";
  if (seconds < 60) return `${prefix}${seconds}s${suffix}`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${prefix}${minutes}m${suffix}`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${prefix}${hours}h${suffix}`;
  const days = Math.floor(hours / 24);
  return `${prefix}${days}d${suffix}`;
}

export function statusColor(status: string): string {
  switch (status) {
    case "active":
      return "text-success";
    case "revoked":
    case "disabled":
      return "text-destructive";
    case "pending_validation":
      return "text-warning";
    default:
      return "text-muted-foreground";
  }
}

export function statusBgColor(status: string): string {
  switch (status) {
    case "active":
      return "bg-success/15 text-success";
    case "revoked":
    case "disabled":
      return "bg-destructive/15 text-destructive";
    case "pending_validation":
      return "bg-warning/15 text-warning";
    default:
      return "bg-muted text-muted-foreground";
  }
}

// Dynamic-precision cost formatter: shows 4 decimal places for sub-cent amounts,
// 2 for everything else. Returns "$0.00" for zero / non-finite / negative inputs.
export function formatDetailCost(cents: number): string {
  if (!Number.isFinite(cents) || cents <= 0) return "$0.00";
  return `$${(cents / 100).toFixed(cents < 1 ? 4 : 2)}`;
}

// Current calendar-month window in UTC — matches the enforcer's monthly bucket
// (quota:monthly:spend_millicents:...:<YYYY-MM>), which resets on the 1st.
export function monthWindow() {
  const now = new Date();
  const start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
  const nextReset = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1));
  return {
    key: start.toISOString().slice(0, 7),
    startISO: start.toISOString(),
    reset: nextReset,
  };
}

// URL-safe base64 encoding of a session ref object, used for chat deep-links.
export function encodeSessionRef(ref: {
  conversation_id?: string;
  tenant_id?: string;
  user_id?: string;
  from: string;
  to: string;
}): string {
  return btoa(JSON.stringify(ref)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export async function copyToClipboard(text: string): Promise<void> {
  if (navigator.clipboard?.writeText && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.left = "-9999px";
  textarea.style.top = "0";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  try {
    if (!document.execCommand("copy")) throw new Error("copy command was rejected");
  } finally {
    document.body.removeChild(textarea);
  }
}
