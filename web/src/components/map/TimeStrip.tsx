'use client';

import { useMemo } from 'react';
import { cn } from '@/lib/utils';
import { INTENSITY_BG, quantizeLevel } from './intensity';

export interface TimeStripDay {
  /** Local `YYYY-MM-DD` key. */
  date: string;
  /** Recorded sessions that day. */
  sessions: number;
}

export interface TimeStripBranch {
  name: string;
  /** Commits ahead of the default branch, when known. */
  aheadCount?: number;
}

export interface TimeStripProps {
  /** Sessions/day, oldest → newest (rightmost bar is "now"). */
  days: TimeStripDay[];
  /** Open branches — square chips at the right edge, beyond "now". */
  branches?: TimeStripBranch[];
  /** Default branch name, rendered as a quiet mono label. */
  defaultBranch?: string;
  /** Branch chip click → Review (the host owns navigation). */
  onBranchClick?: (branch: string) => void;
  /**
   * When true, bars become scrub targets and the playhead renders.
   * The default is `false` — the strip is fully static; nothing renders that
   * doesn't work.
   */
  scrubbable?: boolean;
  /** Index into `days` where the playhead sits (scrubbable only). */
  playheadIndex?: number;
  onScrub?: (index: number) => void;
  /** Cap on individually rendered branch chips (rest fold into "+N"). */
  maxBranchChips?: number;
  className?: string;
}

/** Bar height per intensity level — a sparkline silhouette, not a chart. */
const LEVEL_HEIGHT: Record<number, string> = {
  0: '2px',
  1: '25%',
  2: '45%',
  3: '70%',
  4: '100%',
};

/**
 * The time strip combines a session-activity sparkline (the heatmap's
 * descendant — sessions/day quantized onto the monochrome intensity ramp)
 * with open-branch chips at the right edge. The playhead is
 * purely additive behind `scrubbable`.
 */
export function TimeStrip({
  days,
  branches = [],
  defaultBranch,
  onBranchClick,
  scrubbable = false,
  playheadIndex,
  onScrub,
  maxBranchChips = 3,
  className,
}: TimeStripProps) {
  const max = useMemo(() => days.reduce((m, d) => Math.max(m, d.sessions), 0), [days]);
  const total = useMemo(() => days.reduce((s, d) => s + d.sessions, 0), [days]);

  const shownBranches = branches.slice(0, maxBranchChips);
  const overflow = branches.length - shownBranches.length;

  return (
    <section
      className={cn(
        'flex items-center gap-4 border border-rule bg-surface px-4 py-2',
        className,
      )}
      aria-label="Project timeline"
    >
      {/* Sparkline — one square bar per day on the intensity ramp. The row is
          right-anchored (justify-end) so when the days overflow the container
          the clipping eats history on the left, never the "now" edge the
          branch chips hang off. */}
      <div
        className="relative flex h-8 min-w-0 flex-1 items-end justify-end gap-[2px] overflow-hidden"
        role="img"
        aria-label={`Session activity: ${total} sessions over ${days.length} days`}
      >
        {days.map((day, i) => {
          const level = quantizeLevel(day.sessions, max);
          const bar = (
            <span
              aria-hidden
              className={cn('block w-[5px] shrink-0', INTENSITY_BG[level])}
              style={{ height: LEVEL_HEIGHT[level] }}
            />
          );
          if (!scrubbable) {
            return (
              <span
                key={day.date}
                title={`${day.date} · ${day.sessions} ${day.sessions === 1 ? 'session' : 'sessions'}`}
                className="flex h-full items-end"
              >
                {bar}
              </span>
            );
          }
          return (
            <button
              key={day.date}
              type="button"
              aria-label={`Scrub to ${day.date}`}
              title={`${day.date} · ${day.sessions} ${day.sessions === 1 ? 'session' : 'sessions'}`}
              className="flex h-full items-end focus-mono cursor-pointer"
              onClick={() => onScrub?.(i)}
            >
              {bar}
            </button>
          );
        })}
        {scrubbable && playheadIndex !== undefined && days.length > 0 && (
          <span
            aria-hidden
            data-testid="timestrip-playhead"
            className="absolute inset-y-0 w-px bg-ink"
            // Right-anchored like the bars (5px bar + 2px gap = 7px pitch):
            // the playhead stays on its bar even when history clips left.
            style={{
              right: `${(days.length - 1 - Math.min(Math.max(playheadIndex, 0), days.length - 1)) * 7 + 2}px`,
            }}
          />
        )}
      </div>

      {/* Branches — the future sits at the right edge, beyond "now". */}
      <div className="flex shrink-0 items-center gap-2">
        {defaultBranch && (
          <span className="font-mono text-[11px] text-ink-3">{defaultBranch}</span>
        )}
        {shownBranches.map((b) => (
          <button
            key={b.name}
            type="button"
            aria-label={`Review branch ${b.name}`}
            className="border border-rule px-2 py-0.5 font-mono tabular-nums text-[11px] text-ink-2 hover:bg-surface-hover focus-mono cursor-pointer"
            onClick={() => onBranchClick?.(b.name)}
          >
            {b.name}
            {b.aheadCount !== undefined && b.aheadCount > 0 ? ` +${b.aheadCount}` : ''}
          </button>
        ))}
        {overflow > 0 && (
          <span
            className="border border-rule px-2 py-0.5 font-mono tabular-nums text-[11px] text-ink-4"
            title={branches
              .slice(maxBranchChips)
              .map((b) => b.name)
              .join(', ')}
          >
            +{overflow}
          </span>
        )}
      </div>
    </section>
  );
}
