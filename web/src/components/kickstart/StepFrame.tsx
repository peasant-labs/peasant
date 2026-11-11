'use client';

import type { ReactNode } from 'react';
import { Button, StepIndicator } from '@/lib/ft-ui';

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
  return (
    <section className="swz ks-wizard" aria-label={ariaLabel}>
      <div className="swz-head">
        <StepIndicator
          steps={[...steps]}
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
