import { describe, it, expect } from 'vitest';
import {
  isMapKey,
  reduceMapKey,
  withExpanded,
  zoomOut,
  type MapKeyboardState,
} from './keyboard';
import type { MapZoom } from './types';
import { NAV_NODES } from './test-fixtures';

function state(partial: Partial<MapKeyboardState> = {}): MapKeyboardState {
  return {
    focusedId: null,
    selectedId: null,
    zoom: { level: 'project', expanded: new Set<string>() },
    ...partial,
  };
}

describe('isMapKey', () => {
  it('claims exactly the canvas-navigation keys', () => {
    for (const key of ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Enter', 'e', 'E', 'Escape']) {
      expect(isMapKey(key)).toBe(true);
    }
    expect(isMapKey('Tab')).toBe(false);
    expect(isMapKey(' ')).toBe(false);
  });
});

describe('arrow navigation', () => {
  it('anchors to the first node (lowest layer, lowest order) when nothing is focused', () => {
    const next = reduceMapKey(state(), { key: 'ArrowRight' }, NAV_NODES);
    expect(next.focusedId).toBe('a0');
  });

  it('re-anchors to the first node when the focused node is no longer visible', () => {
    const next = reduceMapKey(state({ focusedId: 'gone' }), { key: 'ArrowDown' }, NAV_NODES);
    expect(next.focusedId).toBe('a0');
  });

  it('moves within the layer by order with ArrowRight/ArrowLeft', () => {
    let s = state({ focusedId: 'a0' });
    s = reduceMapKey(s, { key: 'ArrowRight' }, NAV_NODES);
    expect(s.focusedId).toBe('a1');
    s = reduceMapKey(s, { key: 'ArrowRight' }, NAV_NODES);
    expect(s.focusedId).toBe('a2');
    s = reduceMapKey(s, { key: 'ArrowLeft' }, NAV_NODES);
    expect(s.focusedId).toBe('a1');
  });

  it('is a no-op (same reference) at the row edges', () => {
    const atStart = state({ focusedId: 'a0' });
    expect(reduceMapKey(atStart, { key: 'ArrowLeft' }, NAV_NODES)).toBe(atStart);
    const atEnd = state({ focusedId: 'a2' });
    expect(reduceMapKey(atEnd, { key: 'ArrowRight' }, NAV_NODES)).toBe(atEnd);
  });

  it('moves across layers to the nearest order, ties resolving to the lower order', () => {
    // a1 (order 2) ↓ layer 1: b0 (order 1) and b1 (order 3) tie → b0.
    const down = reduceMapKey(state({ focusedId: 'a1' }), { key: 'ArrowDown' }, NAV_NODES);
    expect(down.focusedId).toBe('b0');
    // b0 (order 1) ↑ layer 0: a0 (order 0) and a1 (order 2) tie → a0.
    const up = reduceMapKey(state({ focusedId: 'b0' }), { key: 'ArrowUp' }, NAV_NODES);
    expect(up.focusedId).toBe('a0');
  });

  it('skips empty layers to the nearest one in that direction', () => {
    // Layer 2 is empty: b1 ↓ lands on layer 3.
    const down = reduceMapKey(state({ focusedId: 'b1' }), { key: 'ArrowDown' }, NAV_NODES);
    expect(down.focusedId).toBe('c0');
    const up = reduceMapKey(state({ focusedId: 'c0' }), { key: 'ArrowUp' }, NAV_NODES);
    expect(up.focusedId).toBe('b0');
  });

  it('is a no-op at the top and bottom layers', () => {
    const top = state({ focusedId: 'a0' });
    expect(reduceMapKey(top, { key: 'ArrowUp' }, NAV_NODES)).toBe(top);
    const bottom = state({ focusedId: 'c0' });
    expect(reduceMapKey(bottom, { key: 'ArrowDown' }, NAV_NODES)).toBe(bottom);
  });
});

describe('Enter — select', () => {
  it('selects the focused node', () => {
    const next = reduceMapKey(state({ focusedId: 'b0' }), { key: 'Enter' }, NAV_NODES);
    expect(next.selectedId).toBe('b0');
    expect(next.focusedId).toBe('b0');
  });

  it('is a no-op without focus or when already selected', () => {
    const unfocused = state();
    expect(reduceMapKey(unfocused, { key: 'Enter' }, NAV_NODES)).toBe(unfocused);
    const selected = state({ focusedId: 'b0', selectedId: 'b0' });
    expect(reduceMapKey(selected, { key: 'Enter' }, NAV_NODES)).toBe(selected);
  });
});

describe('Shift+Enter / E — expand (zoom-in)', () => {
  it('expands the focused node with Shift+Enter', () => {
    const next = reduceMapKey(
      state({ focusedId: 'a0' }),
      { key: 'Enter', shiftKey: true },
      NAV_NODES,
    );
    expect(next.zoom.expanded.has('a0')).toBe(true);
    expect(next.selectedId).toBeNull(); // expand is not select
  });

  it('expands with the E key (either case)', () => {
    for (const key of ['e', 'E']) {
      const next = reduceMapKey(state({ focusedId: 'b0' }), { key }, NAV_NODES);
      expect(next.zoom.expanded.has('b0')).toBe(true);
    }
  });

  it('is a no-op on nodes without children', () => {
    const s = state({ focusedId: 'a1' });
    expect(reduceMapKey(s, { key: 'Enter', shiftKey: true }, NAV_NODES)).toBe(s);
    expect(reduceMapKey(s, { key: 'e' }, NAV_NODES)).toBe(s);
  });

  it('does not mutate the previous expanded set', () => {
    const s = state({ focusedId: 'a0' });
    const next = reduceMapKey(s, { key: 'e' }, NAV_NODES);
    expect(s.zoom.expanded.has('a0')).toBe(false);
    expect(next.zoom.expanded).not.toBe(s.zoom.expanded);
  });
});

describe('Escape — deselect, then zoom out', () => {
  it('clears the selection first, leaving zoom untouched', () => {
    const zoom: MapZoom = { level: 'file', expanded: new Set(['a0']) };
    const next = reduceMapKey(
      state({ focusedId: 'b0', selectedId: 'b0', zoom }),
      { key: 'Escape' },
      NAV_NODES,
    );
    expect(next.selectedId).toBeNull();
    expect(next.zoom).toBe(zoom);
  });

  it('then collapses the most recent expansion', () => {
    let zoom: MapZoom = { level: 'project', expanded: new Set<string>() };
    zoom = withExpanded(zoom, 'a0');
    zoom = withExpanded(zoom, 'b0');
    const next = reduceMapKey(state({ zoom }), { key: 'Escape' }, NAV_NODES);
    expect(Array.from(next.zoom.expanded)).toEqual(['a0']);
  });

  it('then steps the zoom level down one level at a time', () => {
    const fromFile = reduceMapKey(
      state({ zoom: { level: 'file', expanded: new Set() } }),
      { key: 'Escape' },
      NAV_NODES,
    );
    expect(fromFile.zoom.level).toBe('package');
    const fromPackage = reduceMapKey(fromFile, { key: 'Escape' }, NAV_NODES);
    expect(fromPackage.zoom.level).toBe('project');
  });

  it('is a no-op when fully zoomed out with nothing selected', () => {
    const s = state();
    expect(reduceMapKey(s, { key: 'Escape' }, NAV_NODES)).toBe(s);
  });
});

describe('zoom helpers', () => {
  it('withExpanded is idempotent per id', () => {
    const zoom: MapZoom = { level: 'project', expanded: new Set(['a0']) };
    expect(withExpanded(zoom, 'a0')).toBe(zoom);
  });

  it('zoomOut returns the same reference at the outermost state', () => {
    const zoom: MapZoom = { level: 'project', expanded: new Set<string>() };
    expect(zoomOut(zoom)).toBe(zoom);
  });
});

describe('unhandled keys', () => {
  it('returns the same state reference', () => {
    const s = state({ focusedId: 'a0' });
    expect(reduceMapKey(s, { key: 'Tab' }, NAV_NODES)).toBe(s);
  });
});
