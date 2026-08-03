'use client';

import { useState, type ReactNode } from 'react';
import { ChevronDown, ChevronUp } from 'lucide-react';
import { cn } from '@/lib/utils';

export interface RailShellProps {
  /** Header-row label (also the bottom-sheet toggle label below `lg`). */
  title?: string;
  /**
   * Quiet eyebrow above the title (e.g. "Project" / "Code area") — names WHAT
   * this panel is, so the rail reads as a labeled mini-header, not a bare name.
   */
  eyebrow?: string;
  /** Right side of the header row (counts, a glyph — keep it quiet). */
  meta?: ReactNode;
  /** Compose with `RailSection` children; sections divide with hairlines. */
  children: ReactNode;
  className?: string;
}

/**
 * The 320px right-rail card frame for canvas surfaces in DESIGN_SYSTEM.md:
 * sticky hairline card on `lg+`, collapsing
 * to a bottom sheet below the two-column breakpoint.
 *
 * Layout contract: render as the second child of a `flex gap-5` row whose
 * first child (the canvas column) is `flex-1 min-w-0`. Children render in
 * both the rail and the sheet — avoid `id` attributes inside.
 */
export function RailShell({ title, eyebrow, meta, children, className }: RailShellProps) {
  const [sheetOpen, setSheetOpen] = useState(false);

  const header = (title || meta || eyebrow) && (
    <div className="flex items-start justify-between gap-2 border-b border-rule px-5 py-3">
      <div className="min-w-0">
        {eyebrow && <span className="v2-eyebrow block">{eyebrow}</span>}
        <span className="block truncate text-sm font-medium text-ink" title={title}>
          {title}
        </span>
      </div>
      {meta && <span className="shrink-0 pt-0.5">{meta}</span>}
    </div>
  );

  return (
    <>
      {/* Desktop rail */}
      <aside
        className={cn('hidden w-[320px] shrink-0 lg:block', className)}
        aria-label={title ?? 'Details'}
      >
        <div className="sticky top-[108px] max-h-[calc(100vh-132px)] overflow-y-auto border border-rule bg-surface">
          {header}
          <div className="divide-y divide-rule">{children}</div>
        </div>
      </aside>

      {/* Bottom sheet below the two-column breakpoint */}
      <div
        className="fixed inset-x-0 bottom-0 z-40 border-t border-rule bg-surface lg:hidden"
        aria-label={title ?? 'Details'}
      >
        <button
          type="button"
          aria-expanded={sheetOpen}
          aria-label={`${sheetOpen ? 'Collapse' : 'Expand'} ${title ?? 'details'} panel`}
          className="flex w-full items-center justify-between px-5 py-3 focus-mono cursor-pointer hover:bg-surface-hover"
          onClick={() => setSheetOpen((v) => !v)}
        >
          <span className="min-w-0 text-left">
            {eyebrow && <span className="v2-eyebrow block">{eyebrow}</span>}
            <span className="block truncate text-sm font-medium text-ink">{title ?? 'Details'}</span>
          </span>
          <span className="flex items-center gap-3">
            {meta}
            {sheetOpen ? (
              <ChevronDown size={14} aria-hidden className="text-ink-3" />
            ) : (
              <ChevronUp size={14} aria-hidden className="text-ink-3" />
            )}
          </span>
        </button>
        {sheetOpen && (
          <div className="max-h-[50vh] divide-y divide-rule overflow-y-auto border-t border-rule">
            {children}
          </div>
        )}
      </div>
    </>
  );
}

export interface RailSectionProps {
  /** Eyebrow section label (rendered with `.v2-eyebrow`). */
  label?: string;
  children: ReactNode;
  className?: string;
}

/** One eyebrow-labeled section inside a `RailShell`. */
export function RailSection({ label, children, className }: RailSectionProps) {
  return (
    <section className={cn('px-5 py-4', className)}>
      {label && <div className="v2-eyebrow mb-2">{label}</div>}
      {children}
    </section>
  );
}
