import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  formatMinutes,
  longestWeeklyStreak,
  medianMinutes,
  sessionsThisWeek,
  weeklySessions,
} from './rootStats';
import {
  parseStrictYAML,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

const REQUIRED_NAMES = [
  'weeks-start-on-monday',
  'no-sessions-this-week',
  'empty-topics',
  'rounds-to-whole-minutes',
  'malformed-date-skipped',
] as const;

interface StatsCase {
  name: string;
  now: string;
  days: { date: string; sessions: number }[];
  durations: number[];
  expected: { thisWeek: number; weekly: number[]; streak: number; median: number | null; medianLabel: string | null };
}

function loadCases(): StatsCase[] {
  const source = readFileSync(resolve(process.cwd(), 'src/lib/home/testdata/root-stats.yaml'), 'utf8');
  const root = requireRecord(parseStrictYAML(source, 'root stats fixture'), 'root stats fixture');
  requireExactRequiredFields(root, ['requiredNames', 'cases'], 'root stats fixture');
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== [...REQUIRED_NAMES].sort().join()) {
    throw new Error(`root stats fixture requiredNames must be exactly ${REQUIRED_NAMES.join(', ')}`);
  }
  if (!Array.isArray(root.cases)) throw new Error('root stats fixture cases must be a list');
  const cases = root.cases.map((value, index) => requireRecord(value, `root stats fixture.cases[${index}]`));
  requireUniqueNames(cases, 'root stats fixture.cases');
  cases.forEach((testCase, index) => {
    const where = `root stats fixture.cases[${index}]`;
    requireExactRequiredFields(testCase, ['name', 'now', 'days', 'durations', 'expected'], where);
    requireExactRequiredFields(
      requireRecord(testCase.expected, `${where}.expected`),
      ['thisWeek', 'weekly', 'streak', 'median', 'medianLabel'],
      `${where}.expected`,
    );
    if (!Array.isArray(testCase.days) || !Array.isArray(testCase.durations)) throw new Error(`${where} days and durations must be lists`);
    testCase.days.forEach((day, dayIndex) =>
      requireExactRequiredFields(requireRecord(day, `${where}.days[${dayIndex}]`), ['date', 'sessions'], `${where}.days[${dayIndex}]`),
    );
  });
  const names = cases.map((testCase) => testCase.name);
  const missing = REQUIRED_NAMES.filter((name) => !names.includes(name));
  const unknown = names.filter((name) => !(REQUIRED_NAMES as readonly unknown[]).includes(name));
  if (missing.length || unknown.length) {
    throw new Error(`root stats fixture must hold exactly the required cases; missing ${missing.join(', ') || 'none'}, unknown ${unknown.join(', ') || 'none'}`);
  }
  return cases as unknown as StatsCase[];
}

describe('root stats strip computations', () => {
  for (const testCase of loadCases()) {
    it(testCase.name, () => {
      const nowMs = Date.parse(testCase.now);
      expect(sessionsThisWeek(testCase.days, nowMs)).toBe(testCase.expected.thisWeek);
      expect(weeklySessions(testCase.days, nowMs)).toEqual(testCase.expected.weekly);
      expect(longestWeeklyStreak(testCase.days)).toBe(testCase.expected.streak);
      const median = medianMinutes(testCase.durations);
      expect(median).toBe(testCase.expected.median);
      expect(median === null ? null : formatMinutes(median)).toBe(testCase.expected.medianLabel);
    });
  }
});
