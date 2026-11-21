'use client';

import { useEffect, useRef } from 'react';
import { Button } from '@/lib/ft-ui';

/** One answer of a button-answered stop, as served. */
export interface FootAnswer {
  /** The wire token the answer sends. */
  readonly value: string;
  /** The served label, drawn as given. */
  readonly label: string;
  /** The one primary answer of the stop, drawn rightmost. */
  readonly primary: boolean;
}

/**
 * The answers of a stop that is answered by a button, drawn in the frame's
 * foot where `continue` sits on every other stop, so a reader clicking through
 * finds them without moving the pointer. The question stays in the body and
 * names this group (`labelledBy` is its id), so a screen reader hears the
 * question with the answers.
 *
 * The primary answer is drawn last, at the bottom right, as fairtrade places
 * the one primary action of a view. While a browser sign-in is waiting, the
 * answers are disabled, the primary one shows it is busy, and `stop waiting`
 * comes first, before the answers in the tab order. Focus that was on the
 * answers when the wait began moves to `stop waiting`, and returns to the
 * primary answer when the wait ends without leaving the stop, so a keyboard
 * reader is never left on a control that went away.
 */
export function FootAnswers({
  labelledBy,
  answers,
  waiting,
  onAnswer,
  onStopWaiting,
}: {
  labelledBy: string;
  answers: readonly FootAnswer[];
  waiting: boolean;
  onAnswer: (value: string) => void;
  onStopWaiting: () => void;
}) {
  const group = useRef<HTMLDivElement>(null);
  const wasWaiting = useRef(waiting);
  const ordered = [...answers.filter((answer) => !answer.primary), ...answers.filter((answer) => answer.primary)];

  useEffect(() => {
    const before = wasWaiting.current;
    wasWaiting.current = waiting;
    const box = group.current;
    if (box === null || before === waiting) return;
    const active = document.activeElement;
    // Focus is ours to move only when it was on these controls, or was lost
    // because the control that held it was disabled or removed.
    const ours = active === null || active === document.body || box.contains(active);
    if (!ours) return;
    const target = waiting ? box.querySelector<HTMLElement>('[data-foot-answer="stop"]') : box.querySelector<HTMLElement>('[data-foot-answer="primary"]');
    target?.focus();
  }, [waiting]);

  return (
    <div className="ks-foot-answers" role="group" aria-labelledby={labelledBy} ref={group}>
      {waiting && (
        <Button variant="ghost" data-foot-answer="stop" onClick={onStopWaiting}>
          stop waiting
        </Button>
      )}
      {ordered.map((answer) => (
        <Button
          key={answer.value}
          variant={answer.primary ? 'primary' : 'secondary'}
          data-foot-answer={answer.primary ? 'primary' : 'other'}
          loading={waiting && answer.primary}
          disabled={waiting}
          onClick={() => onAnswer(answer.value)}
        >
          {answer.label}
        </Button>
      ))}
    </div>
  );
}
