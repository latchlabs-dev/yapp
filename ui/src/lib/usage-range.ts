export type UsageGranularity = "hourly" | "daily" | "monthly";
export type UsageRangeKey = "24h" | "7d" | "30d" | "90d" | "6mo" | "12mo" | "2y" | "5y";

export interface UsageRangePreset {
  key: UsageRangeKey;
  label: string;
  durationDays: number;
  defaultGranularity: UsageGranularity;
}

export const USAGE_RANGE_PRESETS: readonly UsageRangePreset[] = [
  { key: "24h", label: "Last 24 hours", durationDays: 1, defaultGranularity: "hourly" },
  { key: "7d", label: "Last 7 days", durationDays: 7, defaultGranularity: "daily" },
  { key: "30d", label: "Last 30 days", durationDays: 30, defaultGranularity: "daily" },
  { key: "90d", label: "Last 3 months", durationDays: 90, defaultGranularity: "daily" },
  { key: "6mo", label: "Last 6 months", durationDays: 183, defaultGranularity: "daily" },
  { key: "12mo", label: "Last 12 months", durationDays: 365, defaultGranularity: "monthly" },
  { key: "2y", label: "Last 2 years", durationDays: 730, defaultGranularity: "monthly" },
  { key: "5y", label: "Last 5 years", durationDays: 1825, defaultGranularity: "monthly" },
];

const DAY_MS = 24 * 60 * 60 * 1000;
const MAX_RANGE_DAYS: Record<UsageGranularity, number> = {
  hourly: 31,
  daily: 366,
  monthly: 5 * 366,
};

export function getUsageRangePreset(key: UsageRangeKey): UsageRangePreset {
  const preset = USAGE_RANGE_PRESETS.find((candidate) => candidate.key === key);
  if (!preset) throw new Error(`Unknown usage range: ${key}`);
  return preset;
}

export function usageRangeWindow(
  key: UsageRangeKey,
  anchor = new Date(),
): { from: string; to: string } {
  const preset = getUsageRangePreset(key);
  const to = new Date(anchor);
  const from = new Date(to.getTime() - preset.durationDays * DAY_MS);
  return { from: from.toISOString(), to: to.toISOString() };
}

export function isGranularityAvailable(
  key: UsageRangeKey,
  granularity: UsageGranularity,
): boolean {
  return getUsageRangePreset(key).durationDays <= MAX_RANGE_DAYS[granularity];
}

export function formatUsagePeriod(
  value: string | Date,
  granularity: UsageGranularity,
): string {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return "—";

  if (granularity === "monthly") {
    return new Intl.DateTimeFormat("en-US", {
      month: "short",
      year: "numeric",
      timeZone: "UTC",
    }).format(date);
  }

  if (granularity === "hourly") {
    return new Intl.DateTimeFormat("en-US", {
      month: "short",
      day: "numeric",
      year: "numeric",
      hour: "numeric",
      hour12: true,
      timeZone: "UTC",
    }).format(date);
  }

  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
    timeZone: "UTC",
  }).format(date);
}

export function formatUsagePeriodTick(
  value: string | Date,
  granularity: UsageGranularity,
  includeYear = false,
): string {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return "—";

  if (granularity === "monthly") {
    return new Intl.DateTimeFormat("en-US", {
      month: "short",
      year: "2-digit",
      timeZone: "UTC",
    }).format(date);
  }

  if (granularity === "hourly") {
    return new Intl.DateTimeFormat("en-US", {
      month: "short",
      day: "numeric",
      hour: "numeric",
      hour12: true,
      timeZone: "UTC",
    }).format(date);
  }

  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    year: includeYear ? "2-digit" : undefined,
    timeZone: "UTC",
  }).format(date);
}
