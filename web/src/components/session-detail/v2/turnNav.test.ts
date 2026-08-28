import { describe, it, expect } from 'vitest';
import { nextNavTurn } from '@peasant-labs/fairtrade/ui';

// Vim-style j/k turn navigation (roadmap 4.1). The DOM glue (keydown + scroll)
// lives in the sibling viewer package; the pure index math is exported and
// tested here in peasant's harness. Turn indices are display order (not
// necessarily contiguous — filtered/scoped views skip turns).

describe('nextNavTurn', () => {
  const turns = [0, 2, 5, 6, 9];

  it('moves to the next/previous turn in display order', () => {
    expect(nextNavTurn(turns, 2, 1)).toBe(5);
    expect(nextNavTurn(turns, 5, -1)).toBe(2);
  });

  it('clamps at the ends (no wrap)', () => {
    expect(nextNavTurn(turns, 9, 1)).toBe(9); // already last
    expect(nextNavTurn(turns, 0, -1)).toBe(0); // already first
  });

  it('snaps to the first turn going down / last going up when no anchor', () => {
    expect(nextNavTurn(turns, undefined, 1)).toBe(0);
    expect(nextNavTurn(turns, undefined, -1)).toBe(9);
  });

  it('snaps to an end when the anchor is not a known turn', () => {
    expect(nextNavTurn(turns, 3, 1)).toBe(0); // 3 absent -> first (down)
    expect(nextNavTurn(turns, 3, -1)).toBe(9); // 3 absent -> last (up)
  });

  it('returns undefined for an empty turn list', () => {
    expect(nextNavTurn([], undefined, 1)).toBeUndefined();
    expect(nextNavTurn([], 5, -1)).toBeUndefined();
  });
});
