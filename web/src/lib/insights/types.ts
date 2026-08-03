/**
 * Types for the insight computation pipeline.
 * Used by the session detail viewer.
 */

// ---------------------------------------------------------------------------
// Phase Detection
// ---------------------------------------------------------------------------

export type PhaseType =
  | "planning"
  | "exploration"
  | "implementation"
  | "testing"
  | "error"
  | "debug"
  | "retry-loop"
  | "user-correction"
  | "recovery"
  | "abandonment";

export interface Phase {
  type: PhaseType;
  startTurn: number;
  endTurn: number;
  /** Badges for absorbed micro-phases (e.g., "1 error"). */
  badges: PhaseBadge[];
}

export interface PhaseBadge {
  type: PhaseType;
  count: number;
}
