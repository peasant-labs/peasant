/**
 * Pure keyboard reducer for the map canvas.
 *
 * Model:
 * - `ArrowLeft` / `ArrowRight` move focus within the focused node's layer by
 *   order.
 * - `ArrowUp` / `ArrowDown` move focus to the nearest layer in that direction,
 *   landing on the node with the nearest order (ties → lower order).
 * - `Enter` selects the focused node (opens its rail panel).
 * - `Shift+Enter` or `E` expands the focused node in place (zoom-in).
 * - `Escape` deselects first; then collapses the most recent expansion; then
 *   steps the zoom level out one level (file → package → project).
 *
 * The reducer is pure: it never mutates its inputs and returns the *same
 * state reference* when a key is a no-op, so hosts can detect handled keys
 * by identity and decide whether to `preventDefault`.
 */

import type { MapZoom, ZoomLevel } from './types';

/** Minimal navigation projection of a visible node. */
export interface KeyboardNavNode {
  id: string;
  layer: number;
  order: number;
  /** Whether expand/zoom-in (`Shift+Enter` / `E`) applies to this node. */
  hasChildren: boolean;
}

export interface MapKeyboardState {
  /** Keyboard cursor — which node arrows move from. */
  focusedId: string | null;
  /** Selection — which node's rail panel is open. */
  selectedId: string | null;
  zoom: MapZoom;
}

export interface MapKeyEvent {
  key: string;
  shiftKey?: boolean;
}

const MAP_KEYS = new Set([
  'ArrowLeft',
  'ArrowRight',
  'ArrowUp',
  'ArrowDown',
  'Enter',
  'e',
  'E',
  'Escape',
]);

/** True when the host should route the key through the canvas reducer. */
export function isMapKey(key: string): boolean {
  return MAP_KEYS.has(key);
}

const ZOOM_OUT: Record<ZoomLevel, ZoomLevel | null> = {
  file: 'package',
  package: 'project',
  project: null,
};

/** Returns a zoom with `id` added to the expanded set (new set, inputs untouched). */
export function withExpanded(zoom: MapZoom, id: string): MapZoom {
  if (zoom.expanded.has(id)) return zoom;
  const expanded = new Set(zoom.expanded);
  expanded.add(id);
  return { level: zoom.level, expanded };
}

/**
 * One step of zoom-out: drop the most recently added expansion if any
 * (JS sets preserve insertion order), else step the level down. Returns the
 * same reference when already fully zoomed out.
 */
export function zoomOut(zoom: MapZoom): MapZoom {
  if (zoom.expanded.size > 0) {
    const ids = Array.from(zoom.expanded);
    ids.pop();
    return { level: zoom.level, expanded: new Set(ids) };
  }
  const down = ZOOM_OUT[zoom.level];
  if (down === null) return zoom;
  return { level: down, expanded: zoom.expanded };
}

function compareOrderId(a: KeyboardNavNode, b: KeyboardNavNode): number {
  if (a.order !== b.order) return a.order - b.order;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/** First node in reading order: lowest layer, then lowest order, then id. */
function firstNode(nodes: readonly KeyboardNavNode[]): KeyboardNavNode | undefined {
  let best: KeyboardNavNode | undefined;
  for (const n of nodes) {
    if (
      !best ||
      n.layer < best.layer ||
      (n.layer === best.layer && compareOrderId(n, best) < 0)
    ) {
      best = n;
    }
  }
  return best;
}

function siblingInLayer(
  nodes: readonly KeyboardNavNode[],
  cur: KeyboardNavNode,
  dir: -1 | 1,
): KeyboardNavNode | undefined {
  const row = nodes.filter((n) => n.layer === cur.layer).sort(compareOrderId);
  const idx = row.findIndex((n) => n.id === cur.id);
  if (idx === -1) return undefined;
  return row[idx + dir];
}

function nearestAcrossLayers(
  nodes: readonly KeyboardNavNode[],
  cur: KeyboardNavNode,
  dir: -1 | 1,
): KeyboardNavNode | undefined {
  // Nearest existing layer in the direction (layers may be sparse).
  let targetLayer: number | undefined;
  for (const n of nodes) {
    const delta = (n.layer - cur.layer) * dir;
    if (delta <= 0) continue;
    if (targetLayer === undefined || (n.layer - targetLayer) * dir < 0) targetLayer = n.layer;
  }
  if (targetLayer === undefined) return undefined;

  // Node with the nearest order in that layer; ties → lower order, then id.
  let best: KeyboardNavNode | undefined;
  for (const n of nodes) {
    if (n.layer !== targetLayer) continue;
    if (!best) {
      best = n;
      continue;
    }
    const dn = Math.abs(n.order - cur.order);
    const db = Math.abs(best.order - cur.order);
    if (dn < db || (dn === db && compareOrderId(n, best) < 0)) best = n;
  }
  return best;
}

/**
 * Reduces one key event over the current state given the currently *visible*
 * nodes (the host derives `nodes` from `visibleNodes()` / `computePositions()`).
 * Returns the same state reference when nothing changes.
 */
export function reduceMapKey(
  state: MapKeyboardState,
  event: MapKeyEvent,
  nodes: readonly KeyboardNavNode[],
): MapKeyboardState {
  const focused =
    state.focusedId !== null ? nodes.find((n) => n.id === state.focusedId) : undefined;

  switch (event.key) {
    case 'ArrowLeft':
    case 'ArrowRight':
    case 'ArrowUp':
    case 'ArrowDown': {
      // No (valid) focus yet: arrows anchor to the first node.
      if (!focused) {
        const first = firstNode(nodes);
        return first ? { ...state, focusedId: first.id } : state;
      }
      let next: KeyboardNavNode | undefined;
      if (event.key === 'ArrowLeft') next = siblingInLayer(nodes, focused, -1);
      else if (event.key === 'ArrowRight') next = siblingInLayer(nodes, focused, 1);
      else if (event.key === 'ArrowUp') next = nearestAcrossLayers(nodes, focused, -1);
      else next = nearestAcrossLayers(nodes, focused, 1);
      return next ? { ...state, focusedId: next.id } : state;
    }

    case 'Enter': {
      if (!focused) return state;
      if (event.shiftKey) {
        if (!focused.hasChildren) return state;
        const zoom = withExpanded(state.zoom, focused.id);
        return zoom === state.zoom ? state : { ...state, zoom };
      }
      if (state.selectedId === focused.id) return state;
      return { ...state, selectedId: focused.id };
    }

    case 'e':
    case 'E': {
      if (!focused || !focused.hasChildren) return state;
      const zoom = withExpanded(state.zoom, focused.id);
      return zoom === state.zoom ? state : { ...state, zoom };
    }

    case 'Escape': {
      if (state.selectedId !== null) return { ...state, selectedId: null };
      const zoom = zoomOut(state.zoom);
      return zoom === state.zoom ? state : { ...state, zoom };
    }

    default:
      return state;
  }
}
