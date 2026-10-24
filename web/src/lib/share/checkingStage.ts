/**
 * What the publish popup says while it checks a session, before anything can
 * leave this computer: the step running now and, once a step is slow, what is
 * slow. The check is why publish waits, so the lines say it runs here.
 */
export type CheckingStep = 'preparing' | 'reindexing' | 'scanning' | 'reading';

export interface CheckingStage {
  /** The step running now, in one line. */
  stage: string;
  /** Once the step has run for SLOW_AFTER_MS: what is slow, and why publish waits. */
  note?: string;
}

/** How long one step runs before the popup says what is slow. */
export const SLOW_AFTER_MS = 10_000;

const STAGES: Record<CheckingStep, { stage: (turns: number) => string; slow: string }> = {
  preparing: {
    stage: () => 'checking for secrets and personal details before anything leaves your computer…',
    slow: 'this session is still loading on this computer.',
  },
  reindexing: {
    stage: () => 'updating this session’s index…',
    slow: 'peasant is reading this session’s recording again, on this computer. publish waits for it.',
  },
  scanning: {
    // A page that has not received the session's turns has no count to name.
    stage: (turns) => (turns > 0 ? `scanning ${turns} ${turns === 1 ? 'turn' : 'turns'}…` : 'scanning this session…'),
    slow: 'every turn is checked for secrets and personal details on this computer before anything is sent. publish waits for it.',
  },
  reading: {
    stage: () => 'reading who can read it online…',
    slow: 'peasant online is slow to answer.',
  },
};

export function checkingStage(step: CheckingStep, { turns, elapsedMs }: { turns: number; elapsedMs: number }): CheckingStage {
  const { stage, slow } = STAGES[step];
  if (elapsedMs < SLOW_AFTER_MS) return { stage: stage(turns) };
  return { stage: stage(turns), note: `taking longer than usual (${Math.floor(elapsedMs / 1000)}s): ${slow}` };
}
