/**
 * Deterministic layered layout for the map canvas.
 *
 * The server assigns every node a `layer` (row) and `order` (stable sort
 * within layer); this module converts layer/order into x/y pixel positions
 * plus a metric-scaled width. Everything here is a pure function of its
 * inputs — no randomness, no clock reads, no force physics — so the same
 * graph yields the same picture on every render and every machine.
 */

import type {
  ActivityEdgeDatum,
  EdgeViolationDatum,
  MapEdgeDatum,
  MapNodeDatum,
  MapZoom,
  SizeMetric,
  ZoomLevel,
} from './types';

// ---------------------------------------------------------------------------
// Geometry constants (px). Exposed for callers and tests.
// ---------------------------------------------------------------------------

export const NODE_HEIGHT = 56;
export const MIN_NODE_WIDTH = 96;
export const MAX_NODE_WIDTH = 280;
/** Vertical pitch between layer rows (> NODE_HEIGHT so edges can route). */
export const ROW_HEIGHT = 128;
/** Horizontal gap between siblings within a row. */
export const NODE_GAP = 24;

export interface LayoutOptions {
  rowHeight?: number;
  nodeGap?: number;
  minNodeWidth?: number;
  maxNodeWidth?: number;
  nodeHeight?: number;
}

export interface PositionedMapNode {
  node: MapNodeDatum;
  x: number;
  y: number;
  width: number;
  height: number;
}

/** Shared empty expansion set (a `ReadonlySet` is safe to share). */
export const EMPTY_EXPANDED: ReadonlySet<string> = new Set<string>();

export const DEFAULT_ZOOM: MapZoom = { level: 'project', expanded: EMPTY_EXPANDED };

// ---------------------------------------------------------------------------
// Tree helpers
// ---------------------------------------------------------------------------

interface TreeIndex {
  byId: Map<string, MapNodeDatum>;
  /** parent id → children sorted by (order, id). */
  children: Map<string, MapNodeDatum[]>;
  /** Roots sorted by (layer, order, id). */
  roots: MapNodeDatum[];
}

function compareOrderId(a: MapNodeDatum, b: MapNodeDatum): number {
  if (a.order !== b.order) return a.order - b.order;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

function buildTreeIndex(nodes: readonly MapNodeDatum[]): TreeIndex {
  const byId = new Map<string, MapNodeDatum>();
  for (const n of nodes) byId.set(n.id, n);

  const children = new Map<string, MapNodeDatum[]>();
  const roots: MapNodeDatum[] = [];
  for (const n of nodes) {
    if (n.parent && byId.has(n.parent)) {
      const list = children.get(n.parent);
      if (list) list.push(n);
      else children.set(n.parent, [n]);
    } else {
      roots.push(n);
    }
  }
  for (const list of children.values()) list.sort(compareOrderId);
  roots.sort((a, b) => {
    if (a.layer !== b.layer) return a.layer - b.layer;
    return compareOrderId(a, b);
  });
  return { byId, children, roots };
}

/**
 * Base tree depth rendered per zoom level. `project` renders the top-level
 * modules (depth 0). `package` opens every module to its packages (depth 1).
 * `file` ALSO bases at depth 1 — file grain is reached only through explicit
 * per-node expansion, because the canvas never draws the whole repo at file
 * grain so collapsed siblings stay aggregate.
 */
const BASE_DEPTH: Record<ZoomLevel, number> = {
  project: 0,
  package: 1,
  file: 1,
};

/**
 * Derives which tree level renders for the given zoom + expanded set. A node
 * renders as an aggregate unless it descends: nodes shallower than the zoom
 * level's base depth always descend; any node in `zoom.expanded` descends in
 * place (its collapsed siblings stay aggregate). Leaves always render.
 */
export function visibleNodes(nodes: readonly MapNodeDatum[], zoom: MapZoom): MapNodeDatum[] {
  const { children, roots } = buildTreeIndex(nodes);
  const base = BASE_DEPTH[zoom.level];
  const out: MapNodeDatum[] = [];

  const visit = (node: MapNodeDatum, depth: number): void => {
    const kids = children.get(node.id);
    const descend = !!kids && kids.length > 0 && (depth < base || zoom.expanded.has(node.id));
    if (descend) {
      for (const kid of kids) visit(kid, depth + 1);
    } else {
      out.push(node);
    }
  };
  for (const root of roots) visit(root, 0);
  return out;
}

// ---------------------------------------------------------------------------
// Positioning
// ---------------------------------------------------------------------------

function metricValue(node: MapNodeDatum, metric: SizeMetric): number {
  const v = metric === 'touchCount' ? node.touchCount : node.loc;
  return v > 0 ? v : 0;
}

/**
 * Pure layer/order → x/y conversion for the currently visible nodes.
 *
 * - `y` = compacted row index × rowHeight. The distinct layer values present
 *   among the VISIBLE nodes are mapped to consecutive rows (0, 1, 2, …) in
 *   ascending layer order, so a sparse or outlier layer number (e.g. an
 *   unparsed root file on layer 10 while every visible sibling sits on
 *   layer 0) can never create an empty vertical band.
 * - `x` = running offset within the row by (order, id), centered per row
 *   against the widest row.
 * - `width` scales linearly with the active size metric, normalized against
 *   the max visible value and clamped to [min, max]. All-zero metrics yield
 *   the minimum width everywhere.
 *
 * Fully deterministic: input order never affects output (sorting is total),
 * and there is no randomness and no clock access.
 */
export function computePositions(
  nodes: readonly MapNodeDatum[],
  sizeMetric: SizeMetric = 'loc',
  zoom: MapZoom = DEFAULT_ZOOM,
  opts: LayoutOptions = {},
): PositionedMapNode[] {
  const rowHeight = opts.rowHeight ?? ROW_HEIGHT;
  const gap = opts.nodeGap ?? NODE_GAP;
  const minW = opts.minNodeWidth ?? MIN_NODE_WIDTH;
  const maxW = opts.maxNodeWidth ?? MAX_NODE_WIDTH;
  const height = opts.nodeHeight ?? NODE_HEIGHT;

  const visible = visibleNodes(nodes, zoom);
  if (visible.length === 0) return [];

  let maxMetric = 0;
  for (const n of visible) maxMetric = Math.max(maxMetric, metricValue(n, sizeMetric));

  const widthOf = (n: MapNodeDatum): number => {
    if (maxMetric === 0) return minW;
    const ratio = metricValue(n, sizeMetric) / maxMetric;
    return Math.round(minW + ratio * (maxW - minW));
  };

  // Group by layer, sort within layer by (order, id).
  const layers = new Map<number, MapNodeDatum[]>();
  for (const n of visible) {
    const row = layers.get(n.layer);
    if (row) row.push(n);
    else layers.set(n.layer, [n]);
  }
  const layerKeys = Array.from(layers.keys()).sort((a, b) => a - b);
  for (const key of layerKeys) layers.get(key)!.sort(compareOrderId);

  // Compact the distinct layer values to consecutive row indices.
  const rowOf = new Map<number, number>();
  layerKeys.forEach((key, index) => rowOf.set(key, index));

  // Per-layer centering against the widest row.
  const rowWidth = (row: MapNodeDatum[]): number =>
    row.reduce((sum, n) => sum + widthOf(n), 0) + gap * (row.length - 1);
  let maxRowWidth = 0;
  for (const key of layerKeys) maxRowWidth = Math.max(maxRowWidth, rowWidth(layers.get(key)!));

  const out: PositionedMapNode[] = [];
  for (const key of layerKeys) {
    const row = layers.get(key)!;
    let x = (maxRowWidth - rowWidth(row)) / 2;
    const y = rowOf.get(key)! * rowHeight;
    for (const n of row) {
      const width = widthOf(n);
      out.push({ node: n, x, y, width, height });
      x += width + gap;
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Edge aggregation up to visible ancestors
// ---------------------------------------------------------------------------

export interface AggregatedEdge {
  from: string;
  to: string;
  /** Summed weight of the underlying edges (import count / task count). */
  weight: number;
}

/** Stable composite key for an aggregated node pair. */
export function edgeKey(from: string, to: string): string {
  return `${from}→${to}`;
}

function makeAncestorResolver(
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): (id: string) => string | undefined {
  const parentOf = new Map<string, string | undefined>();
  for (const n of nodes) parentOf.set(n.id, n.parent || undefined);
  const memo = new Map<string, string | undefined>();
  return (id: string): string | undefined => {
    if (memo.has(id)) return memo.get(id);
    let cur: string | undefined = id;
    // Walk up the parent chain until a visible node (or the root) is found.
    while (cur !== undefined && !visibleIds.has(cur)) {
      cur = parentOf.has(cur) ? parentOf.get(cur) : undefined;
    }
    memo.set(id, cur);
    return cur;
  };
}

/**
 * Aggregates edges between nodes at any grain up to their nearest visible
 * ancestors, summing weights per (from, to) pair. Edges whose endpoints
 * collapse into the same visible node (intra-aggregate) or whose endpoints
 * are unknown are dropped. Output is sorted by (from, to) for determinism.
 */
export function aggregateEdges<E extends { from: string; to: string }>(
  edges: readonly E[],
  weightOf: (edge: E) => number,
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): AggregatedEdge[] {
  const resolve = makeAncestorResolver(nodes, visibleIds);
  const acc = new Map<string, AggregatedEdge>();
  for (const edge of edges) {
    const from = resolve(edge.from);
    const to = resolve(edge.to);
    if (!from || !to || from === to) continue;
    const key = edgeKey(from, to);
    const existing = acc.get(key);
    if (existing) existing.weight += weightOf(edge);
    else acc.set(key, { from, to, weight: weightOf(edge) });
  }
  return Array.from(acc.values()).sort((a, b) =>
    a.from !== b.from ? (a.from < b.from ? -1 : 1) : a.to < b.to ? -1 : a.to > b.to ? 1 : 0,
  );
}

/**
 * Lifts node ids (any grain) to their nearest visible ancestors — the same
 * rule edge aggregation uses. Ids already visible map to themselves; ids
 * with no visible ancestor drop out. Used by the canvas to carry a hidden
 * (collapsed) node's highlight on its visible aggregate.
 */
export function liftIdsToVisible(
  ids: Iterable<string>,
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): ReadonlySet<string> {
  const resolve = makeAncestorResolver(nodes, visibleIds);
  const out = new Set<string>();
  for (const id of ids) {
    const visible = resolve(id);
    if (visible) out.add(visible);
  }
  return out;
}

/** Convenience: aggregate structure edges (`count` weights). */
export function aggregateStructureEdges(
  edges: readonly MapEdgeDatum[],
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): AggregatedEdge[] {
  return aggregateEdges(edges, (e) => e.count, nodes, visibleIds);
}

/** Convenience: aggregate activity edges (`taskCount` weights). */
export function aggregateActivityEdges(
  edges: readonly ActivityEdgeDatum[],
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): AggregatedEdge[] {
  return aggregateEdges(edges, (e) => e.taskCount, nodes, visibleIds);
}

/** Result of `aggregateViolationsDetailed`. */
export interface AggregatedViolations {
  /** Violations lifted to visible ancestor pairs (= `aggregateViolations`). */
  lifted: EdgeViolationDatum[];
  /**
   * Visible node id → the *original* violations whose endpoints both
   * collapse into that node. These are invisible as edges at the current
   * grain — the canvas badges the owning aggregate and the rail panel can
   * list them.
   */
  containedByNode: Map<string, EdgeViolationDatum[]>;
}

function compareViolations(a: EdgeViolationDatum, b: EdgeViolationDatum): number {
  const ka = `${a.kind}|${a.from}|${a.to}`;
  const kb = `${b.kind}|${b.from}|${b.to}`;
  return ka < kb ? -1 : ka > kb ? 1 : 0;
}

/**
 * Lifts violations to visible ancestors (deduped by kind + lifted pair) and
 * additionally reports, per visible node, the original violations that
 * collapsed entirely inside it — so a cycle hidden inside an aggregate is
 * never silently dropped because violations are the visual alarm.
 * Output is deterministically sorted.
 */
export function aggregateViolationsDetailed(
  violations: readonly EdgeViolationDatum[],
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): AggregatedViolations {
  const resolve = makeAncestorResolver(nodes, visibleIds);
  const seenLifted = new Set<string>();
  const seenContained = new Set<string>();
  const lifted: EdgeViolationDatum[] = [];
  const containedByNode = new Map<string, EdgeViolationDatum[]>();
  for (const v of violations) {
    const from = resolve(v.from);
    const to = resolve(v.to);
    if (!from || !to) continue;
    if (from === to) {
      const key = `${from}|${v.kind}|${v.from}|${v.to}`;
      if (seenContained.has(key)) continue;
      seenContained.add(key);
      const list = containedByNode.get(from);
      if (list) list.push(v);
      else containedByNode.set(from, [v]);
      continue;
    }
    const key = `${v.kind}|${edgeKey(from, to)}`;
    if (seenLifted.has(key)) continue;
    seenLifted.add(key);
    lifted.push({ kind: v.kind, from, to });
  }
  lifted.sort(compareViolations);
  for (const list of containedByNode.values()) list.sort(compareViolations);
  return { lifted, containedByNode };
}

/**
 * Lifts violations to visible ancestors and dedupes by (kind, from, to).
 * A violation fully inside one collapsed aggregate disappears *as an edge*
 * at that grain — use `aggregateViolationsDetailed` to also surface those
 * contained violations on the owning node.
 */
export function aggregateViolations(
  violations: readonly EdgeViolationDatum[],
  nodes: readonly MapNodeDatum[],
  visibleIds: ReadonlySet<string>,
): EdgeViolationDatum[] {
  return aggregateViolationsDetailed(violations, nodes, visibleIds).lifted;
}
