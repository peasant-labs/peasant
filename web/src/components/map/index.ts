/**
 * Map canvas library — pure, deterministic components for the Map and Review
 * surfaces. No data fetching, no routes: pages adapt `@peasant-labs/schema`
 * wire payloads (`@/app/map/lib/mapData.ts`) into these prop shapes.
 */

export type {
  ActivityEdgeDatum,
  EdgeDelta,
  EdgeViolationDatum,
  EdgeViolationKind,
  MapEdgeDatum,
  MapNodeDatum,
  MapNodeKind,
  MapZoom,
  NodeDelta,
  SizeMetric,
  ZoomLevel,
} from './types';

export {
  DEFAULT_ZOOM,
  EMPTY_EXPANDED,
  MAX_NODE_WIDTH,
  MIN_NODE_WIDTH,
  NODE_GAP,
  NODE_HEIGHT,
  ROW_HEIGHT,
  aggregateActivityEdges,
  aggregateEdges,
  aggregateStructureEdges,
  aggregateViolations,
  aggregateViolationsDetailed,
  computePositions,
  edgeKey,
  liftIdsToVisible,
  visibleNodes,
} from './layout';
export type {
  AggregatedEdge,
  AggregatedViolations,
  LayoutOptions,
  PositionedMapNode,
} from './layout';

export { isMapKey, reduceMapKey, withExpanded, zoomOut } from './keyboard';
export type { KeyboardNavNode, MapKeyEvent, MapKeyboardState } from './keyboard';

export {
  INTENSITY_BG,
  NODE_BORDER,
  NODE_COUNT_TEXT,
  NODE_FILL,
  NODE_TEXT,
  effortLevel,
  quantizeLevel,
  traceabilityLevel,
} from './intensity';
export type { IntensityLevel } from './intensity';

export { MapCanvas } from './MapCanvas';
export type { MapCanvasProps } from './MapCanvas';
export { MapSquareNode, mapNodeAriaLabel, mapNodeDomId } from './MapSquareNode';
export type { MapNodeAriaState, MapSquareNodeData } from './MapSquareNode';
export {
  ACTIVITY_DASH,
  ACTIVITY_REMOVED_DASH,
  ActivityFlowEdge,
  StructureEdge,
  activityStrokeWidth,
} from './edges';
export type { MapFlowEdgeData } from './edges';
export { TimeStrip } from './TimeStrip';
export type { TimeStripBranch, TimeStripDay, TimeStripProps } from './TimeStrip';
export { RailShell, RailSection } from './RailShell';
export type { RailSectionProps, RailShellProps } from './RailShell';
