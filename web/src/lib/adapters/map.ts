/**
 * Adapter signature for the CodeMap lifted surface.
 *
 * Converts the map graph wire payload (from GET /api/v1/map/{projectHash})
 * into the cooked prop payload the lifted <CodeMap> component accepts.
 * The pattern mirrors SessionDetailV2: fetch → reshape → mount via props.
 *
 * TRANSFORM (not CLEAN): the adapter calls mapGraphToData() from
 * web/src/app/map/lib/mapData.ts to produce topology datums. The lifted
 * <CodeMap> component carries layout.ts internally and derives pixel
 * geometry (x/y + width) from layer/order — geometry is NOT in the payload.
 *
 * activityEdges are excluded from CodeMapPayload (they feed the node-detail
 * rail's "often edited with" rows, wired separately by the host page).
 *
 * Wire type sourced from the generated schema package and normalized by the
 * REST decoder before this adapter runs.
 * Payload type sourced from: @peasant-labs/fairtrade/graph.
 */

import type { CodeMapPayload } from '@peasant-labs/fairtrade/graph';
import type { DecodedMapGraphPayload } from '@/lib/api/map';
import { mapGraphToData } from '@/app/map/lib/mapData';

// ── Adapter signature ─────────────────────────────────────────────────────────

/**
 * Map the map graph wire payload (MapGraphPayload) to the CodeMap surface
 * prop payload. TRANSFORM: uses mapGraphToData() to produce topology datums
 * (mapData.ts); the lifted component handles geometry internally via layout.ts.
 *
 * activityEdges from the wire are NOT included in the payload. The surface
 * slice wires them separately as a node-detail prop for the rail panel.
 *
 * @param wire GET /api/v1/map/{projectHash} response
 * @returns Cooked prop payload for the lifted <CodeMap> surface
 */
export function adaptCodeMap(wire: DecodedMapGraphPayload): CodeMapPayload {
  const topology = mapGraphToData(wire);
  return {
    repoFound: wire.repoFound,
    nodes: topology.nodes,
    structureEdges: topology.structureEdges,
    violations: topology.violations,
  };
}
