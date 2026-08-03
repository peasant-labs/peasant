'use client';

import { memo } from 'react';
import { Handle, Position, type NodeProps } from '@xyflow/react';
import { TriangleAlert } from 'lucide-react';
import { cn } from '@/lib/utils';
import {
  NODE_BORDER,
  NODE_COUNT_TEXT,
  NODE_FILL,
  NODE_TEXT,
  INTENSITY_BG,
  effortLevel,
  traceabilityLevel,
} from './intensity';
import type { MapNodeDatum, NodeDelta, SizeMetric } from './types';

/** `data` payload for the `mapNode` React Flow node type. */
export interface MapSquareNodeData {
  node: MapNodeDatum;
  selected: boolean;
  /** Keyboard cursor in the canvas-level keyboard model. */
  focused: boolean;
  delta?: NodeDelta;
  /** Effort overlay toggle — renders the bottom-edge intensity bar. */
  showEffort: boolean;
  sizeMetric: SizeMetric;
  /**
   * Stable DOM id for the node root (`mapNodeDomId(node.id)`) — the
   * `aria-activedescendant` target on the canvas wrapper.
   */
  domId?: string;
  /** Whether expand/zoom-in applies (the node has children). */
  expandable?: boolean;
  /** Whether the node is in the zoom's expanded set. */
  expanded?: boolean;
  /**
   * Violations whose endpoints both collapse inside this aggregate at the
   * current grain (`aggregateViolationsDetailed().containedByNode`).
   */
  containedViolations?: number;
  /**
   * Cross-cutting emphasis (e.g. a hovered task's edited files): a
   * `rule-strong` border + a small square ink marker. Never touches the
   * traceability-owned fill, and stays distinct from the selection outline.
   */
  highlighted?: boolean;
  [key: string]: unknown;
}

/** Invisible connection points — edges are data, not user-editable. */
const HIDDEN_HANDLE: React.CSSProperties = {
  opacity: 0,
  width: 1,
  height: 1,
  minWidth: 0,
  minHeight: 0,
  border: 'none',
  pointerEvents: 'none',
};

const KIND_LABEL: Record<MapNodeDatum['kind'], string> = {
  module: 'module',
  package: 'package',
  file: 'file',
};

/**
 * Stable DOM id for a node root, derived from the node id (repo-relative
 * path). Deterministic — the canvas wrapper points `aria-activedescendant`
 * at it, and pages may use it for scroll/anchor targeting.
 */
export function mapNodeDomId(nodeId: string): string {
  return `map-node-${nodeId.replace(/\s+/g, '_')}`;
}

/** State fragment of a node's accessible label. */
export interface MapNodeAriaState {
  selected?: boolean;
  expanded?: boolean;
  delta?: NodeDelta;
  containedViolations?: number;
  highlighted?: boolean;
}

/**
 * The node's accessible name: identity + coverage, then delta, hidden
 * violations, and selection/expansion state. Shared by the node root's
 * `aria-label` and the canvas's live-region announcements so the two never
 * drift.
 */
export function mapNodeAriaLabel(node: MapNodeDatum, state: MapNodeAriaState = {}): string {
  const { selected, expanded, delta, containedViolations, highlighted } = state;
  let label = `${node.name} · ${KIND_LABEL[node.kind]} · ${node.recordedFiles} of ${
    node.totalFiles
  } files recorded`;
  if (delta === 'new') label += ' · new';
  else if (delta === 'removed') label += ' · removed';
  if (containedViolations && containedViolations > 0) {
    label += ` · contains ${containedViolations} violation${containedViolations === 1 ? '' : 's'}`;
  }
  if (highlighted) label += ' · highlighted';
  if (selected) label += ' · selected';
  if (expanded) label += ' · expanded';
  return label;
}

/**
 * The square map node: hairline border, fill dimmed stepwise by
 * traceability coverage (the channel's only owner), `font-mono` counts,
 * optional bottom-edge effort intensity bar, and NEW/removed delta states per
 * the delta-state rules in DESIGN_SYSTEM.md. Selection outline is
 * `--rule-strong` — never a
 * glow.
 */
function MapSquareNodeImpl({ data }: NodeProps) {
  const d = data as MapSquareNodeData;
  const {
    node,
    selected,
    focused,
    delta,
    showEffort,
    sizeMetric,
    domId,
    expandable,
    expanded,
    containedViolations = 0,
    highlighted = false,
  } = d;

  const level = traceabilityLevel(node.recordedFiles, node.totalFiles);
  const isNew = delta === 'new';
  const removed = delta === 'removed';

  const metricText =
    sizeMetric === 'touchCount' ? `${node.touchCount} touches` : `${node.loc} loc`;
  const ariaLabel = mapNodeAriaLabel(node, {
    selected,
    expanded,
    delta,
    containedViolations,
    highlighted,
  });

  const outline = selected
    ? { outline: '2px solid hsl(var(--rule-strong))', outlineOffset: '1px' }
    : focused
      ? { outline: '2px solid hsl(var(--focus))', outlineOffset: '1px' }
      : undefined;

  return (
    <div
      id={domId}
      role="button"
      aria-label={ariaLabel}
      aria-pressed={selected}
      aria-expanded={expandable ? !!expanded : undefined}
      data-state={selected ? 'selected' : focused ? 'focused' : undefined}
      className={cn(
        'relative h-full w-full overflow-hidden border px-2 py-1.5 text-left focus-mono cursor-pointer',
        removed
          ? 'border-dashed border-rule bg-transparent'
          : cn(NODE_BORDER[level], NODE_FILL[level]),
        isNew && 'border-rule-strong',
        // Highlight emphasizes via border + a square ink marker — it never
        // touches the traceability-owned fill, and the selection outline
        // (2px offset outline) stays visually distinct on top of it.
        highlighted && 'border-rule-strong',
      )}
      style={outline}
    >
      <Handle type="target" position={Position.Top} style={HIDDEN_HANDLE} isConnectable={false} />
      {highlighted && (
        <span
          aria-hidden
          data-testid="highlight-marker"
          className="absolute left-0 top-0 h-1.5 w-1.5 bg-ink"
        />
      )}
      <div className="flex items-start justify-between gap-1">
        <span
          className={cn(
            'truncate text-[12px] font-medium leading-tight',
            // Removed nodes use the receding `ink-4` label for
            // removed nodes (sub-AA by design — the state is "no longer there").
            removed ? 'text-ink-4' : NODE_TEXT[level],
          )}
        >
          {node.name}
        </span>
        {(containedViolations > 0 || isNew) && (
          <span className="flex shrink-0 items-center gap-1">
            {containedViolations > 0 && (
              <span
                aria-hidden
                data-testid="violation-badge"
                className="flex items-center gap-0.5 font-mono tabular-nums text-[10px] leading-tight text-danger"
              >
                <TriangleAlert size={10} aria-hidden />
                {containedViolations}
              </span>
            )}
            {isNew && <span className="v2-eyebrow shrink-0">NEW</span>}
          </span>
        )}
      </div>
      <span
        className={cn(
          'block truncate font-mono tabular-nums text-[10px] leading-tight',
          removed ? 'text-ink-4' : NODE_COUNT_TEXT[level],
        )}
      >
        {node.recordedFiles}/{node.totalFiles} · {metricText}
      </span>
      {showEffort && !removed && node.effortDensity > 0 && (
        <span
          aria-hidden
          data-testid="effort-bar"
          className={cn(
            'absolute inset-x-0 bottom-0 h-[3px]',
            INTENSITY_BG[effortLevel(node.effortDensity)],
          )}
        />
      )}
      <Handle
        type="source"
        position={Position.Bottom}
        style={HIDDEN_HANDLE}
        isConnectable={false}
      />
    </div>
  );
}

export const MapSquareNode = memo(MapSquareNodeImpl);
