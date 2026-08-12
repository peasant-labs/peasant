"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import { ChevronRight, SearchIcon } from "lucide-react";
import { displayProject } from "@/lib/quality/utils";
import { Explainer, type ExplainerState } from "@/components/Explainer";
import { Term } from "@/components/Term";
import { Skeleton } from "@/lib/skeleton";
import { Input, Tooltip } from "@/lib/ft-ui";
import type { DecodedProjectSummariesPayload } from '@/lib/api/map';
import type { SessionSummary } from "@/types/messages";
import { formatRelative } from "@/app/review/[[...segments]]/format";
import {
  UNASSIGNED_PROJECT,
  resolveProjectHash,
} from "@/app/review/[[...segments]]/sessions";
import { mapHref, parseProjectHash, reviewHref } from "@/lib/navigation/projectRoutes";

/**
 * The ONE project picker, shared by Home (`/`) and the Map picker (`/map`).
 * Before this, the two surfaces drew the same projects with different columns,
 * headers, sorting and filter thresholds. Now both
 * render identical rows — only the row's destination route differs.
 */

/** A bare home directory (e.g. /Users/sampleuser) isn't really a project. */
export function isHomeFolder(name: string): boolean {
  return name === "~" || /^[/\\](Users|home)[/\\][^/\\]+[/\\]?$/.test(name);
}

export interface PickerRow {
  /** Raw project name (route segment / title); display via displayProject(). */
  name: string;
  /** Opaque projectHash for the REST calls; null when unresolvable. */
  hash: string | null;
  sessions: number;
  /** null = stat unavailable (summaries still loading or failed). */
  recordedFiles: number | null;
  totalFiles: number | null;
  lastWorkMs: number | null;
  openChanges: number | null;
}

/** Canonical row order: most-recently-worked first, then by session count —
 *  the active project (the one you're in) floats to the top on both surfaces. */
function byRecency(a: PickerRow, b: PickerRow): number {
  return (b.lastWorkMs ?? 0) - (a.lastWorkMs ?? 0) || b.sessions - a.sessions;
}

/** Picker rows from the summary endpoint (full stats). Only reads `projects`
 * (never `selection`), so callers that don't have selection-state handy can
 * pass just that field. */
export function rowsFromSummaries(payload: Pick<DecodedProjectSummariesPayload, 'projects'>): PickerRow[] {
  return payload.projects
    .map((p) => ({
      name: p.project,
      hash: p.projectHash,
      sessions: p.sessions,
      recordedFiles: p.recordedFiles,
      totalFiles: p.totalFiles,
      lastWorkMs: p.lastWorkMs ?? null,
      openChanges: p.openChanges,
    }))
    .sort(byRecency);
}

/**
 * Fallback rows from the sessions WS channel while the summary fetch is in
 * flight or failed: session counts and last-work are real (derived from the
 * channel); recorded coverage and open changes are unavailable (—).
 */
export function rowsFromSessions(sessions: SessionSummary[]): PickerRow[] {
  const byName = new Map<string, { sessions: number; lastWorkMs: number | null }>();
  for (const s of sessions) {
    const name = s.project ?? UNASSIGNED_PROJECT;
    const parsed = Date.parse(s.startTime);
    const startMs = Number.isNaN(parsed) ? null : parsed;
    const existing = byName.get(name);
    if (!existing) {
      byName.set(name, { sessions: 1, lastWorkMs: startMs });
    } else {
      existing.sessions++;
      if (startMs !== null && (existing.lastWorkMs === null || startMs > existing.lastWorkMs)) {
        existing.lastWorkMs = startMs;
      }
    }
  }
  return Array.from(byName.entries())
    .map(([name, agg]) => ({
      name,
      hash: resolveProjectHash(sessions, name),
      sessions: agg.sessions,
      recordedFiles: null,
      totalFiles: null,
      lastWorkMs: agg.lastWorkMs,
      openChanges: null,
    }))
    .sort(byRecency);
}

/**
 * The shared "What am I looking at?" explainer for BOTH picker surfaces, so the
 * column meaning lives on screen (not only in the header tooltips) and the two
 * surfaces can't drift. `destination` only swaps the closing call-to-action.
 */
export function PickerExplainer({
  explainer,
  destination,
}: {
  explainer: ExplainerState;
  destination: "changes" | "map";
}) {
  return (
    <Explainer explainer={explainer} title="what am I looking at?">
      <p>
        Each row is a project this tool found recorded AI conversations for. The
        columns show <Term k="coverage">recorded files</Term> out of total project
        files, when it was last worked on, and how many{" "}
        <Term k="change">lines of work</Term> (branches) haven&rsquo;t been merged
        back into the main version yet.
      </p>
      <p>A recorded file has an edit captured during a saved AI conversation.</p>
      <p>Click a project to see its {destination === "map" ? "map" : "changes"}.</p>
    </Explainer>
  );
}

const PICKER_GRID =
  "grid grid-cols-[minmax(0,1fr)_104px_92px_104px_24px] items-center gap-3 px-5";

/** Coverage cell: the "N of M" count plus a monochrome micro-bar of the
 *  recorded/total ratio — a per-row glance at how much was built on record.
 *  While `pending` (per-project stats still loading), it shows a sized skeleton
 *  rather than "—", so the count + bar never flash empty→value. */
function CoverageCell({ row, pending }: { row: PickerRow; pending: boolean }) {
  const has = row.recordedFiles !== null && row.totalFiles !== null && row.totalFiles > 0;
  const pct = has ? Math.min(100, Math.round((row.recordedFiles! / row.totalFiles!) * 100)) : 0;
  // Stats not yet resolved (and not failed) → shimmer in place of the count/bar.
  if (!has && pending) {
    return (
      <span className="flex flex-col items-end gap-1">
        <Skeleton className="h-3 w-12" />
        <Skeleton className="h-1 w-16" />
      </span>
    );
  }
  return (
    <span className="flex flex-col items-end gap-1">
      <span className="text-xs font-mono text-ink-3 tabular-nums">
        {has ? `${row.recordedFiles} of ${row.totalFiles}` : "—"}
      </span>
      {has && (
        <span
          className="block h-1 w-16 border border-rule bg-surface-hover overflow-hidden"
          aria-hidden
          title={`${pct}% of measured files have an edit captured in a saved AI conversation`}
        >
          <span className="block h-full bg-ink" style={{ width: `${pct}%` }} />
        </span>
      )}
    </span>
  );
}

function CoverageHeader() {
  return (
    <Tooltip content="recorded files have an edit captured in a saved AI conversation. total files are all files measured in this project.">
      <button
        type="button"
        aria-label="recorded files out of total project files"
        className="v2-eyebrow flex min-h-6 w-full cursor-help items-center justify-end border-b border-dotted border-ink-4 bg-transparent p-0 text-right focus-mono"
      >
        recorded files / total files
      </button>
    </Tooltip>
  );
}

const DEST = {
  changes: {
    href: reviewHref,
    aria: (label: string) => `Open the changes of ${label}`,
  },
  map: {
    href: mapHref,
    aria: (label: string) => `Open the map of ${label}`,
  },
} as const;

function coverageAccessibleName(row: PickerRow): string {
  return row.recordedFiles !== null && row.totalFiles !== null && row.totalFiles > 0
    ? `${row.recordedFiles} recorded files out of ${row.totalFiles} total files`
    : "recorded and total file counts unavailable";
}

export function ProjectPicker({
  rows,
  destination,
  filterThreshold = 8,
  statsPending = false,
}: {
  rows: PickerRow[];
  /** Where a row click lands. Drives the href + aria only. */
  destination: "changes" | "map";
  /** Show the filter box once the list is at least this long. */
  filterThreshold?: number;
  /** Per-project stats (coverage + unmerged branches) are still loading — show
   *  a skeleton in those cells instead of "—", so they don't pop in. */
  statsPending?: boolean;
}) {
  const [query, setQuery] = useState("");
  const dest = DEST[destination];

  // Filter by display name AND raw path, so typing "peasant" finds it even
  // though the row shows just the leaf.
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(
      (r) =>
        r.name.toLowerCase().includes(q) ||
        displayProject(r.name).toLowerCase().includes(q),
    );
  }, [rows, query]);

  return (
    <div className="flex flex-col gap-2" data-tour="project-picker">
      {rows.length > filterThreshold && (
        <Input
          type="search"
          iconLeft={SearchIcon}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="search projects"
          placeholder="search projects…"
           className="w-full"
        />
      )}

      {visible.length === 0 ? (
        <p className="border border-rule bg-surface px-5 py-6 text-center text-[13px] text-ink-3">
          No projects match &ldquo;{query.trim()}&rdquo;.
        </p>
      ) : (
        <div className="border border-rule bg-surface">
          <div className={`${PICKER_GRID} py-2 border-b border-rule`}>
            <span className="v2-eyebrow">project</span>
            <CoverageHeader />
            <span
              className="v2-eyebrow text-right"
              title="When this project was last worked on in a recorded AI conversation."
            >
              Last worked
            </span>
            <span
              className="v2-eyebrow text-right"
              title="Branches (separate lines of work) that haven't been merged back into the project's main version yet."
            >
              Unmerged branches
            </span>
            <span aria-hidden />
          </div>
          <div className="divide-y divide-rule">
            {visible.map((row) => {
              const projectHash = parseProjectHash(row.hash);
              const home = isHomeFolder(row.name);
              const primary = home ? "Home folder" : displayProject(row.name);
              // Show the raw path when it adds context (short ambiguous names
              // like 'app', or the home folder).
              const showPath = home || primary !== row.name;
              return projectHash ? (
                <Link
                  // Key on the unique projectHash (two project hashes can share
                  // a canonical_cwd → same display name); fall back to name on
                  // the sessions path where hash may be null but names are deduped.
                  key={row.hash ?? row.name}
                  href={dest.href(projectHash)}
                  aria-label={dest.aria(primary)}
                  aria-describedby={`coverage-${row.hash ?? row.name.replace(/[^a-z0-9]+/gi, '-')}`}
                  className={`${PICKER_GRID} py-3 hover:bg-surface-hover transition-colors focus-mono cursor-pointer`}
                >
                  <span id={`coverage-${row.hash ?? row.name.replace(/[^a-z0-9]+/gi, '-')}`} className="sr-only">
                    {coverageAccessibleName(row)}
                  </span>
                  <span className="min-w-0">
                    <span className="block text-[13px] font-medium text-ink truncate">{primary}</span>
                    {showPath && (
                      <span className="block font-mono text-[11px] text-ink-4 truncate" title={row.name}>
                        {home ? "work not tied to a project" : row.name}
                      </span>
                    )}
                  </span>
                  <span
                    aria-label={coverageAccessibleName(row)}
                  >
                    <CoverageCell row={row} pending={statsPending} />
                  </span>
                  <span className="text-xs font-mono text-ink-3 tabular-nums text-right">
                    {row.lastWorkMs !== null ? formatRelative(row.lastWorkMs) : "—"}
                  </span>
                  {row.openChanges !== null ? (
                    <span className="text-xs font-mono text-ink-2 tabular-nums text-right">
                      {row.openChanges}
                    </span>
                  ) : statsPending ? (
                    // Loading — shimmer in place so the count doesn't pop in.
                    <Skeleton className="h-3 w-6 justify-self-end" />
                  ) : (
                    <span className="text-xs font-mono text-ink-2 tabular-nums text-right">—</span>
                  )}
                  <ChevronRight size={15} className="text-ink-4 justify-self-end" aria-hidden />
                </Link>
              ) : (
                <div
                  key={row.name}
                  className={`${PICKER_GRID} py-3 opacity-60`}
                  aria-label={`${primary} cannot be opened because its project identity is unavailable`}
                >
                  <span className="min-w-0">
                    <span className="block text-[13px] font-medium text-ink truncate">{primary}</span>
                    <span className="block font-mono text-[11px] text-ink-4 truncate">project identity unavailable</span>
                  </span>
                  <span
                    aria-label={coverageAccessibleName(row)}
                  >
                    <CoverageCell row={row} pending={statsPending} />
                  </span>
                  <span className="text-xs font-mono text-ink-3 tabular-nums text-right">
                    {row.lastWorkMs !== null ? formatRelative(row.lastWorkMs) : "—"}
                  </span>
                  <span className="text-xs font-mono text-ink-2 tabular-nums text-right">—</span>
                  <span aria-hidden />
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
