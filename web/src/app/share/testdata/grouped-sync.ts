/**
 * Test-only builder for the grouped sync chooser payload
 * (GET /api/v1/sync/sessions?view=grouped). It lets each mounted-share test
 * keep its own session fixture while the production surface consumes the real
 * wire shape: one ordered item list of transcript rows (with their Sync mirror
 * and collapsed helper groups) and helper-only context containers.
 *
 * This is test support, not a production code path and not a case table; the
 * combinatorial cases live in the YAML fixtures.
 */

export interface GroupedSyncSessionSpec {
  id: string;
  harness: string;
  startTime: string;
  durationMins: number;
  totalTokens: number;
  turnCount: number;
  toolCallCount?: number;
  project?: string;
  projectHash: string;
  preview?: string;
  outcome?: string;
  inputSubmissionCount?: number;
  /** Sync status menu: new / updated / synced / held. Defaults to new. */
  syncStatus?: 'new' | 'updated' | 'synced' | 'held';
  hostSlug?: string;
  model?: string;
  helperGroups?: GroupedSyncHelperGroupSpec[];
}

export interface GroupedSyncHelperGroupSpec {
  groupId: string;
  helperThreadCount: number;
  memberScope: string;
}

export interface GroupedSyncContextSpec {
  groupId: string;
  ownerStatus: string;
  helperGroups: GroupedSyncHelperGroupSpec[];
}

function transcriptItem(session: GroupedSyncSessionSpec) {
  return {
    kind: 'transcript' as const,
    transcript: {
      session: {
        id: session.id,
        harness: session.harness,
        startTime: session.startTime,
        durationMins: session.durationMins,
        totalTokens: session.totalTokens,
        turnCount: session.turnCount,
        toolCallCount: session.toolCallCount ?? 0,
        project: session.project,
        projectHash: session.projectHash,
        preview: session.preview,
        outcome: session.outcome,
        inputSubmissionCount: session.inputSubmissionCount,
      },
      sync: {
        id: session.id,
        harness: session.harness,
        projectName: session.project ?? 'unknown project',
        projectHash: session.projectHash,
        hostSlug: session.hostSlug ?? 'fixture-host',
        startTime: session.startTime,
        durationMs: Math.round(session.durationMins * 60000),
        totalTokens: session.totalTokens,
        turnCount: session.turnCount,
        model: session.model ?? 'fixture-model',
        inputSubmissionCount: session.inputSubmissionCount,
        syncStatus: session.syncStatus ?? 'new',
      },
    },
    helperGroups: session.helperGroups ?? [],
  };
}

export function buildGroupedSyncResponse(
  sessions: GroupedSyncSessionSpec[],
  contexts: GroupedSyncContextSpec[] = [],
) {
  const items = [
    ...sessions.map(transcriptItem),
    ...contexts.map((context) => ({
      kind: 'context_container' as const,
      context: { groupId: context.groupId, ownerStatus: context.ownerStatus },
      helperGroups: context.helperGroups,
    })),
  ];
  return {
    items,
    page: 1,
    limit: items.length || 1,
    totalItems: items.length,
    ordinarySessionTotal: sessions.length,
    helperThreadTotal: sessions.reduce((total, session) => total + (session.helperGroups?.reduce((count, group) => count + group.helperThreadCount, 0) ?? 0), 0),
  };
}
