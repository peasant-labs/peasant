/**
 * Test-only builder for the grouped sync chooser payloads
 * (GET /api/v1/sync/sessions?view=grouped and the scoped
 * GET /api/v1/session-groups/{groupId}/members). It lets each mounted-share
 * test keep its own session fixture while the production surface consumes the
 * real wire shape: ordered transcript rows (with their Sync mirror and
 * collapsed helper groups), helper-only context containers, and member pages
 * that may themselves anchor further nested groups.
 *
 * Fixture tables name projects by short readable keys. The wire requires the
 * canonical 64-character lowercase project digest, so the builder maps each key
 * to a deterministic digest instead of emitting a non-contract literal. The
 * production decoder still validates every emitted payload.
 *
 * This is test support, not a production code path and not a case table; the
 * combinatorial cases live in the YAML fixtures.
 */

import { createHash } from 'node:crypto';

export type GroupedSyncPurpose = 'unknown' | 'interaction' | 'delegated_work' | 'helper_review';
export type GroupedSyncStatus = 'new' | 'updated' | 'synced' | 'held';
export type GroupedSyncOwnerStatus =
  | 'unknown'
  | 'resolved'
  | 'general_link_only'
  | 'known_unavailable'
  | 'inaccessible'
  | 'conflicting';

export interface GroupedSyncSessionSpec {
  id: string;
  harness: string;
  startTime: string;
  durationMins: number;
  totalTokens: number;
  turnCount: number;
  toolCallCount?: number;
  project?: string;
  /** Symbolic project key, or an already-canonical 64-character digest. */
  projectHash: string;
  preview?: string;
  outcome?: string;
  inputSubmissionCount?: number;
  /** Sync status menu: new / updated / synced / held. Defaults to new. */
  syncStatus?: GroupedSyncStatus;
  hostSlug?: string;
  model?: string;
  helperGroups?: GroupedSyncHelperGroupSpec[];
}

export interface GroupedSyncHelperGroupSpec {
  groupId: string;
  helperThreadCount: number;
  memberScope: string;
  /** Only helper_review groups are rendered by the chooser. Defaults to it. */
  purpose?: GroupedSyncPurpose;
}

export interface GroupedSyncContextSpec {
  groupId: string;
  ownerStatus: GroupedSyncOwnerStatus;
  helperGroups: GroupedSyncHelperGroupSpec[];
}

/**
 * Map a fixture project key to the canonical wire digest. An already-canonical
 * digest passes through unchanged so a fixture can assert an exact identity.
 */
export function canonicalProjectHash(projectKey: string): string {
  if (/^[0-9a-f]{64}$/.test(projectKey)) return projectKey;
  return createHash('sha256').update(projectKey).digest('hex');
}

function helperGroupItems(groups: GroupedSyncHelperGroupSpec[] | undefined) {
  return (groups ?? []).map((group) => ({
    groupId: group.groupId,
    purpose: group.purpose ?? ('helper_review' as const),
    helperThreadCount: group.helperThreadCount,
    memberScope: group.memberScope,
  }));
}

function transcriptItem(session: GroupedSyncSessionSpec) {
  const projectHash = canonicalProjectHash(session.projectHash);
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
        projectHash,
        preview: session.preview,
        outcome: session.outcome,
        inputSubmissionCount: session.inputSubmissionCount,
      },
      sync: {
        id: session.id,
        harness: session.harness,
        projectName: session.project ?? 'unknown project',
        projectHash,
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
    helperGroups: helperGroupItems(session.helperGroups),
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
      helperGroups: helperGroupItems(context.helperGroups),
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

/**
 * Build one scoped member page. A member row is a transcript item and may carry
 * its own nested `helperGroups`, exactly as the member route serves them.
 */
export function buildGroupedMembersResponse(
  members: GroupedSyncSessionSpec[],
  options: { page?: number; limit?: number; total?: number } = {},
) {
  const items = members.map(transcriptItem);
  return {
    members: items,
    page: options.page ?? 1,
    limit: options.limit ?? (items.length || 1),
    total: options.total ?? items.length,
  };
}
