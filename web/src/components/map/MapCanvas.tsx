'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  MarkerType,
  type Edge,
  type EdgeTypes,
  type Node,
  type NodeTypes,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import { Maximize, Minus, Plus } from 'lucide-react';
import { cn } from '@/lib/utils';
import {
  DEFAULT_ZOOM,
  EMPTY_EXPANDED,
  aggregateActivityEdges,
  aggregateStructureEdges,
  aggregateViolationsDetailed,
  computePositions,
  edgeKey,
  liftIdsToVisible,
} from './layout';
import {
  isMapKey,
  reduceMapKey,
  withExpanded,
  type KeyboardNavNode,
  type MapKeyboardState,
} from './keyboard';
import {
  MapSquareNode,
  mapNodeAriaLabel,
  mapNodeDomId,
  type MapSquareNodeData,
} from './MapSquareNode';
import { ActivityFlowEdge, StructureEdge, type MapFlowEdgeData } from './edges';
import type {
  ActivityEdgeDatum,
  EdgeDelta,
  EdgeViolationDatum,
  MapEdgeDatum,
  MapNodeDatum,
  MapZoom,
  NodeDelta,
  SizeMetric,
} from './types';

const NODE_TYPES: NodeTypes = { mapNode: MapSquareNode };
const EDGE_TYPES: EdgeTypes = { structure: StructureEdge, activity: ActivityFlowEdge };

/**
 * Shared fit constraints (first-paint legibility): every fitView call — the
 * initial fit, the refit on visible-set changes, and the Fit button — clamps
 * to [FIT_MIN_ZOOM, FIT_MAX_ZOOM]. The floor keeps node labels (12px) at
 * ≥6px so a fit never shrinks the graph to confetti — an oversized graph
 * overflows the viewport and pans instead; the ceiling keeps a 2-node graph
 * from being blown up to comical size. Manual zoom keeps the wider
 * CANVAS_MIN_ZOOM..CANVAS_MAX_ZOOM range.
 */
export const FIT_VIEW_OPTIONS = { padding: 0.15, minZoom: 0.5, maxZoom: 1.25 } as const;
const CANVAS_MIN_ZOOM = 0.2;
const CANVAS_MAX_ZOOM = 2;

export interface MapCanvasProps {
  /** Full node tree (all zoom levels); `parent` links form the tree. */
  nodes: MapNodeDatum[];
  /** Solid import edges (any grain; aggregated up to visible ancestors). */
  structureEdges?: MapEdgeDatum[];
  /** Dashed co-edit edges (any grain; aggregated up to visible ancestors). */
  activityEdges?: ActivityEdgeDatum[];
  /** Cycles + wrong-way edges — the only red on the canvas. */
  violations?: EdgeViolationDatum[];
  /** Node-width metric (default `'loc'`). */
  sizeMetric?: SizeMetric;
  /** Controlled zoom state; omit for internal (uncontrolled) zoom. */
  zoom?: MapZoom;
  /** Controlled selection; omit for internal (uncontrolled) selection. */
  selectedId?: string | null;
  /** Per-node delta states by node id (Review changed-slice). */
  nodeDeltas?: Record<string, NodeDelta>;
  /**
   * Legacy per-edge delta states keyed with `edgeKey(from, to)`, applied to
   * BOTH edge families of a node pair. Prefer the per-family
   * `structureEdgeDeltas` / `activityEdgeDeltas`, which take precedence
   * per key over this prop.
   */
  edgeDeltas?: Record<string, EdgeDelta>;
  /**
   * Delta states for STRUCTURE (import) edges only, keyed with
   * `edgeKey(from, to)`. Takes precedence over `edgeDeltas` for its family.
   */
  structureEdgeDeltas?: Record<string, EdgeDelta>;
  /**
   * Delta states for ACTIVITY (co-edit) edges only, keyed with
   * `edgeKey(from, to)`. Takes precedence over `edgeDeltas` for its family.
   */
  activityEdgeDeltas?: Record<string, EdgeDelta>;
  /**
   * Cross-cutting node emphasis (e.g. a hovered task's edited files), any
   * grain: ids hidden at the current zoom light up their nearest visible
   * ancestor. Border + ink-marker treatment — never the fill channel, and
   * distinct from the selection outline.
   */
  highlightedIds?: ReadonlySet<string>;
  /** Effort overlay: bottom-edge intensity bar per node (default off). */
  showEffortOverlay?: boolean;
  onSelect?: (id: string | null) => void;
  onExpand?: (id: string) => void;
  onZoomChange?: (zoom: MapZoom) => void;
  /** Wrapper aria-label (default "Code map"). */
  ariaLabel?: string;
  /** Size the canvas via the parent (the wrapper fills it). */
  className?: string;
}

/** True when the user prefers reduced motion — pan/zoom easing is disabled. */
function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(false);
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia('(prefers-reduced-motion: reduce)');
    setReduced(mq.matches);
    const onChange = (e: MediaQueryListEvent) => setReduced(e.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);
  return reduced;
}

/** On-screen square zoom controls use Lucide icons, never emoji. */
function ZoomControls({ reducedMotion }: { reducedMotion: boolean }) {
  const rf = useReactFlow();
  const duration = reducedMotion ? 0 : 200;
  const btn =
    'flex h-7 w-7 items-center justify-center border border-rule bg-surface text-ink-2 hover:bg-surface-hover focus-mono cursor-pointer';
  return (
    <div className="absolute bottom-3 right-3 z-10 flex flex-col" role="group" aria-label="Map zoom controls">
      <button type="button" aria-label="Zoom in" className={cn(btn, 'border-b-0')} onClick={() => rf.zoomIn({ duration })}>
        <Plus size={14} aria-hidden />
      </button>
      <button type="button" aria-label="Zoom out" className={cn(btn, 'border-b-0')} onClick={() => rf.zoomOut({ duration })}>
        <Minus size={14} aria-hidden />
      </button>
      <button
        type="button"
        aria-label="Fit map to view"
        className={btn}
        onClick={() => rf.fitView({ ...FIT_VIEW_OPTIONS, duration })}
      >
        <Maximize size={14} aria-hidden />
      </button>
    </div>
  );
}

/**
 * The reusable map canvas uses deterministic layered layout rendered
 * through React Flow with fully controlled positions (`nodesDraggable=false`,
 * no force physics), square metric-encoded nodes, orthogonal edges (solid
 * structure / dashed activity / danger violations), semantic zoom via node
 * visibility + per-node expansion, and the canvas keyboard model. Pure
 * component — no data fetching; pages adapt wire payloads into props.
 */
export function MapCanvas(props: MapCanvasProps) {
  return (
    <ReactFlowProvider>
      <MapCanvasInner {...props} />
    </ReactFlowProvider>
  );
}

function MapCanvasInner({
  nodes,
  structureEdges = [],
  activityEdges = [],
  violations = [],
  sizeMetric = 'loc',
  zoom,
  selectedId,
  nodeDeltas,
  edgeDeltas,
  structureEdgeDeltas,
  activityEdgeDeltas,
  highlightedIds,
  showEffortOverlay = false,
  onSelect,
  onExpand,
  onZoomChange,
  ariaLabel,
  className,
}: MapCanvasProps) {
  const rf = useReactFlow();
  const reducedMotion = usePrefersReducedMotion();

  // Controlled-or-internal zoom + selection (the library is usable standalone).
  const [internalZoom, setInternalZoom] = useState<MapZoom>(DEFAULT_ZOOM);
  const [internalSelected, setInternalSelected] = useState<string | null>(null);
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const effZoom = zoom ?? internalZoom;
  const effSelected = selectedId !== undefined ? selectedId : internalSelected;

  const applySelection = useCallback(
    (next: string | null) => {
      setInternalSelected(next);
      onSelect?.(next);
    },
    [onSelect],
  );

  const applyZoom = useCallback(
    (next: MapZoom, prev: MapZoom) => {
      setInternalZoom(next);
      for (const id of next.expanded) {
        if (!prev.expanded.has(id)) onExpand?.(id);
      }
      onZoomChange?.(next);
    },
    [onExpand, onZoomChange],
  );

  const positioned = useMemo(
    () => computePositions(nodes, sizeMetric, effZoom),
    [nodes, sizeMetric, effZoom],
  );
  const visibleIds = useMemo(
    () => new Set(positioned.map((p) => p.node.id)),
    [positioned],
  );

  const childCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const n of nodes) {
      if (n.parent) counts.set(n.parent, (counts.get(n.parent) ?? 0) + 1);
    }
    return counts;
  }, [nodes]);

  const navNodes: KeyboardNavNode[] = useMemo(
    () =>
      positioned.map((p) => ({
        id: p.node.id,
        layer: p.node.layer,
        order: p.node.order,
        hasChildren: (childCounts.get(p.node.id) ?? 0) > 0,
      })),
    [positioned, childCounts],
  );

  // Violations lifted to visible pairs + the ones hidden inside aggregates
  // (the latter badge the owning node — the alarm never silently vanishes).
  const violationAgg = useMemo(
    () => aggregateViolationsDetailed(violations, nodes, visibleIds),
    [violations, nodes, visibleIds],
  );

  // Highlights lifted to visible ancestors — a collapsed file's highlight
  // carries on its visible aggregate (same lifting rule as edges).
  const visibleHighlights = useMemo<ReadonlySet<string>>(
    () =>
      highlightedIds && highlightedIds.size > 0
        ? liftIdsToVisible(highlightedIds, nodes, visibleIds)
        : EMPTY_EXPANDED,
    [highlightedIds, nodes, visibleIds],
  );

  const rfNodes: Node[] = useMemo(
    () =>
      positioned.map((p) => {
        const data: MapSquareNodeData = {
          node: p.node,
          selected: p.node.id === effSelected,
          focused: p.node.id === focusedId,
          delta: nodeDeltas?.[p.node.id],
          showEffort: showEffortOverlay,
          sizeMetric,
          domId: mapNodeDomId(p.node.id),
          expandable: (childCounts.get(p.node.id) ?? 0) > 0,
          expanded: effZoom.expanded.has(p.node.id),
          containedViolations: violationAgg.containedByNode.get(p.node.id)?.length ?? 0,
          highlighted: visibleHighlights.has(p.node.id),
        };
        return {
          id: p.node.id,
          type: 'mapNode',
          position: { x: p.x, y: p.y },
          style: { width: p.width, height: p.height },
          draggable: false,
          data,
        };
      }),
    [
      positioned,
      effSelected,
      focusedId,
      nodeDeltas,
      showEffortOverlay,
      sizeMetric,
      childCounts,
      effZoom,
      violationAgg,
      visibleHighlights,
    ],
  );

  const rfEdges: Edge[] = useMemo(() => {
    const aggStructure = aggregateStructureEdges(structureEdges, nodes, visibleIds);
    const aggActivity = aggregateActivityEdges(activityEdges, nodes, visibleIds);
    const aggViolations = violationAgg.lifted;
    const violationKeys = new Set(aggViolations.map((v) => edgeKey(v.from, v.to)));
    const structureKeys = new Set(aggStructure.map((e) => edgeKey(e.from, e.to)));

    // Per-family deltas win over the legacy both-families `edgeDeltas` prop.
    const structureDelta = (key: string): EdgeDelta | undefined =>
      structureEdgeDeltas?.[key] ?? edgeDeltas?.[key];
    const activityDelta = (key: string): EdgeDelta | undefined =>
      activityEdgeDeltas?.[key] ?? edgeDeltas?.[key];

    const marker = (color: string) => ({
      type: MarkerType.ArrowClosed,
      width: 14,
      height: 14,
      color,
    });

    const edges: Edge[] = aggStructure.map((e) => {
      const key = edgeKey(e.from, e.to);
      const violation = violationKeys.has(key);
      const delta = structureDelta(key);
      const data: MapFlowEdgeData = { weight: e.weight, violation, delta };
      return {
        id: `s:${key}`,
        source: e.from,
        target: e.to,
        type: 'structure',
        data,
        markerEnd: marker(
          violation
            ? 'hsl(var(--danger))'
            : delta === 'new'
              ? 'hsl(var(--edge-strong))'
              : 'hsl(var(--edge))',
        ),
      };
    });

    // Violation pairs with no surviving structure edge still render — the
    // alarm is never hidden by aggregation of the underlying import edges.
    for (const v of aggViolations) {
      const key = edgeKey(v.from, v.to);
      if (structureKeys.has(key)) continue;
      structureKeys.add(key);
      const data: MapFlowEdgeData = { weight: 0, violation: true };
      edges.push({
        id: `v:${key}`,
        source: v.from,
        target: v.to,
        type: 'structure',
        data,
        markerEnd: marker('hsl(var(--danger))'),
      });
    }

    for (const e of aggActivity) {
      const key = edgeKey(e.from, e.to);
      const data: MapFlowEdgeData = { weight: e.weight, delta: activityDelta(key) };
      edges.push({ id: `a:${key}`, source: e.from, target: e.to, type: 'activity', data });
    }
    return edges;
  }, [
    structureEdges,
    activityEdges,
    violationAgg,
    nodes,
    visibleIds,
    edgeDeltas,
    structureEdgeDeltas,
    activityEdgeDeltas,
  ]);

  // Re-fit when the visible set changes (zoom level / expansion), honoring
  // prefers-reduced-motion. The initial fit comes from the `fitView` prop.
  const visKey = useMemo(() => positioned.map((p) => p.node.id).join('|'), [positioned]);
  const firstFit = useRef(true);
  useEffect(() => {
    if (firstFit.current) {
      firstFit.current = false;
      return;
    }
    rf.fitView({ ...FIT_VIEW_OPTIONS, duration: reducedMotion ? 0 : 200 });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [visKey]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      if (!isMapKey(e.key)) return;
      const before: MapKeyboardState = {
        focusedId,
        selectedId: effSelected,
        zoom: effZoom,
      };
      const after = reduceMapKey(before, { key: e.key, shiftKey: e.shiftKey }, navNodes);
      if (after === before) return;
      e.preventDefault();
      e.stopPropagation();
      if (after.focusedId !== before.focusedId) setFocusedId(after.focusedId);
      if (after.selectedId !== before.selectedId) applySelection(after.selectedId);
      if (after.zoom !== before.zoom) applyZoom(after.zoom, before.zoom);
    },
    [focusedId, effSelected, effZoom, navNodes, applySelection, applyZoom],
  );

  const handleNodeClick = useCallback(
    (_e: React.MouseEvent, node: Node) => {
      // Click selects and anchors keyboard focus; it never changes zoom.
      setFocusedId(node.id);
      applySelection(node.id);
    },
    [applySelection],
  );

  const handleNodeDoubleClick = useCallback(
    (_e: React.MouseEvent, node: Node) => {
      if ((childCounts.get(node.id) ?? 0) === 0) return;
      const next = withExpanded(effZoom, node.id);
      if (next !== effZoom) applyZoom(next, effZoom);
    },
    [childCounts, effZoom, applyZoom],
  );

  const handlePaneClick = useCallback(() => applySelection(null), [applySelection]);

  // Screen-reader feedback for the roving keyboard focus: the wrapper points
  // `aria-activedescendant` at the focused node's DOM id, and a visually
  // hidden live region re-announces the node's full label (identity, coverage,
  // delta, hidden violations, selection) whenever it changes.
  const focusedVisible = focusedId !== null && visibleIds.has(focusedId);
  const focusedAnnouncement = useMemo(() => {
    if (!focusedId) return '';
    const data = rfNodes.find((n) => n.id === focusedId)?.data as
      | MapSquareNodeData
      | undefined;
    if (!data) return '';
    return mapNodeAriaLabel(data.node, {
      selected: data.selected,
      expanded: data.expanded,
      delta: data.delta,
      containedViolations: data.containedViolations,
      highlighted: data.highlighted,
    });
  }, [focusedId, rfNodes]);

  return (
    <div
      role="application"
      aria-label={ariaLabel ?? 'Code map'}
      aria-activedescendant={focusedVisible ? mapNodeDomId(focusedId) : undefined}
      tabIndex={0}
      onKeyDown={handleKeyDown}
      className={cn('relative h-full w-full focus-mono', className)}
    >
      <div aria-live="polite" role="status" className="sr-only" data-testid="map-live-region">
        {focusedAnnouncement}
      </div>
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        nodeTypes={NODE_TYPES}
        edgeTypes={EDGE_TYPES}
        onNodeClick={handleNodeClick}
        onNodeDoubleClick={handleNodeDoubleClick}
        onPaneClick={handlePaneClick}
        nodesDraggable={false}
        nodesConnectable={false}
        nodesFocusable={false}
        edgesFocusable={false}
        elementsSelectable={false}
        selectNodesOnDrag={false}
        panOnDrag
        // No scroll-hijack without a modifier: the page keeps
        // scrolling over the canvas; zoom is pinch / controls / modifier key.
        panOnScroll={false}
        zoomOnScroll={false}
        zoomOnPinch
        zoomOnDoubleClick={false}
        preventScrolling={false}
        minZoom={CANVAS_MIN_ZOOM}
        maxZoom={CANVAS_MAX_ZOOM}
        fitView
        fitViewOptions={FIT_VIEW_OPTIONS}
        proOptions={{ hideAttribution: true }}
      >
        <ZoomControls reducedMotion={reducedMotion} />
      </ReactFlow>
    </div>
  );
}
