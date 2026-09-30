'use client';

import { Sparkline, StatsStrip } from '@/lib/ft-ui';
import type { RootStatsItem } from '@/lib/home/rootStats';

/**
 * The summary line above the root list: fairtrade's stats strip and, when the
 * trends topic has loaded, the sessions-per-week bars beside it. The bars
 * carry their values in their accessible name, never as their only record.
 */
export function RootStats({ items, weekly }: { items: RootStatsItem[]; weekly: number[] | null }) {
  if (items.length === 0 && weekly === null) return null;
  return (
    <section
      aria-label="general stats"
      className="mx-0 flex w-full max-w-none flex-wrap items-center justify-between gap-x-6 gap-y-3 border border-rule bg-surface px-4 py-3"
    >
      <StatsStrip items={items} label="your sessions in numbers" />
      {weekly !== null && (
        <span className="inline-flex items-center gap-2 font-mono text-sm lowercase text-ink-3">
          <span aria-hidden="true">sessions per week</span>
          <Sparkline
            type="bar"
            data={weekly}
            color="teal"
            width={96}
            height={28}
            label={`sessions per week, last ${weekly.length} weeks: ${weekly.join(', ')}`}
          />
        </span>
      )}
    </section>
  );
}
