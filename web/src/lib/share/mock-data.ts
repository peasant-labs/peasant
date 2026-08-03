import {
  getMockRedactions,
  type MockRedaction,
} from '@/lib/session-detail/mock-redactions';
import type { AnnotationSummary } from '@/lib/api/annotations';
import type {
  ShareStatus,
  ShareSession,
  ShareDiscoveryResult,
} from './types';

// ---------------------------------------------------------------------------
// Mock sessions — realistic variety
// ---------------------------------------------------------------------------

const MOCK_SESSIONS: ShareSession[] = [
  {
    id: 'sess-a1b2c3d4-e5f6-7890-abcd-ef1234567890',
    provider: 'claude-code',
    projectName: 'sample-project',
    projectHash: 'ph-adl-001',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-24T09:15:00Z',
    durationMins: 47,
    totalTokens: 128_450,
    turnCount: 34,
    model: 'claude-opus-4-6',
    shareStatus: 'new',    outcome: 'resolved',
    preview:
      'Wire up the agent data leverage pipeline so ingested transcripts feed the scoring model',
  },
  {
    id: 'sess-b2c3d4e5-f6a7-8901-bcde-f12345678901',
    provider: 'claude-code',
    projectName: 'sample-project',
    projectHash: 'ph-adl-001',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-23T14:30:00Z',
    durationMins: 22,
    totalTokens: 56_200,
    turnCount: 18,
    model: 'claude-sonnet-4-6',
    shareStatus: 'new',    outcome: 'partial',
    preview:
      'Fix the flaky token-count assertion in the sample-project parser tests',
  },
  {
    id: 'sess-c3d4e5f6-a7b8-9012-cdef-123456789012',
    provider: 'claude-code',
    projectName: 'sample-project',
    projectHash: 'ph-adl-001',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-22T10:00:00Z',
    durationMins: 65,
    totalTokens: 201_800,
    turnCount: 52,
    model: 'claude-opus-4-6',
    shareStatus: 'updated',    outcome: 'failed',
    preview:
      'Refactor the leverage scoring module to share the redaction pass with ingest',
  },
  {
    id: 'sess-d4e5f6a7-b8c9-0123-defa-234567890123',
    provider: 'opencode',
    projectName: 'village-api',
    projectHash: 'ph-mkt-002',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-24T08:00:00Z',
    durationMins: 33,
    totalTokens: 84_700,
    turnCount: 26,
    model: 'claude-sonnet-4-6',
    shareStatus: 'new',    preview:
      'Add a /health endpoint to village-api and document the response schema',
  },
  {
    id: 'sess-e5f6a7b8-c9d0-1234-efab-345678901234',
    provider: 'claude-code',
    projectName: 'village-api',
    projectHash: 'ph-mkt-002',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-21T16:45:00Z',
    durationMins: 15,
    totalTokens: 31_500,
    turnCount: 10,
    model: 'claude-haiku-4-5',
    shareStatus: 'shared',    preview:
      'Investigate why village-api WebSocket subscriptions drop after 30 seconds',
  },
  {
    id: 'sess-f6a7b8c9-d0e1-2345-fabc-456789012345',
    provider: 'gemini-cli',
    projectName: 'infra-terraform',
    projectHash: 'ph-infra-003',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-23T11:20:00Z',
    durationMins: 41,
    totalTokens: 112_000,
    turnCount: 29,
    model: 'gemini-2.5-pro',
    shareStatus: 'new',    preview:
      'Provision the staging VPC with Terraform and tag every resource for cost tracking',
  },
  {
    id: 'sess-a7b8c9d0-e1f2-3456-abcd-567890123456',
    provider: 'claude-code',
    projectName: 'infra-terraform',
    projectHash: 'ph-infra-003',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-20T09:30:00Z',
    durationMins: 28,
    totalTokens: 67_300,
    turnCount: 21,
    model: 'claude-sonnet-4-6',
    shareStatus: 'shared',    preview:
      'Migrate the infra-terraform state backend from local files to remote S3',
  },
  {
    id: 'sess-b8c9d0e1-f2a3-4567-bcde-678901234567',
    provider: 'claude-code',
    projectName: 'docs-site',
    projectHash: 'ph-docs-004',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-24T07:00:00Z',
    durationMins: 12,
    totalTokens: 19_800,
    turnCount: 8,
    model: 'claude-haiku-4-5',
    shareStatus: 'held',    preview:
      'Draft the getting-started page for the docs site and add a code sample',
  },
  {
    id: 'sess-c9d0e1f2-a3b4-5678-cdef-789012345678',
    provider: 'codex',
    projectName: 'docs-site',
    projectHash: 'ph-docs-004',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-22T15:10:00Z',
    durationMins: 55,
    totalTokens: 145_600,
    turnCount: 38,
    model: 'codex-1',
    shareStatus: 'updated',    preview:
      'Set up the docs-site search index and verify it builds in CI',
  },
  {
    id: 'sess-d0e1f2a3-b4c5-6789-defa-890123456789',
    provider: 'claude-code',
    projectName: 'billing-service',
    projectHash: 'ph-bill-005',
    hostSlug: 'acme-dev-mbp',
    startTime: '2026-02-23T17:00:00Z',
    durationMins: 38,
    totalTokens: 95_400,
    turnCount: 31,
    model: 'claude-opus-4-6',
    shareStatus: 'new',    preview:
      'Implement idempotent retry handling in the billing-service charge worker',
  },
];

// ---------------------------------------------------------------------------
// Mock API functions (replace with real fetch later)
// ---------------------------------------------------------------------------

function countByStatus(sessions: ShareSession[]): Record<ShareStatus, number> {
  const counts: Record<ShareStatus, number> = { new: 0, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 };
  for (const s of sessions) counts[s.shareStatus]++;
  return counts;
}

export function fetchMockSessions(): ShareDiscoveryResult {
  return {
    sessions: MOCK_SESSIONS.map((s) => ({ ...s })),
    counts: countByStatus(MOCK_SESSIONS),
  };
}

export function fetchMockRedactionPreview(sessionId: string): MockRedaction[] {
  return getMockRedactions(sessionId);
}

// ---------------------------------------------------------------------------
// Deterministic id hashing — used to keep mock annotations stable per session.
// ---------------------------------------------------------------------------

function hashId(sessionId: string): number {
  let hash = 0;
  for (let i = 0; i < sessionId.length; i++) {
    hash = ((hash << 5) - hash + sessionId.charCodeAt(i)) | 0;
  }
  return Math.abs(hash);
}

// ---------------------------------------------------------------------------
// Mock session-level annotations — deterministic per session id.
//
// Shaped exactly like the live GET /api/v1/annotations response
// (AnnotationSummary[]) so the Labels step can run identically against mock and
// real data. Mixes `rule`/`agent` (auto) and `human` (manual) annotator kinds
// so the auto/manual grouping is exercised.
// ---------------------------------------------------------------------------

interface MockAnnotationSpec {
  typeId: string;
  typeName: string;
  value: string;
  annotatorKind: AnnotationSummary['annotatorKind'];
  annotatorName: string;
}

const MOCK_ANNOTATION_POOL: MockAnnotationSpec[] = [
  { typeId: 'quality.outcome', typeName: 'Outcome', value: 'success', annotatorKind: 'rule', annotatorName: 'outcome-classifier' },
  { typeId: 'quality.retry_loops', typeName: 'Retry loops', value: '2', annotatorKind: 'rule', annotatorName: 'retry-detector' },
  { typeId: 'quality.frustration_signal', typeName: 'Frustration signal', value: 'present', annotatorKind: 'agent', annotatorName: 'claude-grader' },
  { typeId: 'quality.resolution_evidence', typeName: 'Resolution evidence', value: 'tests-passed', annotatorKind: 'agent', annotatorName: 'claude-grader' },
  { typeId: 'review.usefulness', typeName: 'Usefulness', value: 'high', annotatorKind: 'human', annotatorName: 'human-web' },
  { typeId: 'review.note', typeName: 'Reviewer note', value: 'great refactor', annotatorKind: 'human', annotatorName: 'human-web' },
];

/**
 * Session-level annotations for a session, deterministic on the id hash so the
 * same session always yields the same labels. ~1 in 4 sessions has none,
 * exercising the Labels step's "no labels" empty state. Returns the same shape
 * as the live endpoint so the step is data-source agnostic.
 */
export function getMockAnnotations(sessionId: string): AnnotationSummary[] {
  const hash = hashId(sessionId);
  if (hash % 4 === 0) return [];
  const count = 2 + (hash % 4); // 2-5 annotations
  const start = hash % MOCK_ANNOTATION_POOL.length;
  const out: AnnotationSummary[] = [];
  for (let i = 0; i < count; i++) {
    const spec = MOCK_ANNOTATION_POOL[(start + i) % MOCK_ANNOTATION_POOL.length];
    out.push({
      id: `${sessionId}::ann::${i}`,
      targetKind: 'session',
      targetSessionId: sessionId,
      isPrimary: i === 0,
      annotatorKind: spec.annotatorKind,
      annotatorName: spec.annotatorName,
      typeId: spec.typeId,
      typeName: spec.typeName,
      value: spec.value,
      createdAt: BigInt(1_700_000_000_000 + hash + i),
    });
  }
  return out;
}
