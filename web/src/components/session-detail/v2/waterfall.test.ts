import { describe, it, expect } from 'vitest';
import { buildTaskWaterfall, type TaskGroup } from '@peasant-labs/fairtrade/ui';

// Steps waterfall (roadmap 5.2): the duration-lane geometry is a pure transform
// over the package's TaskGroup data (computeTasks). It's exported and tested here
// in peasant's harness; the render is the thin viewer half.

// Minimal TaskGroup for the geometry under test (only promptEntryIndex +
// durationMs matter to buildTaskWaterfall).
function task(promptEntryIndex: number, durationMs: number): TaskGroup {
  return { promptEntryIndex, durationMs } as TaskGroup;
}

describe('buildTaskWaterfall', () => {
  it('returns [] for no tasks', () => {
    expect(buildTaskWaterfall([])).toEqual([]);
  });

  it('sizes each segment by its share of total duration and tiles the lane', () => {
    const segs = buildTaskWaterfall([task(0, 1000), task(4, 3000)]);
    expect(segs).toHaveLength(2);
    expect(segs[0]).toMatchObject({ promptEntryIndex: 0, offsetPct: 0, widthPct: 25 });
    expect(segs[1]).toMatchObject({ promptEntryIndex: 4, offsetPct: 25, widthPct: 75 });
    // Segments tile the lane back-to-back: last offset + width === 100.
    expect(segs[1].offsetPct + segs[1].widthPct).toBeCloseTo(100, 6);
  });

  it('accumulates offsets across many tasks', () => {
    const segs = buildTaskWaterfall([task(0, 2000), task(2, 2000), task(5, 6000)]);
    expect(segs.map((s) => Math.round(s.offsetPct))).toEqual([0, 20, 40]);
    expect(segs.map((s) => Math.round(s.widthPct))).toEqual([20, 20, 60]);
  });

  it('clamps negative durations and still tiles correctly', () => {
    const segs = buildTaskWaterfall([task(0, -500), task(1, 1000)]);
    expect(segs[0]).toMatchObject({ durationMs: 0, widthPct: 0 });
    expect(segs[1]).toMatchObject({ widthPct: 100 });
  });

  it('returns all-zero geometry for an untimed transcript (total 0)', () => {
    const segs = buildTaskWaterfall([task(0, 0), task(3, 0)]);
    for (const s of segs) {
      expect(s.offsetPct).toBe(0);
      expect(s.widthPct).toBe(0);
    }
  });
});
