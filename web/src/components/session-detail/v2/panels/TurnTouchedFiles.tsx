'use client';

import { cn } from '@/lib/utils';
import { pathsMatch, type TurnFileTouches } from '../lib/scopeTurns';

interface TurnTouchedFilesProps {
  /**
   * This turn's file touches (from `collectFileTouches`, which relativizes
   * raw wire paths to repo-relative paths); caller skips empty.
   */
  touches: TurnFileTouches;
  /** The file-scope path, when active — its row is marked current (bold, aria-current). */
  activeFile?: string;
  className?: string;
}

/**
 * Per-turn touched-files panel: the turn's tool-call file touches as
 * font-mono rows, edits (attribution) visually distinct from reads (context).
 * Each path is plain text: the code map is a route-only section, so the panel
 * does not link into it. Mounted by the SessionDetailV2 adapter through the
 * package's `renderTurnPanel` slot while a scope is active, so the files sit
 * inside the turn card they belong to.
 */
export function TurnTouchedFiles({
  touches,
  activeFile,
  className,
}: TurnTouchedFilesProps) {
  return (
    <div
      aria-label={`Files touched in turn ${touches.turnIndex}`}
      className={cn('flex flex-wrap gap-x-10 gap-y-2', className)}
    >
      {touches.edits.length > 0 && (
        <FileGroup
          label="files changed"
          files={touches.edits}
          kind="edit"
          activeFile={activeFile}
        />
      )}
      {touches.reads.length > 0 && (
        <FileGroup
          label="files read"
          files={touches.reads}
          kind="read"
          activeFile={activeFile}
        />
      )}
    </div>
  );
}

function FileGroup({
  label,
  files,
  kind,
  activeFile,
}: {
  label: string;
  files: string[];
  kind: 'edit' | 'read';
  activeFile?: string;
}) {
  const isActive = (filePath: string) => !!activeFile && pathsMatch(activeFile, filePath);
  return (
    <div className="min-w-0">
      <p className="v2-eyebrow">{label}</p>
      <ul className="mt-0.5 flex flex-col">
        {files.map((filePath) => (
          <li
            key={filePath}
            aria-current={isActive(filePath) ? 'true' : undefined}
            className={cn(
              'break-all font-mono text-[12px] leading-5',
              kind === 'read' ? 'text-ink-3' : 'text-ink',
              isActive(filePath) && 'font-semibold text-ink',
            )}
          >
            {filePath}
          </li>
        ))}
      </ul>
    </div>
  );
}
