'use client';

import { BaseEdge, getSmoothStepPath, type EdgeProps } from '@xyflow/react';
import type { EdgeDelta } from './types';

/** `data` payload for the custom map edge types. */
export interface MapFlowEdgeData {
  /** Aggregated weight (import count / shared-task count). */
  weight: number;
  /** Structure edges only: render in danger tokens (cycle / wrong-way). */
  violation?: boolean;
  delta?: EdgeDelta;
  [key: string]: unknown;
}

/**
 * Heavier hairline for stronger co-edit coupling — weight maps to stroke
 * width, never to color. The floor is about two shared tasks; clamped so the
 * edge stays a hairline.
 */
export function activityStrokeWidth(taskCount: number): number {
  return Math.min(1 + Math.max(0, taskCount - 2) * 0.25, 2.5);
}

function useOrthogonalPath(props: EdgeProps): string {
  const [path] = getSmoothStepPath({
    sourceX: props.sourceX,
    sourceY: props.sourceY,
    targetX: props.targetX,
    targetY: props.targetY,
    sourcePosition: props.sourcePosition,
    targetPosition: props.targetPosition,
    borderRadius: 0, // square corners — radius is 0, everywhere
  });
  return path;
}

/**
 * Structure (import) edge: solid orthogonal hairline in `--edge`; violations
 * are the only red on the canvas (`--danger`); NEW edges use `--edge-strong`;
 * removed edges go dashed.
 */
export function StructureEdge(props: EdgeProps) {
  const path = useOrthogonalPath(props);
  const d = (props.data ?? { weight: 0 }) as MapFlowEdgeData;
  const stroke = d.violation
    ? 'hsl(var(--danger))'
    : d.delta === 'new'
      ? 'hsl(var(--edge-strong))'
      : 'hsl(var(--edge))';
  return (
    <BaseEdge
      id={props.id}
      path={path}
      markerEnd={props.markerEnd}
      style={{
        stroke,
        strokeWidth: d.delta === 'new' ? 1.5 : 1,
        strokeDasharray: d.delta === 'removed' ? '2 4' : undefined,
      }}
    />
  );
}

/** Dash pattern for unchanged/new activity edges (4 on, 3 off). */
export const ACTIVITY_DASH = '4 3';
/**
 * Dash pattern for REMOVED activity edges. Activity edges are already dashed,
 * so the map's "removed = dashed" rule carries no signal here — the removed
 * state recedes instead: a sparser dash (1.5 on, 4.5 off — 25% ink duty vs
 * the unchanged ~57%) at fixed 1px width (no weight scaling). The stroke
 * stays `--edge`: `ink-4` is *higher*-contrast than `--edge` on the canvas
 * in both themes, so swapping the token would make removed edges louder,
 * not quieter.
 */
export const ACTIVITY_REMOVED_DASH = '1.5 4.5';

/**
 * Activity (co-edit) edge: dashed orthogonal hairline, width scaled by
 * shared-task count. No arrowhead — co-work is symmetric. Removed edges
 * recede via `ACTIVITY_REMOVED_DASH` + fixed 1px width.
 *
 * The Map surface does not pass activity edges to the canvas
 * (co-edit coupling reads as node-panel rows instead). This edge type stays
 * because Review's changed-slice canvas (`app/review/.../ChangeDetail.tsx`)
 * still renders `payload.slice.activityEdges` through `MapCanvas`.
 */
export function ActivityFlowEdge(props: EdgeProps) {
  const path = useOrthogonalPath(props);
  const d = (props.data ?? { weight: 0 }) as MapFlowEdgeData;
  const removed = d.delta === 'removed';
  const stroke = d.delta === 'new' ? 'hsl(var(--edge-strong))' : 'hsl(var(--edge))';
  return (
    <BaseEdge
      id={props.id}
      path={path}
      style={{
        stroke,
        strokeWidth: removed ? 1 : activityStrokeWidth(d.weight),
        strokeDasharray: removed ? ACTIVITY_REMOVED_DASH : ACTIVITY_DASH,
      }}
    />
  );
}
