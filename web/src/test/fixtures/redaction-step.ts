import type { ShareSession } from '@/lib/share/types';
import type { Redaction } from '@/types/messages';

export const REDACTION_STEP_SESSION: ShareSession = {
  id: 'redaction-step-session',
  provider: 'claude-code',
  projectName: 'demo-project',
  projectHash: 'demo-project-hash',
  hostSlug: 'demo-host',
  startTime: '2026-02-24T09:00:00Z',
  durationMins: 30,
  totalTokens: 10_000,
  turnCount: 10,
  model: 'claude-sonnet-4-6',
  shareStatus: 'new',
  preview: 'demo',
};

export const REDACTION_STEP_SECOND_SESSION: ShareSession = {
  ...REDACTION_STEP_SESSION,
  id: 'redaction-step-second-session',
  startTime: '2026-02-24T10:00:00Z',
};

export const REDACTION_STEP_MATCH: Redaction = {
  id: 'credential-match',
  category: 'CREDENTIAL',
  confidence: 100,
  lineNumber: 1,
  contextBefore: [],
  contextAfter: [],
  originalText: 'sk-test-credential',
  redactedReplacement: '[REDACTED_CREDENTIAL]',
  description: 'API key',
  status: 'pending',
};

export const REDACTION_STEP_STANDARD_MATCH: Redaction = {
  id: 'standard-email-match',
  category: 'PII',
  confidence: 94,
  lineNumber: 12,
  contextBefore: ['const notificationEmail ='],
  contextAfter: ['sendNotification(notificationEmail)'],
  originalText: 'standard.user@example.com',
  redactedReplacement: '<EMAIL>',
  description: 'Email address',
  status: 'pending',
};

export const REDACTION_STEP_REFRESHED_MATCH: Redaction = {
  id: 'refreshed-project-match',
  category: 'INTERNAL',
  confidence: 100,
  lineNumber: 24,
  contextBefore: ['const branch ='],
  contextAfter: ['checkout(branch)'],
  originalText: 'feature/refreshed-project',
  redactedReplacement: '<GIT_BRANCH>',
  description: 'Git branch',
  status: 'pending',
};

export const REDACTION_STEP_SCAN_FAILURE = 'preview scan failed';

export const REDACTION_STEP_FAILURE_EXPECTATIONS = {
  allFailure: {
    scannedLabel: 'scanned 0 of 1',
    progressText: '0 / 1 sessions scanned successfully',
    incompleteTitle: 'redaction scan incomplete',
  },
  mixed: {
    scannedLabel: 'scanned 1 of 2',
    progressText: '1 / 2',
  },
  recovery: {
    // Recovery is a re-scan at the one offered level, not a switch to a weaker
    // one: the weaker levels are refused by the local API, and answering a
    // transient scan failure by protecting less was never the right move. The
    // control the failure bridge exposes is Re-scan.
    recoveryLevel: 'Re-scan',
    calls: [
      { sessionId: REDACTION_STEP_SESSION.id, level: 'standard' },
      { sessionId: REDACTION_STEP_SESSION.id, level: 'standard' },
    ],
  },
  forbiddenSafeCopy: /safe to share as-is/i,
} as const;

export const REDACTION_STEP_MOCK_EXPECTATIONS = {
  minimal: {
    count: 6,
    includedPath: '/Users/acme-dev/Projects/internal-api',
  },
  standard: {
    count: 8,
    includedEmail: 'vitor.eduardo@company.com',
  },
  maximum: {
    count: 9,
    includedRemote: 'https://github.com/acme-corp/internal-api',
  },
} as const;

export const REDACTION_STEP_DISCOVERY_PAYLOAD = {
  sessions: [
    {
      id: REDACTION_STEP_SESSION.id,
      harness: REDACTION_STEP_SESSION.provider,
      startTime: REDACTION_STEP_SESSION.startTime,
      durationMins: REDACTION_STEP_SESSION.durationMins,
      totalTokens: REDACTION_STEP_SESSION.totalTokens,
      turnCount: REDACTION_STEP_SESSION.turnCount,
      toolCallCount: 0,
      project: REDACTION_STEP_SESSION.projectName,
      preview: REDACTION_STEP_SESSION.preview,
    },
  ],
};
