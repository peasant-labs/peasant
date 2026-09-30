/**
 * The root page's stats strip, computed in the browser from topics the page
 * already receives. Pure functions, no React.
 *
 * Weeks are calendar weeks that start on Monday, in UTC, because the trends
 * topic names each day by its UTC date.
 */

import type { DayStats } from '@peasant-labs/schema';

/** The part of a trends day the strip reads. */
export type TrendDay = Pick<DayStats, 'date' | 'sessions'>;

const DAY_MS = 86_400_000;

/** Days since the Unix epoch of a `YYYY-MM-DD` UTC date, or null when malformed. */
function epochDay(date: string): number | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!match) return null;
  const ms = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]));
  return Number.isNaN(ms) ? null : Math.floor(ms / DAY_MS);
}

/** The Monday-start week a day falls in, as a whole number that counts weeks. */
function weekIndex(day: number): number {
  // 1970-01-01 was a Thursday, so day 4 is the first Monday. Shift so that
  // every Monday starts a new index.
  return Math.floor((day - 4) / 7);
}

function currentWeek(nowMs: number): number {
  return weekIndex(Math.floor(nowMs / DAY_MS));
}

/** Sessions per week index, from the trends topic's days. */
function sessionsByWeek(days: readonly TrendDay[]): Map<number, number> {
  const weeks = new Map<number, number>();
  for (const day of days) {
    const index = epochDay(day.date);
    if (index === null) continue;
    const week = weekIndex(index);
    weeks.set(week, (weeks.get(week) ?? 0) + day.sessions);
  }
  return weeks;
}

/** Sessions started in the current calendar week. */
export function sessionsThisWeek(days: readonly TrendDay[], nowMs: number): number {
  return sessionsByWeek(days).get(currentWeek(nowMs)) ?? 0;
}

/** Sessions per week for the last `weeks` weeks, oldest first, ending with this week. */
export function weeklySessions(days: readonly TrendDay[], nowMs: number, weeks = 8): number[] {
  const byWeek = sessionsByWeek(days);
  const last = currentWeek(nowMs);
  return Array.from({ length: weeks }, (_, offset) => byWeek.get(last - weeks + 1 + offset) ?? 0);
}

/** The longest run of consecutive weeks that each have at least one session. */
export function longestWeeklyStreak(days: readonly TrendDay[]): number {
  const active = [...sessionsByWeek(days).entries()]
    .filter(([, sessions]) => sessions > 0)
    .map(([week]) => week)
    .sort((a, b) => a - b);
  let longest = 0;
  let run = 0;
  let previous: number | null = null;
  for (const week of active) {
    run = previous !== null && week === previous + 1 ? run + 1 : 1;
    longest = Math.max(longest, run);
    previous = week;
  }
  return longest;
}

/** The median of the durations, in whole minutes, or null when there are none. */
export function medianMinutes(durations: readonly number[]): number | null {
  const values = durations.filter((value) => Number.isFinite(value) && value >= 0).sort((a, b) => a - b);
  if (values.length === 0) return null;
  const middle = Math.floor(values.length / 2);
  const median = values.length % 2 === 1 ? values[middle] : (values[middle - 1] + values[middle]) / 2;
  return Math.round(median);
}

/** A duration in minutes as `34m` or `1h 06m`. */
export function formatMinutes(minutes: number): string {
  const whole = Math.max(0, Math.round(minutes));
  if (whole < 60) return `${whole}m`;
  const hours = Math.floor(whole / 60);
  const rest = whole % 60;
  return `${hours}h ${String(rest).padStart(2, '0')}m`;
}

/** One pair of the stats strip, in fairtrade's StatsStrip item shape. */
export interface RootStatsItem {
  label: string;
  value: string | number;
  order?: 'value-first' | 'label-first';
}

/**
 * The sources of the stats strip. Each is `null` until it has loaded (or when
 * it failed), and a pair whose source is missing is left out rather than shown
 * as zero.
 */
export interface RootStatsSources {
  /** `totalSessions` of the dashboard topic. */
  dashboardSessions: number | null;
  /** How many projects GET /api/v1/projects/summary lists. */
  projects: number | null;
  /** How many sessions the publications read reports as published. */
  published: number | null;
  /** The days of the trends topic. */
  trendDays: readonly TrendDay[] | null;
  /** `durationMinutes` of each session of the quality topic. */
  qualityDurations: readonly number[] | null;
  nowMs: number;
}

/** The stats strip's pairs, in reading order. */
export function rootStatsItems(sources: RootStatsSources): RootStatsItem[] {
  const items: RootStatsItem[] = [];
  if (sources.dashboardSessions !== null) items.push({ value: sources.dashboardSessions, label: 'sessions' });
  if (sources.projects !== null) items.push({ value: sources.projects, label: 'projects' });
  if (sources.published !== null) items.push({ value: sources.published, label: 'published' });
  if (sources.trendDays !== null) {
    items.push({ value: sessionsThisWeek(sources.trendDays, sources.nowMs), label: 'this week' });
    const streak = longestWeeklyStreak(sources.trendDays);
    if (streak > 0) items.push({ label: 'longest streak', value: `${streak} wk`, order: 'label-first' });
  }
  if (sources.qualityDurations !== null) {
    const median = medianMinutes(sources.qualityDurations);
    if (median !== null) items.push({ label: 'median session', value: formatMinutes(median), order: 'label-first' });
  }
  return items;
}
