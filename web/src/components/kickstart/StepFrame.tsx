'use client';

import type { CSSProperties, ReactNode } from 'react';
import { Button, StepIndicator } from '@/lib/ft-ui';

/** The most steps whose labels fit under the rail's markers. */
const LABELLED_RAIL_MAX = 4;

/**
 * Where a step's label sits under its marker. A label is wider than its marker,
 * so one centred on the marker spills past both sides of it: the first label
 * would leave the frame on the left and the last on the right. The first label
 * therefore starts at its marker's left edge, the last ends at its marker's
 * right edge, and the rest stay centred. The look of each place is in
 * `kickstart.css`, on the class named here.
 */
const RailLabelPlace = {
  Start: 'start',
  Middle: 'middle',
  End: 'end',
} as const;
type RailLabelPlace = (typeof RailLabelPlace)[keyof typeof RailLabelPlace];

const RAIL_LABEL_CLASS: Readonly<Record<RailLabelPlace, string>> = {
  [RailLabelPlace.Start]: 'ks-rail-label ks-rail-label-start',
  [RailLabelPlace.Middle]: 'ks-rail-label ks-rail-label-middle',
  [RailLabelPlace.End]: 'ks-rail-label ks-rail-label-end',
};

/** The place of the step at `index` among `count` steps; a lone step starts. */
function railLabelPlace(index: number, count: number): RailLabelPlace {
  if (index === 0) return RailLabelPlace.Start;
  if (index === count - 1) return RailLabelPlace.End;
  return RailLabelPlace.Middle;
}

export interface FrameStep {
  id: string;
  label: string;
}

export interface FrameAction {
  label: string;
  onClick: () => void;
  disabled?: boolean;
  loading?: boolean;
}

/**
 * The shell every stop sits in: fairtrade's step rail, a scrolling body and a
 * footer with back, "step N of M" and the stop's one primary action. The steps
 * are the stops the server lists as visible, in its order. The shell knows
 * nothing about what a stop contains.
 */
export function StepFrame({
  ariaLabel,
  steps,
  current,
  completed,
  reachable,
  onJump,
  notices,
  back,
  primary,
  children,
}: {
  ariaLabel: string;
  steps: readonly FrameStep[];
  current: string;
  completed: ReadonlySet<string>;
  reachable: ReadonlySet<string>;
  onJump: (id: string) => void;
  notices?: ReactNode;
  back: (() => void) | null;
  primary: FrameAction | null;
  children: ReactNode;
}) {
  const index = steps.findIndex((step) => step.id === current);
  // A rail of many steps cannot fit its labels under the markers, so past this
  // many it shows numbers only. Every label stays in the accessible name, and
  // the stop's own title heads the body.
  const labelled = steps.length <= LABELLED_RAIL_MAX;
  // Each label knows its place, and the rail knows how many labels share it, so
  // a label can wrap inside its share instead of crossing into its neighbour's.
  const railSteps = steps.map((step, position) => ({
    id: step.id,
    label: <span className={RAIL_LABEL_CLASS[railLabelPlace(position, steps.length)]}>{step.label}</span>,
  }));
  const railStyle = { '--ks-rail-steps': steps.length } as CSSProperties;
  return (
    <section className={labelled ? 'swz ks-wizard ks-wizard-labelled' : 'swz ks-wizard'} aria-label={ariaLabel}>
      <div className="swz-head ks-rail" style={railStyle}>
        <StepIndicator
          steps={railSteps}
          current={current}
          completed={completed as Set<string>}
          reachable={reachable as Set<string>}
          onJump={onJump}
          aria-label={`${ariaLabel} progress`}
        />
      </div>
      <div className="swz-body">
        {notices}
        {children}
      </div>
      <div className="swz-foot">
        <Button variant="ghost" onClick={back ?? undefined} disabled={back === null}>
          back
        </Button>
        <span className="swz-count">
          <span className="sr-only">
            step {index < 0 ? 0 : index + 1} of {steps.length}
          </span>
          <span aria-hidden="true">
            step <span className="tnum">{index < 0 ? 0 : index + 1}</span> / <span className="tnum">{steps.length}</span>
          </span>
        </span>
        <div className="ks-actions">
          {primary !== null && (
            <Button variant="primary" onClick={primary.onClick} disabled={primary.disabled} loading={primary.loading}>
              {primary.label}
            </Button>
          )}
        </div>
      </div>
    </section>
  );
}
