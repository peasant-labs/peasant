'use client';

import Link from 'next/link';
import { cn } from '@/lib/utils';
import { pathsMatch, type TurnFileTouches } from '../lib/scopeTurns';
import { mapHref, type ProjectHash } from '@/lib/navigation/projectRoutes';

interface TurnTouchedFilesProps {
  /**
   * This turn's file touches (from `collectFileTouches`, which relativizes
   * raw wire paths to repo-relative Map node ids); caller skips empty.
   */
  touches: TurnFileTouches;
  /** Canonical project identity used to build /map/<hash>?node=<path> links. */
  projectHash: ProjectHash;
  /** The file-scope path, when active — its rows render underlined. */
  activeFile?: string;
  className?: string;
}

/**
 * Per-turn touched-files panel: the turn's tool-call file touches as
 * font-mono rows, edits (attribution) visually distinct from reads (context).
 * Every file links to its node on the Map. Mounted by the SessionDetailV2
 * adapter through the package's `renderTurnPanel` slot while a scope is
 * active, so the files sit inside the turn card they belong to.
 */
export function TurnTouchedFiles({
  touches,
  projectHash,
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
          projectHash={projectHash}
          activeFile={activeFile}
        />
      )}
      {touches.reads.length > 0 && (
        <FileGroup
          label="files read"
          files={touches.reads}
          kind="read"
          projectHash={projectHash}
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
  projectHash,
  activeFile,
}: {
  label: string;
  files: string[];
  kind: 'edit' | 'read';
  projectHash: ProjectHash;
  activeFile?: string;
}) {
  return (
    <div className="min-w-0">
      <p className="v2-eyebrow">{label}</p>
      <ul className="mt-0.5 flex flex-col">
        {files.map((filePath) => (
          <li key={filePath}>
            <Link
              href={mapHref(projectHash, { node: filePath })}
              aria-label={`Open ${filePath} on the Map`}
              className={cn(
                'block break-all font-mono text-[12px] leading-5 focus-mono cursor-pointer hover:underline',
                kind === 'read' ? 'text-ink-3' : 'text-ink',
                !!activeFile && pathsMatch(activeFile, filePath) && 'underline',
              )}
            >
              {filePath}
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}
