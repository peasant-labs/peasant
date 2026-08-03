/**
 * Prop types for the map-canvas library.
 *
 * The canvas library is a pure component layer: it does not import the
 * page-level API client or any REST/WS decoding logic. But its shapes are
 * NOT a hand-maintained second copy of the wire contract either — every
 * datum type below is derived (via `Pick`) from the canonical
 * `@peasant-labs/schema` root types, so a schema field rename or addition
 * is a type error here rather than silent drift. Only the type-level import
 * is canvas-owned; adapting a payload into these shapes is still the
 * identity function (see `@/app/map/lib/mapData.ts`, the page-boundary
 * mapper).
 */

import type {
  ActivityEdge as SchemaActivityEdge,
  EdgeViolation as SchemaEdgeViolation,
  EdgeViolationKind as SchemaEdgeViolationKind,
  MapEdge as SchemaMapEdge,
  MapNode as SchemaMapNode,
  MapNodeKind as SchemaMapNodeKind,
} from '@peasant-labs/schema';

export type MapNodeKind = SchemaMapNodeKind;

/** A node in the map tree (all zoom levels; `parent` links form the tree). */
export type MapNodeDatum = Pick<
  SchemaMapNode,
  'id' | 'parent' | 'kind' | 'name' | 'language' | 'layer' | 'order' | 'loc' | 'fileCount' | 'recordedFiles' | 'totalFiles' | 'touchCount' | 'effortDensity' | 'agentEditedCount' | 'readCount' | 'readAttribution' | 'readState' | 'changedRegionCount' | 'attributedRegionCount' | 'reviewedRegionCount'
>;

/** A structure (import) edge, aggregated per node pair. */
export type MapEdgeDatum = Pick<SchemaMapEdge, 'from' | 'to' | 'count'>;

/** A co-edit (activity) observation edge. */
export type ActivityEdgeDatum = Pick<SchemaActivityEdge, 'from' | 'to' | 'taskCount'>;

export type EdgeViolationKind = SchemaEdgeViolationKind;

export type EdgeViolationDatum = Pick<SchemaEdgeViolation, 'kind' | 'from' | 'to'>;

/** The switchable node-width metric; one metric owns the width channel. */
export type SizeMetric = 'loc' | 'touchCount';

/** Semantic zoom levels describe what a node is, not just its scale. */
export type ZoomLevel = 'project' | 'package' | 'file';

/**
 * Zoom state: a coarse level plus per-node in-place expansions. An expanded
 * node renders its children in place while collapsed siblings stay aggregate.
 */
export interface MapZoom {
  level: ZoomLevel;
  expanded: ReadonlySet<string>;
}

/** Delta states rendered according to the rules in DESIGN_SYSTEM.md. */
export type NodeDelta = 'new' | 'removed';
export type EdgeDelta = 'new' | 'removed';
