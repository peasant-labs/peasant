import { describe, it, expect } from 'vitest';
import { squarify, type TreemapItem, type TreemapTile } from './changeTreemapLayout';

const W = 400;
const H = 200;

function area(t: TreemapTile): number {
  return t.w * t.h;
}

/** Overlap area between two axis-aligned rects (0 when disjoint). */
function overlap(a: TreemapTile, b: TreemapTile): number {
  const ix = Math.max(0, Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x));
  const iy = Math.max(0, Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y));
  return ix * iy;
}

describe('squarify', () => {
  it('returns [] for no items', () => {
    expect(squarify([], W, H)).toEqual([]);
  });

  it('gives a single item the full rect', () => {
    const tiles = squarify([{ key: 'a', weight: 7 }], W, H);
    expect(tiles).toHaveLength(1);
    expect(tiles[0]).toMatchObject({ key: 'a', x: 0, y: 0, w: W, h: H });
  });

  it('tiles the whole rect with no gaps and areas proportional to weight', () => {
    const items: TreemapItem[] = [
      { key: 'a', weight: 50 },
      { key: 'b', weight: 30 },
      { key: 'c', weight: 15 },
      { key: 'd', weight: 5 },
    ];
    const tiles = squarify(items, W, H);
    expect(tiles).toHaveLength(4);

    const total = W * H;
    const sum = tiles.reduce((s, t) => s + area(t), 0);
    expect(sum).toBeCloseTo(total, 4); // exactly tiles the rect

    const byKey = Object.fromEntries(tiles.map((t) => [t.key, t]));
    const totalWeight = 100;
    for (const it of items) {
      expect(area(byKey[it.key])).toBeCloseTo((it.weight / totalWeight) * total, 2);
    }
  });

  it('produces no overlapping tiles', () => {
    const tiles = squarify(
      [
        { key: 'a', weight: 9 },
        { key: 'b', weight: 7 },
        { key: 'c', weight: 5 },
        { key: 'd', weight: 3 },
        { key: 'e', weight: 1 },
      ],
      W,
      H,
    );
    for (let i = 0; i < tiles.length; i++) {
      for (let j = i + 1; j < tiles.length; j++) {
        expect(overlap(tiles[i], tiles[j])).toBeCloseTo(0, 4);
      }
    }
  });

  it('is deterministic — shuffling tied-weight input yields identical layout', () => {
    const a: TreemapItem[] = [
      { key: 'x', weight: 5 },
      { key: 'y', weight: 5 },
      { key: 'z', weight: 5 },
    ];
    const b: TreemapItem[] = [a[2], a[0], a[1]];
    expect(squarify(b, W, H)).toEqual(squarify(a, W, H));
  });

  it('keeps a dominant tile near-square, not a sliver', () => {
    const tiles = squarify(
      [
        { key: 'big', weight: 80 },
        { key: 's1', weight: 5 },
        { key: 's2', weight: 5 },
        { key: 's3', weight: 5 },
        { key: 's4', weight: 5 },
      ],
      300,
      300,
    );
    const big = tiles.find((t) => t.key === 'big')!;
    const ratio = Math.max(big.w / big.h, big.h / big.w);
    expect(ratio).toBeLessThan(2); // not a thin strip
  });

  it('falls back to equal areas when every weight is 0', () => {
    const tiles = squarify(
      [
        { key: 'a', weight: 0 },
        { key: 'b', weight: 0 },
        { key: 'c', weight: 0 },
      ],
      W,
      H,
    );
    const total = W * H;
    for (const t of tiles) expect(area(t)).toBeCloseTo(total / 3, 2);
  });

  it('gives zero-weight items zero-size tiles among positive ones (no NaN)', () => {
    const tiles = squarify(
      [
        { key: 'a', weight: 10 },
        { key: 'zero', weight: 0 },
        { key: 'b', weight: 6 },
      ],
      W,
      H,
    );
    const byKey = Object.fromEntries(tiles.map((t) => [t.key, t]));
    expect(area(byKey['zero'])).toBe(0);
    for (const t of tiles) {
      expect(Number.isFinite(t.x) && Number.isFinite(t.y)).toBe(true);
      expect(Number.isFinite(t.w) && Number.isFinite(t.h)).toBe(true);
    }
    // The two positive tiles still tile the whole rect.
    expect(area(byKey['a']) + area(byKey['b'])).toBeCloseTo(W * H, 2);
  });

  it('returns all-zero tiles (no NaN/Infinity) for a non-positive rect', () => {
    const tiles = squarify([{ key: 'a', weight: 5 }, { key: 'b', weight: 3 }], 0, H);
    expect(tiles).toHaveLength(2);
    for (const t of tiles) {
      expect(t).toMatchObject({ x: 0, y: 0, w: 0, h: 0 });
    }
  });
});
