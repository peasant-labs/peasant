import type { CollectiveFetchDomainSummary, LocalContributionEvidenceKind, LocalContributionHistoryResponse } from '@peasant-labs/schema';
import type { ObservationState } from './observations';
import { defaultOnlineUrl } from './links';

export const EVIDENCE_LABELS: Record<LocalContributionEvidenceKind, string> = {
  'remote-attempts': 'remote attempts', receipts: 'confirmed local receipts', 'operation-diagnostics': 'operation diagnostics',
};

function rows(data: LocalContributionHistoryResponse | undefined) {
  if (!data) return [];
  switch (data.kind) {
    case 'remote-attempts': return data.remoteAttempts.map(row => ({
      id: `${row.collectiveId}/${row.transcriptId}/${row.attempt.event_num}`,
      title: row.transcriptId,
      outcome: row.attempt.status,
      occurredAt: row.attempt.recorded_at,
      details: [
        { label: 'collective', value: row.collectiveId },
        { label: 'attempt', value: String(row.attempt.event_num) },
        { label: 'actor capacity', value: row.attempt.decided_by_actor || 'none recorded' },
        { label: 'decided at', value: row.attempt.decided_at ?? 'not recorded' },
        { label: 'first seen locally', value: row.firstSeenAt },
      ],
    }));
    case 'receipts': return data.receipts.map(row => ({
      id: row.receiptId, title: row.transcriptId, outcome: row.outcome, occurredAt: row.confirmedAt,
      description: 'confirmed local operation effect; this is not collective approval.',
      details: [
        { label: 'operation', value: row.operation },
        { label: 'collective', value: row.collectiveId ?? 'not target-specific' },
        { label: 'receipt', value: row.receiptId },
        ...(row.sourceReceiptId ? [{ label: 'source receipt', value: row.sourceReceiptId }] : []),
      ],
    }));
    case 'operation-diagnostics': return data.operationDiagnostics.map(row => ({
      id: row.operationId, title: row.operation, outcome: row.outcome, occurredAt: row.attemptedAt,
      description: `${row.issue.message} ${row.issue.retry}`,
      details: [
        { label: 'operation id', value: row.operationId },
        { label: 'issue', value: row.issue.code },
        ...(row.collectiveId ? [{ label: 'collective', value: row.collectiveId }] : []),
        ...(row.transcriptId ? [{ label: 'transcript', value: row.transcriptId }] : []),
      ],
    }));
  }
}

/** The online address by name: peasant online for the default one, else its host. */
function onlineLabel(villageUrl: string): string {
  try {
    const host = new URL(villageUrl).host;
    return host === new URL(defaultOnlineUrl()).host ? 'peasant online' : host;
  } catch {
    return 'peasant online';
  }
}

/**
 * Cook display text only. Do not infer permission, reconcile attempts or interpret traversal tokens.
 * `handle` is the signed-in account's name, shown in place of its id.
 */
export function presentObservations(state: ObservationState, handle?: string | null) {
  const catalog = state.catalog;
  function domain(id: 'catalog' | 'history' | 'associations', saved: CollectiveFetchDomainSummary | undefined) {
    const result = state.report?.domains.find(d => d.domain === id);
    const issue = result?.issue ?? saved?.issue;
    return {
      id, label: id, result: result?.state ?? (saved?.lastAttemptAt || saved?.lastCompletedAt ? saved.state : 'never fetched'),
      commit: result?.commit,
      lastCompletedAt: result?.lastCompletedAt ?? saved?.lastCompletedAt ?? undefined,
      lastAttemptAt: result?.lastAttemptAt ?? saved?.lastAttemptAt ?? undefined,
      issue: issue ? { message: issue.message, retry: issue.retry } : undefined,
    };
  }
  return {
    accountLabel: catalog ? (handle ? `@${handle}` : 'your account') : 'no selected account loaded',
    villageLabel: catalog ? onlineLabel(catalog.scope.villageUrl) : 'saved online observations',
    domains: [domain('catalog', catalog?.fetch.catalog), domain('history', catalog?.fetch.history), domain('associations', catalog?.fetch.associations)],
    collectives: catalog?.collectives.map(row => ({
      id: row.id, name: row.name, description: row.description ?? undefined,
      reasons: row.reasons, observedAt: row.observedAt,
      state: row.state === 'observed' ? 'dated observation; not current permission' : `${row.state}; retained dated evidence only`,
      policy: [row.acceptanceMode, row.dataAccess].filter(Boolean).join(', ') || undefined,
      tracking: row.reasons.includes('explicit'),
    })) ?? [],
    categories: (Object.keys(EVIDENCE_LABELS) as LocalContributionEvidenceKind[]).map(kind => {
      const pager = state.pagers[kind];
      return {
        id: kind, label: EVIDENCE_LABELS[kind], rows: rows(pager.data),
        page: pager.page, pageCount: Math.max(1, Math.ceil((pager.data?.total ?? 0) / pager.limit)),
        total: pager.data?.total ?? 0, coverage: pager.data?.coverage ?? 'unknown; not read yet',
        issue: pager.issue ? { message: pager.issue, retry: pager.conflict ? 'restart this category to read its changed evidence.' : 'retry this category; other categories remain available.' } : undefined,
        conflict: pager.conflict, loading: pager.loading,
      };
    }),
    fetching: state.fetching, fetchDisabled: !catalog || state.loading,
    status: state.loading ? 'reading saved local observations' : state.status ?? (
      catalog?.fetch.catalog.lastCompletedAt && catalog.collectives.length === 0 ? 'catalog was fetched; no saved relevant collectives' : undefined),
    issue: state.issue ? { message: state.issue, retry: 'retry the local read, or check the sign-in in settings. already saved effects are kept.' } : undefined,
  };
}
