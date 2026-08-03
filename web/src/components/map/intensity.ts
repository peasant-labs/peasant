/**
 * Monochrome intensity ramp helpers from DESIGN_SYSTEM.md.
 *
 * The `bg-intensity-0..4` / `border-intensity-*` utilities are backed by the
 * `--intensity-0..4` tokens (fill/text/border ramp, light + dark) added to
 * `globals.css` as design tokens. This module owns the *mapping*
 * from data (traceability ratio, effort density, sessions/day) onto ramp
 * levels and class names — all quantization is pure and deterministic.
 */

export type IntensityLevel = 0 | 1 | 2 | 3 | 4;

/** Intensity-ramp fill classes (ActivityHeatmap's level-class approach). */
export const INTENSITY_BG: Record<IntensityLevel, string> = {
  0: 'bg-intensity-0',
  1: 'bg-intensity-1',
  2: 'bg-intensity-2',
  3: 'bg-intensity-3',
  4: 'bg-intensity-4',
};

/**
 * Node fill by traceability level. Fill shade is owned by
 * traceability coverage, always). Fully recorded code keeps the elevated
 * surface; lower coverage dims stepwise toward the page canvas — text and
 * border carry the lower steps.
 *
 * The fill ladder deliberately stays on the *surface* tokens, never the
 * `intensity-1..4` ramp: in dark mode the intensity ramp ascends toward
 * white ink (intensity-0 is brighter than surface-elev), so any intensity
 * fill under ink text both inverts the dimming direction and fails WCAG AA.
 * surface-elev → surface → canvas is the only token chain that dims
 * monotonically in BOTH themes (light: 100% → 100% → 98% L; dark: 12% →
 * 8% → 4% L) while keeping every assigned text ink ≥ 4.5:1.
 */
export const NODE_FILL: Record<IntensityLevel, string> = {
  4: 'bg-surface-elev',
  3: 'bg-surface',
  2: 'bg-surface',
  1: 'bg-canvas',
  0: 'bg-canvas',
};

/**
 * Node name ink by traceability level. Dimmed labels stop at `intensity-3`
 * to preserve text contrast — never `ink-4`. Contrast
 * against the assigned NODE_FILL (light / dark): level 4 = 19.1 / 15.9,
 * level 3 = 8.5 / 10.8, level 2 = 4.9 / 6.1, levels 1–0 = 4.7 / 6.5.
 */
export const NODE_TEXT: Record<IntensityLevel, string> = {
  4: 'text-ink',
  3: 'text-ink-2',
  2: 'text-intensity-3',
  1: 'text-intensity-3',
  0: 'text-intensity-3',
};

/**
 * Count-row ink (quieter than the name: one ink step down at the top of the
 * ramp, same `intensity-3` floor below — the 10px mono size recedes it).
 * Contrast against the assigned NODE_FILL (light / dark): level 4 =
 * 4.6 / 5.5, level 3 = 4.6 / 6.1, level 2 = 4.9 / 6.1, levels 1–0 = 4.7 / 6.5.
 */
export const NODE_COUNT_TEXT: Record<IntensityLevel, string> = {
  4: 'text-ink-3',
  3: 'text-ink-3',
  2: 'text-intensity-3',
  1: 'text-intensity-3',
  0: 'text-intensity-3',
};

/**
 * Node border by traceability level. Recorded code keeps the hairline rule;
 * dark matter's faint border is `--intensity-1`, never an
 * ad-hoc opacity.
 */
export const NODE_BORDER: Record<IntensityLevel, string> = {
  4: 'border-rule',
  3: 'border-rule',
  2: 'border-rule',
  1: 'border-intensity-1',
  0: 'border-intensity-1',
};

/**
 * Traceability coverage → ramp level. `recordedFiles / totalFiles`:
 * 0 (or unknown total) is dark matter; ≥ 0.9 reads as fully recorded.
 */
export function traceabilityLevel(recordedFiles: number, totalFiles: number): IntensityLevel {
  if (totalFiles <= 0 || recordedFiles <= 0) return 0;
  const ratio = Math.min(recordedFiles / totalFiles, 1);
  if (ratio < 0.25) return 1;
  if (ratio < 0.5) return 2;
  if (ratio < 0.9) return 3;
  return 4;
}

/** Effort density (0..1) → ramp level for the bottom-edge intensity bar. */
export function effortLevel(density: number): IntensityLevel {
  if (density <= 0) return 0;
  const d = Math.min(density, 1);
  if (d <= 0.25) return 1;
  if (d <= 0.5) return 2;
  if (d <= 0.75) return 3;
  return 4;
}

/**
 * Generic value-vs-max quantization (sessions/day sparkline). Zero values are
 * level 0; positive values split the ramp by quartile of the observed max.
 */
export function quantizeLevel(value: number, max: number): IntensityLevel {
  if (value <= 0 || max <= 0) return 0;
  const ratio = Math.min(value / max, 1);
  if (ratio <= 0.25) return 1;
  if (ratio <= 0.5) return 2;
  if (ratio <= 0.75) return 3;
  return 4;
}
