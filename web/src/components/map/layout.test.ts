import { describe, it, expect } from 'vitest';
import {
  MAX_NODE_WIDTH,
  MIN_NODE_WIDTH,
  NODE_GAP,
  NODE_HEIGHT,
  ROW_HEIGHT,
  aggregateActivityEdges,
  aggregateStructureEdges,
  aggregateViolations,
  aggregateViolationsDetailed,
  computePositions,
  liftIdsToVisible,
  visibleNodes,
  type PositionedMapNode,
} from './layout';
import type { MapNodeDatum, MapZoom } from './types';
import {
  FIXTURE_ACTIVITY_EDGES,
  FIXTURE_NODES,
  FIXTURE_STRUCTURE_EDGES,
  FIXTURE_VIOLATIONS,
  FILE_ZOOM,
  PACKAGE_ZOOM,
  PROJECT_ZOOM,
  SPARSE_LAYER_NODES,
} from './test-fixtures';

/** Deterministic scramble — no Math.random in tests either. */
function scramble<T>(items: readonly T[]): T[] {
  const out: T[] = [];
  const rest = [...items].reverse();
  while (rest.length > 1) {
    out.push(rest.pop()!);
    out.push(rest.shift()!);
  }
  return out.concat(rest);
}

function byId(positioned: PositionedMapNode[]): Record<string, { x: number; y: number; width: number; height: number }> {
  const map: Record<string, { x: number; y: number; width: number; height: number }> = {};
  for (const p of positioned) {
    map[p.node.id] = { x: p.x, y: p.y, width: p.width, height: p.height };
  }
  return map;
}

function ids(nodes: MapNodeDatum[]): string[] {
  return nodes.map((n) => n.id).sort();
}

function visibleIdSet(zoom: MapZoom): Set<string> {
  return new Set(visibleNodes(FIXTURE_NODES, zoom).map((n) => n.id));
}

describe('visibleNodes', () => {
  it('renders only top-level modules at project zoom', () => {
    expect(ids(visibleNodes(FIXTURE_NODES, PROJECT_ZOOM))).toEqual(['cmd', 'internal', 'web']);
  });

  it('expands a module in place at project zoom while siblings stay aggregate', () => {
    const zoom: MapZoom = { level: 'project', expanded: new Set(['internal']) };
    expect(ids(visibleNodes(FIXTURE_NODES, zoom))).toEqual([
      'cmd',
      'internal/ingest',
      'internal/store',
      'web',
    ]);
  });

  it('renders packages at package zoom; childless modules render themselves', () => {
    expect(ids(visibleNodes(FIXTURE_NODES, PACKAGE_ZOOM))).toEqual([
      'cmd',
      'internal/ingest',
      'internal/store',
      'web/lib',
    ]);
  });

  it('reaches file grain only via explicit expansion at file zoom', () => {
    // Unexpanded file zoom stays at package grain — the canvas never draws
    // the whole repository at file grain.
    expect(ids(visibleNodes(FIXTURE_NODES, FILE_ZOOM))).toEqual(
      ids(visibleNodes(FIXTURE_NODES, PACKAGE_ZOOM)),
    );

    const zoom: MapZoom = { level: 'file', expanded: new Set(['internal/ingest']) };
    expect(ids(visibleNodes(FIXTURE_NODES, zoom))).toEqual([
      'cmd',
      'internal/ingest/git.go',
      'internal/ingest/pipeline.go',
      'internal/store',
      'web/lib',
    ]);
  });
});

describe('computePositions determinism', () => {
  it('yields identical output for the same input', () => {
    const a = computePositions(FIXTURE_NODES, 'loc', PROJECT_ZOOM);
    const b = computePositions(FIXTURE_NODES, 'loc', PROJECT_ZOOM);
    expect(b).toEqual(a);
  });

  it('is independent of input order (shuffled input, identical positions)', () => {
    const a = computePositions(FIXTURE_NODES, 'loc', PACKAGE_ZOOM);
    const shuffled = scramble(FIXTURE_NODES);
    expect(ids(shuffled as MapNodeDatum[])).toEqual(ids(FIXTURE_NODES)); // sanity
    const b = computePositions(shuffled, 'loc', PACKAGE_ZOOM);
    expect(byId(b)).toEqual(byId(a));
  });
});

describe('computePositions layer compaction', () => {
  // Visible layer values map to consecutive rows (0, 1, 2, …) regardless of
  // how sparse the server-assigned numbering is — an outlier layer can never
  // create an empty vertical band.
  const cases: Array<{ name: string; layers: number[]; wantRows: number[] }> = [
    { name: 'already consecutive layers are identity', layers: [0, 1, 2], wantRows: [0, 1, 2] },
    { name: 'outlier root-file layers collapse', layers: [0, 10, 11, 12], wantRows: [0, 1, 2, 3] },
    { name: 'gap in the middle closes', layers: [0, 2, 7], wantRows: [0, 1, 2] },
    { name: 'single distant layer lands on row 0', layers: [5], wantRows: [0] },
    { name: 'duplicate layers share a row', layers: [3, 0, 3, 9], wantRows: [1, 0, 1, 2] },
  ];

  for (const { name, layers, wantRows } of cases) {
    it(name, () => {
      const nodes = layers.map((layer, i) =>
        ({ ...SPARSE_LAYER_NODES[0], id: `n${i}`, name: `n${i}`, layer, order: i }) as MapNodeDatum,
      );
      const pos = byId(computePositions(nodes, 'loc', PROJECT_ZOOM));
      layers.forEach((_, i) => {
        expect(pos[`n${i}`].y, `node n${i}`).toBe(wantRows[i] * ROW_HEIGHT);
      });
    });
  }

  it('renders the real-repo regression (root files on layers 10–12) without an empty band', () => {
    const pos = byId(computePositions(SPARSE_LAYER_NODES, 'loc', PROJECT_ZOOM));
    expect(pos['cmd'].y).toBe(0);
    expect(pos['internal'].y).toBe(0);
    expect(pos['AGENTS.md'].y).toBe(1 * ROW_HEIGHT);
    expect(pos['CLAUDE.md'].y).toBe(2 * ROW_HEIGHT);
    expect(pos['llm'].y).toBe(3 * ROW_HEIGHT);
  });

  it('keeps shuffled-input identity for sparse layers', () => {
    const a = computePositions(SPARSE_LAYER_NODES, 'loc', PROJECT_ZOOM);
    const b = computePositions(scramble(SPARSE_LAYER_NODES), 'loc', PROJECT_ZOOM);
    expect(byId(b)).toEqual(byId(a));
  });
});

describe('computePositions geometry', () => {
  it('places y = row * rowHeight (consecutive layers) and height = NODE_HEIGHT', () => {
    const pos = byId(computePositions(FIXTURE_NODES, 'loc', PROJECT_ZOOM));
    expect(pos['cmd'].y).toBe(0);
    expect(pos['internal'].y).toBe(ROW_HEIGHT);
    expect(pos['web'].y).toBe(ROW_HEIGHT);
    for (const p of Object.values(pos)) expect(p.height).toBe(NODE_HEIGHT);
  });

  it('scales width by the loc metric, clamped to [min, max]', () => {
    const pos = byId(computePositions(FIXTURE_NODES, 'loc', PROJECT_ZOOM));
    // internal has the max loc (3000) → max width.
    expect(pos['internal'].width).toBe(MAX_NODE_WIDTH);
    const span = MAX_NODE_WIDTH - MIN_NODE_WIDTH;
    expect(pos['cmd'].width).toBe(Math.round(MIN_NODE_WIDTH + (400 / 3000) * span));
    expect(pos['web'].width).toBe(Math.round(MIN_NODE_WIDTH + (2000 / 3000) * span));
  });

  it('switches widths when sized by touchCount', () => {
    const pos = byId(computePositions(FIXTURE_NODES, 'touchCount', PROJECT_ZOOM));
    expect(pos['internal'].width).toBe(MAX_NODE_WIDTH); // 30 touches = max
    expect(pos['web'].width).toBe(MIN_NODE_WIDTH); // zero touches = min
  });

  it('uses the minimum width everywhere when the metric is all-zero', () => {
    const zeroTouch = FIXTURE_NODES.map((n) => ({ ...n, touchCount: 0 }));
    const pos = computePositions(zeroTouch, 'touchCount', PROJECT_ZOOM);
    for (const p of pos) expect(p.width).toBe(MIN_NODE_WIDTH);
  });

  it('lays a row out by order with the node gap and centers rows against the widest', () => {
    const positioned = computePositions(FIXTURE_NODES, 'loc', PROJECT_ZOOM);
    const pos = byId(positioned);

    // Layer 1 (internal, web — ordered 0, 1) is the widest row → starts at 0.
    expect(pos['internal'].x).toBe(0);
    expect(pos['web'].x).toBe(pos['internal'].width + NODE_GAP);

    // Layer 0 (cmd alone) is centered against the widest row.
    const layer1Width = pos['internal'].width + NODE_GAP + pos['web'].width;
    expect(pos['cmd'].x).toBe((layer1Width - pos['cmd'].width) / 2);

    // Row centers coincide.
    const center0 = pos['cmd'].x + pos['cmd'].width / 2;
    const center1 = (pos['internal'].x + (pos['web'].x + pos['web'].width)) / 2;
    expect(center0).toBeCloseTo(center1, 6);
  });

  it('returns empty for an empty graph', () => {
    expect(computePositions([], 'loc', PROJECT_ZOOM)).toEqual([]);
  });
});

describe('edge aggregation up to visible ancestors', () => {
  it('lifts file/package structure edges to modules and drops intra-aggregate edges', () => {
    const agg = aggregateStructureEdges(
      FIXTURE_STRUCTURE_EDGES,
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    // pipeline.go→store and git.go→store collapse into internal→internal → dropped.
    expect(agg).toEqual([
      { from: 'cmd', to: 'internal', weight: 2 },
      { from: 'web', to: 'internal', weight: 3 },
    ]);
  });

  it('sums weights of edges that lift to the same visible pair', () => {
    const agg = aggregateStructureEdges(
      FIXTURE_STRUCTURE_EDGES,
      FIXTURE_NODES,
      visibleIdSet(PACKAGE_ZOOM),
    );
    expect(agg).toEqual([
      { from: 'cmd', to: 'internal/ingest', weight: 2 },
      { from: 'internal/ingest', to: 'internal/store', weight: 2 },
      { from: 'web/lib', to: 'internal/store', weight: 3 },
    ]);
  });

  it('aggregates activity edges by taskCount', () => {
    const agg = aggregateActivityEdges(
      FIXTURE_ACTIVITY_EDGES,
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    expect(agg).toEqual([{ from: 'internal', to: 'web', weight: 6 }]);
  });

  it('drops edges with unknown endpoints', () => {
    const agg = aggregateStructureEdges(
      [{ from: 'cmd', to: 'vendor/x', count: 1 }],
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    expect(agg).toEqual([]);
  });

  it('lifts and dedupes violations; intra-aggregate violations disappear at that grain', () => {
    const project = aggregateViolations(
      FIXTURE_VIOLATIONS,
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    // The cycle inside `internal` collapses; the two wrong-way violations
    // dedupe into one module-level pair.
    expect(project).toEqual([{ kind: 'wrong_way', from: 'internal', to: 'cmd' }]);

    const pkg = aggregateViolations(
      FIXTURE_VIOLATIONS,
      FIXTURE_NODES,
      visibleIdSet(PACKAGE_ZOOM),
    );
    expect(pkg).toEqual([
      { kind: 'cycle', from: 'internal/store', to: 'internal/ingest' },
      { kind: 'wrong_way', from: 'internal/ingest', to: 'cmd' },
      { kind: 'wrong_way', from: 'internal/store', to: 'cmd' },
    ]);
  });
});

describe('aggregateViolationsDetailed', () => {
  it('reports contained violations per owning visible node alongside the lifted ones', () => {
    const detailed = aggregateViolationsDetailed(
      FIXTURE_VIOLATIONS,
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    // Lifted set matches the legacy export exactly.
    expect(detailed.lifted).toEqual(
      aggregateViolations(FIXTURE_VIOLATIONS, FIXTURE_NODES, visibleIdSet(PROJECT_ZOOM)),
    );
    // The store↔ingest cycle collapses inside `internal` — surfaced, not dropped.
    expect(Array.from(detailed.containedByNode.keys())).toEqual(['internal']);
    expect(detailed.containedByNode.get('internal')).toEqual([
      { kind: 'cycle', from: 'internal/store', to: 'internal/ingest' },
    ]);
  });

  it('reports nothing contained when every violation lifts to a visible pair', () => {
    const detailed = aggregateViolationsDetailed(
      FIXTURE_VIOLATIONS,
      FIXTURE_NODES,
      visibleIdSet(PACKAGE_ZOOM),
    );
    expect(detailed.containedByNode.size).toBe(0);
    expect(detailed.lifted).toHaveLength(3);
  });

  it('dedupes identical contained violations per owner', () => {
    const detailed = aggregateViolationsDetailed(
      [
        { kind: 'cycle', from: 'internal/store', to: 'internal/ingest' },
        { kind: 'cycle', from: 'internal/store', to: 'internal/ingest' },
      ],
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    expect(detailed.containedByNode.get('internal')).toHaveLength(1);
  });
});

describe('liftIdsToVisible', () => {
  it('maps visible ids to themselves and collapsed ids to their visible ancestor', () => {
    const lifted = liftIdsToVisible(
      ['cmd', 'internal/ingest/pipeline.go', 'web/lib'],
      FIXTURE_NODES,
      visibleIdSet(PROJECT_ZOOM),
    );
    expect(Array.from(lifted).sort()).toEqual(['cmd', 'internal', 'web']);
  });

  it('drops unknown ids', () => {
    const lifted = liftIdsToVisible(['vendor/x'], FIXTURE_NODES, visibleIdSet(PROJECT_ZOOM));
    expect(lifted.size).toBe(0);
  });
});
