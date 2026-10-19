import type { UsageReport } from "./types";

const DAY_MS = 24 * 60 * 60 * 1000;
const DEFAULT_HALF_LIFE_DAYS = 7;

export interface MonthlyCostProjection {
  monthToDateCents: number;
  projectedEndOfMonthCents: number;
  weightedDailyCents: number;
  remainingMonthDays: number;
  halfLifeDays: number;
}

function utcDayStart(date: Date): Date {
  return new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate()));
}

function utcMonthStart(date: Date): Date {
  return new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1));
}

function utcNextMonthStart(date: Date): Date {
  return new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + 1, 1));
}

function dayKey(date: Date): string {
  return date.toISOString().slice(0, 10);
}

// Projects the current month by applying exponential decay to daily MTD spend.
// Missing report days are treated as zero spend so idle days reduce the run rate.
export function calculateDecayedMonthlyCostProjection(
  reports: UsageReport[],
  now: Date = new Date(),
  halfLifeDays = DEFAULT_HALF_LIFE_DAYS,
): MonthlyCostProjection {
  const monthStart = utcMonthStart(now);
  const nextMonthStart = utcNextMonthStart(now);
  const todayStart = utcDayStart(now);
  const elapsedTodayFraction = Math.min(
    1,
    Math.max((now.getTime() - todayStart.getTime()) / DAY_MS, 1 / 24),
  );

  const byDay = new Map<string, number>();
  for (const report of reports) {
    const period = new Date(report.period_start);
    if (Number.isNaN(period.getTime())) continue;
    if (period < monthStart || period >= nextMonthStart || period > now) continue;
    const key = dayKey(utcDayStart(period));
    byDay.set(key, (byDay.get(key) ?? 0) + report.cost_total_cents);
  }

  const monthToDateCents = Array.from(byDay.values()).reduce((sum, cost) => sum + cost, 0);
  let weightedCost = 0;
  let weightTotal = 0;
  for (
    let day = new Date(monthStart);
    day.getTime() <= todayStart.getTime();
    day = new Date(day.getTime() + DAY_MS)
  ) {
    const ageDays = Math.max(0, (todayStart.getTime() - day.getTime()) / DAY_MS);
    const decayWeight = Math.exp((-Math.LN2 * ageDays) / halfLifeDays);
    const isToday = day.getTime() === todayStart.getTime();
    const observedFraction = isToday ? elapsedTodayFraction : 1;
    const cost = byDay.get(dayKey(day)) ?? 0;
    const normalizedCost = isToday ? cost / observedFraction : cost;
    const adjustedWeight = decayWeight * observedFraction;
    weightedCost += normalizedCost * adjustedWeight;
    weightTotal += adjustedWeight;
  }

  const weightedDailyCents = weightTotal > 0 ? weightedCost / weightTotal : 0;
  const remainingMonthDays = Math.max(0, (nextMonthStart.getTime() - now.getTime()) / DAY_MS);
  return {
    monthToDateCents,
    projectedEndOfMonthCents: monthToDateCents + weightedDailyCents * remainingMonthDays,
    weightedDailyCents,
    remainingMonthDays,
    halfLifeDays,
  };
}
