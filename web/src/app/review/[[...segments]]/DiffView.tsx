'use client';

import Link from 'next/link';
import { ArrowRight } from 'lucide-react';
import type { DecodedChangeDiffPayload } from '@/lib/api/map';
import { MINUS } from './format';

type DiffHunk = DecodedChangeDiffPayload['hunks'][number];

/** Viewer link for a hunk's attributed conversation (origin crumb back to the change). */
function hunkConversationHref(projectName: string, sessionId: string, branch: string): string {
  const params = new URLSearchParams({ origin: 'Review', originBranch: branch });
  return `/projects/${encodeURIComponent(projectName)}/${encodeURIComponent(sessionId)}?${params.toString()}`;
}

/**
 * Renders structured unified-diff hunks (from GET /review/{hash}/diff) as the
 * real before/after lines of a change, the primary mission gap ("see exactly what a
 * PR changed"). Uses the design-system `diff-*` tokens (the one place colour is
 * allowed, and only for diff blocks), and ALSO marks add/del with a +/− glyph
 * so the distinction survives in strict monochrome / for colour-blind readers.
 */
export function DiffView({
  hunks,
  binary,
  truncated,
  projectName,
  branch,
}: {
  hunks: DiffHunk[];
  binary?: boolean;
  truncated?: boolean;
  /** Used to link a hunk to the conversation that wrote it (when attributed). */
  projectName?: string;
  branch?: string;
}) {
  if (binary) {
    return (
      <p className="px-3 py-2 font-mono text-[12px] text-ink-3">
        Binary file — no line-by-line diff.
      </p>
    );
  }
  if (hunks.length === 0) {
    return (
      <p className="px-3 py-2 font-mono text-[12px] text-ink-3">
        No line changes (the file was renamed or only its mode changed).
      </p>
    );
  }
  return (
    <div className="border border-rule bg-code-bg font-mono text-[12px] overflow-x-auto">
      {hunks.map((h, hi) => {
        let oldLine = h.oldStart;
        let newLine = h.newStart;
        return (
          <div key={hi}>
            {/* Hunk header — orientation, not a content line. The right side
                carries the mission climax: which recorded conversation wrote
                this hunk's added lines (git blame → commit → session). */}
            <div className="flex items-center bg-surface-hover text-ink-4">
              <span className="w-[5.5rem] shrink-0 select-none px-2 py-0.5 text-right border-r border-rule">
                @@
              </span>
              <span className="min-w-0 flex-1 truncate px-2 py-0.5">
                {h.header ||
                  `-${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines}`}
              </span>
              {h.sessionId && projectName && branch && (
                <Link
                  href={hunkConversationHref(projectName, h.sessionId, branch)}
                  title={`Written in: ${h.sessionTitle || h.sessionId}`}
                  className="inline-flex shrink-0 items-center gap-1 px-2 py-0.5 text-[11px] text-ink-3 hover:text-ink hover:underline focus-mono cursor-pointer"
                >
                  <span className="hidden sm:inline">written in</span>
                  <span className="max-w-[14rem] truncate text-ink-2">
                    {h.sessionTitle || `${h.sessionId.slice(0, 8)}…`}
                  </span>
                  <ArrowRight size={11} aria-hidden />
                </Link>
              )}
            </div>
            {h.lines.map((l, li) => {
              const o = l.kind === 'add' ? '' : String(oldLine);
              const n = l.kind === 'del' ? '' : String(newLine);
              if (l.kind !== 'add') oldLine++;
              if (l.kind !== 'del') newLine++;
              const rowBg =
                l.kind === 'add' ? 'bg-diff-add' : l.kind === 'del' ? 'bg-diff-del' : '';
              const marker = l.kind === 'add' ? '+' : l.kind === 'del' ? MINUS : '';
              return (
                <div key={li} className={`flex ${rowBg}`}>
                  <span className="w-10 shrink-0 select-none border-r border-rule px-1 py-0.5 text-right tabular-nums text-ink-4">
                    {o}
                  </span>
                  <span className="w-10 shrink-0 select-none border-r border-rule px-1 py-0.5 text-right tabular-nums text-ink-4">
                    {n}
                  </span>
                  <span className="w-4 shrink-0 select-none py-0.5 text-center text-ink-3">
                    {marker}
                  </span>
                  <span className="whitespace-pre-wrap break-all px-1 py-0.5 text-ink">
                    {l.text || ' '}
                  </span>
                </div>
              );
            })}
          </div>
        );
      })}
      {truncated && (
        <p className="border-t border-rule px-2 py-1 text-[11px] text-ink-4">
          Diff truncated — this file is large; open it in your editor for the rest.
        </p>
      )}
    </div>
  );
}
