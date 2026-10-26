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
  repositoryLocationId: string;
  branch: string;
  /**
   * Collapsed helper groups this owner row anchors, from the grouped sync
   * route. Empty or absent for an ordinary session. Each group carries its own
   * opaque member scope; membership is fetched through the member operation,
   * never inferred here.
   */
  helperGroups?: ShareHelperGroup[];
}

/** One collapsed helper group on the grouped chooser route. */
export interface ShareHelperGroup {
  groupId: string;
  /** Saved helper threads, never reviews or messages. */
  helperThreadCount: number;
  /** Opaque originating-route scope; a member fetch replays it, and it is not
   * an access grant. */
  memberScope: string;
}

/**
 * A helper-only grouped result: an owner context that is not an ordinary
 * candidate. It carries no fake owner title and no owner action, and it is not
 * a way to select the hidden owner.
 */
export interface ShareHelperContext {
  groupId: string;
  ownerStatus: string;
  helperGroups: ShareHelperGroup[];
}

export interface ShareBranchGroup {
  branch: string;
  sessions: ShareHierarchySession[];
}

export interface ShareLocationGroup {
  repositoryLocationId: string;
  locationLabel: string;
  branches: ShareBranchGroup[];
}

export interface ShareHierarchyProject {
  key: string;
  projectName: string;
  locations: ShareLocationGroup[];
}

export interface ShareDiscoveryResult<TSession extends ShareSession = ShareSession> {
  sessions: TSession[];
  counts: Record<ShareStatus, number>;
  /** Helper-only grouped results that name no ordinary owner row. */
  helperContexts?: ShareHelperContext[];
}

/**
 * A project is the primary unit of contribution. Sessions group into projects
 * by the canonical `projectHash`; `projectName` is display text only. Derived purely from sessions —
 * see {@link groupByProject}.
 */
export interface ShareProject {
  projectName: string;
  projectHash: string;
  /** Stable key for React lists and selection — the canonical project hash. */
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
