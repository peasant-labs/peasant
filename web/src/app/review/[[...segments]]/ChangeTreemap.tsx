'use client';

/* DEPRECATED — prior-version review surface (the change-weight treemap).
   SUPERSEDED by the lifted <Changes>/<ChangeDetail> from @peasant-labs/fairtrade/graph,
   mounted via ReviewSurface.tsx — the route no longer renders this. Retained DORMANT
   (not imported by the new /review path) as a deprecation candidate; its tests still
   exercise it. Do not extend; remove once the lifted surface is settled (tracked non-blocking, deprecation candidate). */

import { useMemo } from 'react';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';
import { quantizeLevel } from '@/components/map';
import { Treemap, type TreemapDatum } from '@/lib/ft-ui';

/** Stable DOM id for a changed-file row, shared with ChangeDetail's FileRow so
 *  the treemap can scroll to the exact file. getElementById tolerates slashes. */
export function fileRowAnchor(path: string): string {
  return `review-file:${path}`;
}

/**
 * Change-weight treemap (roadmap 5.3): a strict-monochrome "where did this
 * change land" overview above the file list. One tile per changed file, AREA
 * proportional to churn (added+removed lines); a 0..4 grayscale intensity
 * reinforces relative churn weight. No color, no cost, no verdict — facts for
 * orientation. Clicking a tile jumps to that file's row below.
 *
 * Renders the ft-ui Treemap component (squarified, accessibility-compliant,
 * label-suppressed on slivers but always titled + aria-labelled).
 *
 * NOTE: intensity encodes churn relative to the max changed file (same axis
 * as area), because per-file recency is not available in ChangeDetailPayload.
 * If per-file lastTouchMs is added to the API, intensity could encode recency
 * independently (area = churn, intensity = recency — the intended dual encoding).
 */
export function ChangeTreemap({
  payload,
  onSelectFile,
}: {
  payload: DecodedChangeDetailPayload;
  onSelectFile: (path: string) => void;
}) {
  const { data, totalChurn } = useMemo(() => {
    // Two-pass: first find max churn for quantizeLevel, then build TreemapDatum[].
    let maxChurn = 0;
    for (const f of payload.files) {
      const churn = f.linesAdded + f.linesRemoved;
      if (churn > maxChurn) maxChurn = churn;
    }
    let totalChurn = 0;
    const data: TreemapDatum[] = payload.files.map((f) => {
      const churn = f.linesAdded + f.linesRemoved;
      totalChurn += churn;
      return {
        id: f.path,
        label: f.path,
        value: churn,
        intensity: quantizeLevel(churn, maxChurn),
        unit: 'lines',
      };
    });
    return { data, totalChurn };
  }, [payload.files]);

  if (payload.files.length === 0) return null;

  const churnLabel = totalChurn > 0 ? ` · +/−${totalChurn} lines` : '';

  return (
    <section aria-label="Where the change landed" className="border border-rule bg-surface">
      <div className="flex items-baseline justify-between gap-3 border-b border-rule px-5 py-2">
        <span className="v2-eyebrow">where it landed</span>
        <span className="font-mono text-[11px] tabular-nums text-ink-4">
          {payload.files.length} file{payload.files.length === 1 ? '' : 's'}
          {churnLabel}
        </span>
      </div>
      {/* ft-ui Treemap: squarified, one tile per file, area ∝ churn.
          height=240 gives a reasonable banner aspect in a 4:3 layout space. */}
      <Treemap
        data={data}
        height={240}
        ariaLabel={`Changed files sized by churn${churnLabel}`}
        onSelect={(id) => onSelectFile(id)}
      />
    </section>
  );
}
