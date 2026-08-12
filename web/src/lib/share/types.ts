// Share-specific types shared between components and mock data.

export type ShareStatus = 'new' | 'updated' | 'shared' | 'held' | 'error' | 'pushing';

export interface ShareSession {
  id: string;
  provider: 'claude-code' | 'opencode' | 'gemini-cli' | 'codex';
  projectName: string;
  projectHash: string;
  hostSlug: string;
  startTime: string;
  durationMins: number;
  totalTokens: number;
  turnCount: number;
  model: string;
  shareStatus: ShareStatus;
  /**
   * Raw first user message of the session, sourced from the already-redacted
   * indexed transcript (`SessionSummary.preview`). Redaction-safe. Empty when
   * the session has no indexed user entry. Formatted for display by
   * `summarizePrompt()`.
   */
  preview: string;
  /**
   * Heuristic session outcome (`resolved`/`partial`/`failed`), sourced from
   * `SessionSummary.outcome` (Go). Empty when no outcome was computed.
   */
  outcome?: string;
}

export interface ShareHierarchySession extends ShareSession {
  locationLabel: string;
  branch: string;
}

export interface ShareBranchGroup {
  branch: string;
  sessions: ShareHierarchySession[];
}

export interface ShareLocationGroup {
  locationLabel: string;
  branches: ShareBranchGroup[];
}

export interface ShareHierarchyProject {
  key: string;
  projectName: string;
  locations: ShareLocationGroup[];
}

export interface ShareDiscoveryResult {
  sessions: ShareSession[];
  counts: Record<ShareStatus, number>;
}

/**
 * A project is the primary unit of contribution. Sessions group into projects
 * by `projectHash` (falling back to `projectName` when the hash is empty, as
 * the real backend doesn't yet emit a hash). Derived purely from sessions —
 * see {@link groupByProject}.
 */
export interface ShareProject {
  projectName: string;
  projectHash: string;
  /** Stable key for React lists and selection — hash if present, else name. */
  key: string;
  sessions: ShareSession[];
  sessionCount: number;
  /** Sessions eligible to contribute (new or updated). */
  selectableCount: number;
  totalTokens: number;
  /** Earliest/latest session start time across the project. */
  dateRange: { start: string; end: string };
  /** How many sessions sit in each share status. */
  statusRollup: Record<ShareStatus, number>;
}

// ---------------------------------------------------------------------------
// Annotation-based label selection (the real, push-bound model)
// ---------------------------------------------------------------------------

/**
 * How an annotation came to exist, mapped from the schema `annotatorKind`:
 *   - `auto`   — produced by a rule or an agent at ingest time (`rule`/`agent`).
 *   - `manual` — produced by a human (`human`).
 * The Labels step groups annotations by this distinction.
 */
export type LabelOrigin = 'auto' | 'manual';

/**
 * One selectable label for the share wizard. Sourced from a real annotation
 * (`AnnotationSummary` from GET /api/v1/annotations), distilled to the fields
 * the wizard needs to display and to identify it on the push.
 */
export interface ShareLabel {
  /** Annotation store id — the load-bearing key the push selection matches on. */
  id: string;
  /** Owning session id. */
  sessionId: string;
  /** auto (rule/agent) vs manual (human) — drives the grouped UI. */
  origin: LabelOrigin;
  /** Raw annotatorKind from the wire (`rule` | `agent` | `human`). */
  annotatorKind: string;
  /** Annotator name (e.g. `outcome-classifier`, `human-web`) — shown on hover. */
  annotatorName: string;
  /** Type id, e.g. `quality.outcome`. */
  typeId: string;
  /** Human-readable type name when present, else the typeId. */
  typeName: string;
  /** The annotation value, e.g. `success`, `2`. */
  value: string;
}

/**
 * The labels (annotations) the user chose to include in the push, keyed by
 * session id. Reported upward from the Labels step and surfaced in Submit.
 * The flattened `includedIds` is what flows into the push selection.
 */
export interface LabelSelection {
  /** All discovered labels per session (selected or not). */
  bySession: Map<string, ShareLabel[]>;
  /** The set of annotation ids the user kept (default: every discovered id). */
  includedIds: Set<string>;
}

/** An empty selection — nothing discovered, nothing chosen. */
export function emptyLabelSelection(): LabelSelection {
  return { bySession: new Map(), includedIds: new Set() };
}

/** Map a schema annotatorKind to the wizard's auto/manual grouping. */
export function originForAnnotatorKind(annotatorKind: string): LabelOrigin {
  return annotatorKind === 'human' ? 'manual' : 'auto';
}
