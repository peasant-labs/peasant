'use client';

import type { ReactNode } from 'react';
// The design-system Tooltip is props-driven (content + a single focusable
// trigger) and self-contained — it manages its own hover/focus/Esc open state,
// so callers no longer mount a provider.
import { Tooltip } from '@/lib/ft-ui';
import { GLOSSARY, type GlossaryKey } from '@/lib/glossary';
import { cn } from '@/lib/utils';

/**
 * Inline glossary term: a dotted-underlined word with a plain-language
 * definition on hover AND keyboard focus. Definitions live in one place
 * (web/src/lib/glossary.ts) and follow web/DESIGN_SYSTEM.md's copy rules.
 *
 *   <Term k="commit" />                       → renders "saved update"
 *   <Term k="change">lines of work</Term>     → renders custom text, same def
 *
 * Use on the FIRST occurrence of a term per surface; later mentions plain.
 * Anything load-bearing must ALSO appear as visible copy — a tooltip is
 * reinforcement, never the only place an explanation lives.
 */
export function Term({
  k,
  children,
  className,
}: {
  k: GlossaryKey;
  children?: ReactNode;
  className?: string;
}) {
  const entry = GLOSSARY[k];
  return (
    <Tooltip
      content={
        <>
          <span className="font-medium">{entry.term}</span>
          <span className="block mt-1 text-ink-2">{entry.short}</span>
        </>
      }
    >
      <span
        tabIndex={0}
        role="button"
        aria-label={`${entry.term}: ${entry.short}`}
        className={cn(
          'border-b border-dotted border-ink-4 cursor-help focus-mono',
          className,
        )}
      >
        {children ?? entry.term}
      </span>
    </Tooltip>
  );
}
