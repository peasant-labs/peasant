/**
 * Shared test fixtures for the map canvas library. A small three-module
 * repository tree with packages and
 * files, structure/activity edges at mixed grains, and violations — enough
 * to exercise visibility, aggregation, centering, and the keyboard model.
 *
 * Not exported from the library barrel; test-only.
 */

import type {
  ActivityEdgeDatum,
  EdgeViolationDatum,
  MapEdgeDatum,
  MapNodeDatum,
  MapZoom,
} from './types';
import type { IntensityLevel } from './intensity';
import type { KeyboardNavNode } from './keyboard';

function node(partial: Partial<MapNodeDatum> & Pick<MapNodeDatum, 'id' | 'kind' | 'name' | 'layer' | 'order'>): MapNodeDatum {
  return {
    parent: undefined,
    language: undefined,
    loc: 0,
    fileCount: 1,
    recordedFiles: 0,
    totalFiles: 1,
    touchCount: 0,
    effortDensity: 0,
    agentEditedCount: 0,
    readCount: 0,
    readAttribution: 'unavailable',
    readState: 'none',
    changedRegionCount: 0,
    attributedRegionCount: 0,
    reviewedRegionCount: 0,
    ...partial,
  };
}

/**
 * Tree:
 *   cmd (module, layer 0)
 *   internal (module, layer 1)
 *   ├─ internal/ingest (package)
 *   │  ├─ internal/ingest/pipeline.go (file)
 *   │  └─ internal/ingest/git.go (file)
 *   └─ internal/store (package)
 *   web (module, layer 1 — dark matter)
 *   └─ web/lib (package)
 */
export const FIXTURE_NODES: MapNodeDatum[] = [
  node({
    id: 'cmd', kind: 'module', name: 'cmd', layer: 0, order: 0,
    loc: 400, fileCount: 2, recordedFiles: 2, totalFiles: 2, touchCount: 4,
  }),
  node({
    id: 'internal', kind: 'module', name: 'internal', layer: 1, order: 0,
    loc: 3000, fileCount: 16, recordedFiles: 14, totalFiles: 16, touchCount: 30,
    effortDensity: 0.4,
  }),
  node({
    id: 'web', kind: 'module', name: 'web', layer: 1, order: 1,
    loc: 2000, fileCount: 20, recordedFiles: 0, totalFiles: 20, touchCount: 0,
  }),
  node({
    id: 'internal/ingest', parent: 'internal', kind: 'package', name: 'ingest',
    layer: 1, order: 0, loc: 1200, fileCount: 7, recordedFiles: 6, totalFiles: 7,
    touchCount: 18, effortDensity: 0.8,
  }),
  node({
    id: 'internal/store', parent: 'internal', kind: 'package', name: 'store',
    layer: 2, order: 0, loc: 1800, fileCount: 9, recordedFiles: 8, totalFiles: 9,
    touchCount: 12,
  }),
  node({
    id: 'web/lib', parent: 'web', kind: 'package', name: 'lib',
    layer: 2, order: 1, loc: 900, fileCount: 8, recordedFiles: 0, totalFiles: 8,
    touchCount: 0,
  }),
  node({
    id: 'internal/ingest/pipeline.go', parent: 'internal/ingest', kind: 'file',
    name: 'pipeline.go', layer: 1, order: 0, loc: 700, recordedFiles: 1,
    totalFiles: 1, touchCount: 9,
  }),
  node({
    id: 'internal/ingest/git.go', parent: 'internal/ingest', kind: 'file',
    name: 'git.go', layer: 1, order: 1, loc: 500, recordedFiles: 0,
    totalFiles: 1, touchCount: 9, effortDensity: 1,
  }),
];

export const FIXTURE_STRUCTURE_EDGES: MapEdgeDatum[] = [
  { from: 'cmd', to: 'internal/ingest', count: 2 },
  { from: 'internal/ingest/pipeline.go', to: 'internal/store', count: 1 },
  { from: 'internal/ingest/git.go', to: 'internal/store', count: 1 },
  { from: 'web/lib', to: 'internal/store', count: 3 },
];

export const FIXTURE_ACTIVITY_EDGES: ActivityEdgeDatum[] = [
  { from: 'internal/ingest', to: 'web/lib', taskCount: 4 },
  { from: 'internal/ingest/git.go', to: 'web/lib', taskCount: 2 },
];

export const FIXTURE_VIOLATIONS: EdgeViolationDatum[] = [
  // Cycle inside `internal` — collapses to a self-pair at project zoom.
  { kind: 'cycle', from: 'internal/store', to: 'internal/ingest' },
  // Two wrong-way violations that lift to the same module pair.
  { kind: 'wrong_way', from: 'internal/store', to: 'cmd' },
  { kind: 'wrong_way', from: 'internal/ingest/git.go', to: 'cmd' },
];

/**
 * Regression fixture for layer compaction (the real-repo bug): root-level
 * modules on layer 0 plus unparsed root files the server once exiled to
 * distant layers (10/11/12). Layout compaction must render consecutive rows
 * 0..3 — no empty vertical band between the cluster and the root files.
 */
export const SPARSE_LAYER_NODES: MapNodeDatum[] = [
  node({ id: 'cmd', kind: 'module', name: 'cmd', layer: 0, order: 0, loc: 400 }),
  node({ id: 'internal', kind: 'module', name: 'internal', layer: 0, order: 1, loc: 3000 }),
  node({ id: 'AGENTS.md', kind: 'file', name: 'AGENTS.md', layer: 10, order: 0, loc: 50 }),
  node({ id: 'CLAUDE.md', kind: 'file', name: 'CLAUDE.md', layer: 11, order: 1, loc: 80 }),
  node({ id: 'llm', kind: 'module', name: 'llm', layer: 12, order: 2, loc: 120 }),
];

export const PROJECT_ZOOM: MapZoom = { level: 'project', expanded: new Set() };
export const PACKAGE_ZOOM: MapZoom = { level: 'package', expanded: new Set() };
export const FILE_ZOOM: MapZoom = { level: 'file', expanded: new Set() };

/**
 * One node per traceability level, for the per-level rendering matrix.
 * Level 4 sits exactly on the 0.9 "fully recorded" boundary.
 */
export const LEVEL_NODES: Record<IntensityLevel, MapNodeDatum> = {
  4: node({
    id: 'lvl4', kind: 'package', name: 'lvl4', layer: 0, order: 0,
    loc: 100, fileCount: 10, recordedFiles: 9, totalFiles: 10, // 0.9 → 4
  }),
  3: node({
    id: 'lvl3', kind: 'package', name: 'lvl3', layer: 0, order: 1,
    loc: 100, fileCount: 7, recordedFiles: 6, totalFiles: 7, // ≈0.857 → 3
  }),
  2: node({
    id: 'lvl2', kind: 'package', name: 'lvl2', layer: 0, order: 2,
    loc: 100, fileCount: 4, recordedFiles: 1, totalFiles: 4, // 0.25 → 2
  }),
  1: node({
    id: 'lvl1', kind: 'package', name: 'lvl1', layer: 0, order: 3,
    loc: 100, fileCount: 5, recordedFiles: 1, totalFiles: 5, // 0.2 → 1
  }),
  0: node({
    id: 'lvl0', kind: 'package', name: 'lvl0', layer: 0, order: 4,
    loc: 100, fileCount: 20, recordedFiles: 0, totalFiles: 20, // dark matter
  }),
};

/**
 * Keyboard navigation fixture (layers are sparse on purpose — layer 2 is
 * empty so Up/Down must find the *nearest* layer):
 *
 *   layer 0:  a0(order 0)   a1(order 2)   a2(order 4)
 *   layer 1:  b0(order 1)   b1(order 3)
 *   layer 3:  c0(order 0)
 */
export const NAV_NODES: KeyboardNavNode[] = [
  { id: 'a0', layer: 0, order: 0, hasChildren: true },
  { id: 'a1', layer: 0, order: 2, hasChildren: false },
  { id: 'a2', layer: 0, order: 4, hasChildren: false },
  { id: 'b0', layer: 1, order: 1, hasChildren: true },
  { id: 'b1', layer: 1, order: 3, hasChildren: false },
  { id: 'c0', layer: 3, order: 0, hasChildren: false },
];
