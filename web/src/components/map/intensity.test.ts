import { describe, it, expect } from 'vitest';
import {
  NODE_BORDER,
  NODE_COUNT_TEXT,
  NODE_FILL,
  NODE_TEXT,
  effortLevel,
  quantizeLevel,
  traceabilityLevel,
  type IntensityLevel,
} from './intensity';

describe('traceabilityLevel', () => {
  // Boundary table: [recordedFiles, totalFiles, expected level].
  const cases: Array<[number, number, IntensityLevel, string]> = [
    [0, 0, 0, 'unknown total is dark matter'],
    [5, 0, 0, 'unknown total wins over recorded count'],
    [5, -1, 0, 'negative total is dark matter'],
    [0, 10, 0, 'zero recorded is dark matter'],
    [-1, 10, 0, 'negative recorded is dark matter'],
    [1, 10, 1, '0.1 → faint'],
    [24, 100, 1, 'just below the 0.25 boundary'],
    [25, 100, 2, 'exactly 0.25 steps up'],
    [49, 100, 2, 'just below the 0.5 boundary'],
    [50, 100, 3, 'exactly 0.5 steps up'],
    [89, 100, 3, 'just below the 0.9 boundary'],
    [90, 100, 4, 'exactly 0.9 reads as fully recorded'],
    [9, 10, 4, '0.9 boundary at small denominators'],
    [100, 100, 4, 'full coverage'],
    [110, 100, 4, 'over-coverage clamps to 4'],
  ];
  it.each(cases)('(%d, %d) → %d (%s)', (recorded, total, expected) => {
    expect(traceabilityLevel(recorded, total)).toBe(expected);
  });
});

describe('effortLevel', () => {
  const cases: Array<[number, IntensityLevel, string]> = [
    [-0.5, 0, 'negative density is empty'],
    [0, 0, 'zero density is empty'],
    [0.1, 1, 'low density'],
    [0.25, 1, 'inclusive 0.25 boundary stays 1'],
    [0.26, 2, 'just above 0.25'],
    [0.5, 2, 'inclusive 0.5 boundary stays 2'],
    [0.51, 3, 'just above 0.5'],
    [0.75, 3, 'inclusive 0.75 boundary stays 3'],
    [0.76, 4, 'just above 0.75'],
    [1, 4, 'full density'],
    [2, 4, 'over-density clamps to 4'],
  ];
  it.each(cases)('(%f) → %d (%s)', (density, expected) => {
    expect(effortLevel(density)).toBe(expected);
  });
});

describe('quantizeLevel', () => {
  const cases: Array<[number, number, IntensityLevel, string]> = [
    [0, 5, 0, 'zero value is empty'],
    [-1, 5, 0, 'negative value is empty'],
    [3, 0, 0, 'zero max is empty'],
    [1, 4, 1, 'inclusive first quartile'],
    [2, 4, 2, 'inclusive second quartile'],
    [3, 4, 3, 'inclusive third quartile'],
    [4, 4, 4, 'max value is full'],
    [5, 4, 4, 'over-max clamps to 4'],
    [1, 8, 1, 'below first quartile'],
  ];
  it.each(cases)('(%d, %d) → %d (%s)', (value, max, expected) => {
    expect(quantizeLevel(value, max)).toBe(expected);
  });
});

describe('node class ladders', () => {
  it('fill dims monotonically from surface-elev toward canvas — never an intensity fill under ink text', () => {
    expect(NODE_FILL).toEqual({
      4: 'bg-surface-elev',
      3: 'bg-surface',
      2: 'bg-surface',
      1: 'bg-canvas',
      0: 'bg-canvas',
    });
  });

  it('dimmed name ink stops at the text-safe intensity-3 token', () => {
    expect(NODE_TEXT).toEqual({
      4: 'text-ink',
      3: 'text-ink-2',
      2: 'text-intensity-3',
      1: 'text-intensity-3',
      0: 'text-intensity-3',
    });
    for (const cls of Object.values(NODE_TEXT)) expect(cls).not.toBe('text-ink-4');
  });

  it('count-row ink stops at intensity-3 — never ink-4', () => {
    expect(NODE_COUNT_TEXT).toEqual({
      4: 'text-ink-3',
      3: 'text-ink-3',
      2: 'text-intensity-3',
      1: 'text-intensity-3',
      0: 'text-intensity-3',
    });
    for (const cls of Object.values(NODE_COUNT_TEXT)) expect(cls).not.toBe('text-ink-4');
  });

  it("dark matter's border is intensity-1; recorded code keeps the hairline rule", () => {
    expect(NODE_BORDER).toEqual({
      4: 'border-rule',
      3: 'border-rule',
      2: 'border-rule',
      1: 'border-intensity-1',
      0: 'border-intensity-1',
    });
  });
});
