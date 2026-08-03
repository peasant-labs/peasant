/**
 * Squarified treemap layout (Bruls–Huizing–van Wijk) — the pure geometry behind
 * the change-weight treemap (roadmap 5.3). A pure function of its inputs: same
 * items + rect ⇒ identical tiles, no clock/random (mirrors `changeGraphLayout`).
 *
 * Each tile's area is proportional to its weight; the squarify rule keeps tiles
 * as close to square as possible by filling rows along the shorter side and
 * starting a new row when adding a tile would worsen the row's worst aspect
 * ratio. Determinism: items are sorted by weight descending, ties broken by
 * `key` ascending, so shuffling tied-weight input yields identical output.
 */

export interface TreemapItem {
  key: string;
  /** Non-negative sizing weight (e.g. file churn). */
  weight: number;
}

export interface TreemapTile {
  key: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

interface ScaledItem {
  key: string;
  area: number;
}

interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** Worst (largest) aspect ratio in a row of areas laid along a side of `side`. */
function worstRatio(areas: number[], side: number): number {
  if (areas.length === 0) return Infinity;
  let sum = 0;
  let rmax = -Infinity;
  let rmin = Infinity;
  for (const a of areas) {
    sum += a;
    if (a > rmax) rmax = a;
    if (a < rmin) rmin = a;
  }
  if (sum <= 0 || rmin <= 0) return Infinity;
  const side2 = side * side;
  const sum2 = sum * sum;
  return Math.max((side2 * rmax) / sum2, sum2 / (side2 * rmin));
}

/** Place a committed row into `rect` along its shorter side, returning the tiles
 *  and mutating `rect` to the leftover sub-rectangle. */
function layoutRow(row: ScaledItem[], rect: Rect): TreemapTile[] {
  const rowSum = row.reduce((s, r) => s + r.area, 0);
  const tiles: TreemapTile[] = [];
  if (rect.w <= rect.h) {
    // Horizontal band across the top; tiles split the width.
    const thickness = rowSum / rect.w; // band height
    let x = rect.x;
    for (const r of row) {
      const w = thickness > 0 ? r.area / thickness : 0;
      tiles.push({ key: r.key, x, y: rect.y, w, h: thickness });
      x += w;
    }
    rect.y += thickness;
    rect.h -= thickness;
  } else {
    // Vertical band down the left; tiles split the height.
    const thickness = rowSum / rect.h; // band width
    let y = rect.y;
    for (const r of row) {
      const h = thickness > 0 ? r.area / thickness : 0;
      tiles.push({ key: r.key, x: rect.x, y, w: thickness, h });
      y += h;
    }
    rect.x += thickness;
    rect.w -= thickness;
  }
  return tiles;
}

/**
 * Lay `items` out as a squarified treemap filling [0,0,width,height]. Returns one
 * tile per item (input length preserved). Zero-weight items get zero-size tiles
 * (no area, so they never overlap). When every weight is 0, weights fall back to
 * equal (so a binary-only / no-churn change still tiles legibly). A non-positive
 * width or height yields all-zero tiles (no NaN/Infinity).
 */
export function squarify(items: TreemapItem[], width: number, height: number): TreemapTile[] {
  if (items.length === 0) return [];

  // Deterministic order: weight desc, then key asc.
  const sorted = [...items].sort((a, b) => b.weight - a.weight || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));

  if (width <= 0 || height <= 0) {
    return sorted.map((it) => ({ key: it.key, x: 0, y: 0, w: 0, h: 0 }));
  }

  const totalWeight = sorted.reduce((s, it) => s + Math.max(0, it.weight), 0);
  const totalArea = width * height;
  // All-zero fallback → equal weights.
  const scaled: ScaledItem[] = sorted.map((it) => ({
    key: it.key,
    area:
      totalWeight > 0
        ? (Math.max(0, it.weight) / totalWeight) * totalArea
        : totalArea / sorted.length,
  }));

  // Positive-area items get squarified; zero-area items become zero tiles.
  const positive = scaled.filter((s) => s.area > 0);
  const zero = scaled.filter((s) => s.area <= 0);

  const tiles: TreemapTile[] = [];
  const rect: Rect = { x: 0, y: 0, w: width, h: height };
  let row: ScaledItem[] = [];
  let i = 0;
  while (i < positive.length) {
    const next = positive[i];
    const side = Math.min(rect.w, rect.h);
    const rowAreas = row.map((r) => r.area);
    if (row.length === 0 || worstRatio(rowAreas, side) >= worstRatio([...rowAreas, next.area], side)) {
      row.push(next);
      i += 1;
    } else {
      tiles.push(...layoutRow(row, rect));
      row = [];
    }
  }
  if (row.length > 0) tiles.push(...layoutRow(row, rect));

  for (const z of zero) tiles.push({ key: z.key, x: 0, y: 0, w: 0, h: 0 });
  return tiles;
}
