import {
  collectiveFetchHTTPStatus,
  zCollectiveFetchReport,
  zCollectiveObservationResponse,
  zLocalContributionHistoryResponse,
  zLocalContributionTraversalConflict,
  type CollectiveFetchReport,
  type CollectiveObservationResponse,
  type LocalContributionHistoryQuery,
  type LocalContributionHistoryResponse,
  type LocalContributionTraversalConflict,
} from '@peasant-labs/schema';
import { decodeLocalAnswer, LocalRequestError, localRefusal, readLocalAnswer } from './validated-local';

export const OBSERVATIONS_PATH = '/api/v1/village/collective-observations';
export const CONTRIBUTIONS_PATH = '/api/v1/village/contributions';

export class ContributionTraversalError extends LocalRequestError {
  constructor(readonly conflict: LocalContributionTraversalConflict) {
    super(CONTRIBUTIONS_PATH, 409, `${conflict.issue.message} ${conflict.issue.retry}`);
    this.name = 'ContributionTraversalError';
  }
}

export interface ObservationFetchAnswer {
  status: number;
  report: CollectiveFetchReport;
}

export async function readCollectiveObservations(signal?: AbortSignal): Promise<CollectiveObservationResponse> {
  const answer = await readLocalAnswer(OBSERVATIONS_PATH, { signal });
  if (!answer.ok) throw localRefusal(OBSERVATIONS_PATH, answer);
  return decodeLocalAnswer(OBSERVATIONS_PATH, answer, zCollectiveObservationResponse);
}

/** Non-2xx reports retain their complete domain effects. An unscoped refusal still rejects. */
export async function fetchCollectiveObservations(signal?: AbortSignal): Promise<ObservationFetchAnswer> {
  const path = `${OBSERVATIONS_PATH}/fetch`;
  const answer = await readLocalAnswer(path, { method: 'POST', signal });
  const parsed = zCollectiveFetchReport.safeParse(answer.body);
  if (!parsed.success && !answer.ok && answer.body && typeof answer.body === 'object' && 'error' in answer.body) {
    throw localRefusal(path, answer);
  }
  const report = decodeLocalAnswer(path, answer, zCollectiveFetchReport);
  if (collectiveFetchHTTPStatus(report) !== answer.status) {
    throw new LocalRequestError(path, answer.status,
      `peasant returned a fetch status inconsistent with its domain report. saved effects may remain; reload saved observations and update peasant before retrying.`);
  }
  return { status: answer.status, report };
}

export async function readContributionHistory(query: LocalContributionHistoryQuery, signal?: AbortSignal): Promise<LocalContributionHistoryResponse> {
  const params = new URLSearchParams({ kind: query.kind, page: String(query.page), limit: String(query.limit) });
  if (query.collectiveId) params.set('collectiveId', query.collectiveId);
  if (query.revision !== undefined) params.set('revision', query.revision);
  const path = `${CONTRIBUTIONS_PATH}?${params}`;
  const answer = await readLocalAnswer(path, { signal });
  if (answer.status === 409) {
    throw new ContributionTraversalError(decodeLocalAnswer(path, answer, zLocalContributionTraversalConflict));
  }
  if (!answer.ok) throw localRefusal(path, answer);
  const result = decodeLocalAnswer(path, answer, zLocalContributionHistoryResponse);
  if (result.kind !== query.kind || result.page !== query.page || result.limit !== query.limit ||
      (query.revision !== undefined && result.traversalRevision !== query.revision)) {
    throw new LocalRequestError(path, answer.status,
      `peasant returned another evidence category, page or traversal revision for ${path}. prior evidence is retained; reload this category before continuing.`);
  }
  return result;
}

/** PUT verifies visibility online; DELETE removes only the saved preference offline. */
export async function setCollectiveTracking(id: string, tracking: boolean, signal?: AbortSignal): Promise<CollectiveObservationResponse> {
  const path = `${OBSERVATIONS_PATH}/${encodeURIComponent(id)}/tracking`;
  const answer = await readLocalAnswer(path, { method: tracking ? 'PUT' : 'DELETE', signal });
  if (!answer.ok) throw localRefusal(path, answer);
  return decodeLocalAnswer(path, answer, zCollectiveObservationResponse);
}
