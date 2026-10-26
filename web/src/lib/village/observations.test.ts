import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import {
  encodeContributionTraversalRevision, zCollectiveObservationResponse, zCollectiveFetchReport,
  type CollectiveObservationResponse, type CollectiveObservationScope, type CollectiveFetchReport,
  type LocalContributionEvidenceKind, type OperationIssue,
} from '@peasant-labs/schema';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ObservationController } from './observations';
import { fetchCollectiveObservations, readContributionHistory } from '@/lib/api/collective-observations';
import { presentObservations } from './presentation';

const f = parse(readFileSync(resolve('src/lib/village/testdata/observations.yaml'), 'utf8'), { merge: true }) as {
  requiredNames: string[]; scope: CollectiveObservationScope; otherScope: CollectiveObservationScope;
  incarnation: string; collectiveId: string; transcriptId: string; observedAt: string; historyAt: string;
  catalog: CollectiveObservationResponse; report: CollectiveFetchReport; conflictIssue: OperationIssue;
  categories: LocalContributionEvidenceKind[];
  partialCases: { name: string; status: number; report: CollectiveFetchReport }[];
  historyCases: { name: string; kind: LocalContributionEvidenceKind; arrayName: string; rows: unknown[]; expectedOutcome: string; expectedDetail: { label: string; value: string } }[];
};
const names = new Set<string>();
const named = (name: string, fn: () => Promise<void>) => { names.add(name); it(name, fn); };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const token = (kind: LocalContributionEvidenceKind, generation = '1') => encodeContributionTraversalRevision({ scope: f.scope, incarnation: f.incarnation, kind, generation });
const history = (kind: LocalContributionEvidenceKind, page = 1, generation = '1') => ({
  scope: f.scope, kind, page, limit: 50, total: 151, coverage: 'available', traversalRevision: token(kind, generation),
  ...(kind === 'remote-attempts' ? { remoteAttempts: [] } : kind === 'receipts' ? { receipts: [] } : { operationDiagnostics: [] }),
});
let controller: ObservationController;
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  controller = new ObservationController();
  fetcher = vi.fn().mockImplementation(async (raw: string) => {
    const url = new URL(raw);
    if (url.pathname.endsWith('/contributions')) return json(history(url.searchParams.get('kind') as LocalContributionEvidenceKind, Number(url.searchParams.get('page'))));
    return json(f.catalog);
  });
  vi.stubGlobal('fetch', fetcher);
});
afterEach(() => { controller.clear(); vi.unstubAllGlobals(); });

it('preserves the required scenario names and canonical fixture validity', () => {
  expect(names).toEqual(new Set(f.requiredNames));
  expect(zCollectiveObservationResponse.safeParse(f.catalog).success).toBe(true);
  expect(zCollectiveFetchReport.safeParse(f.report).success).toBe(true);
});
for (const scenario of f.partialCases) named(scenario.name, async () => {
  await controller.load();
  const newer = structuredClone(f.catalog);
  newer.cacheRevision = '2';
  newer.collectives[0].name = 'New Catalog Name';
  fetcher.mockResolvedValueOnce(json(scenario.report, scenario.status)).mockResolvedValueOnce(json(newer));
  await controller.fetch();
  const state = controller.getSnapshot();
  expect(state.catalog?.collectives[0].name).toBe('New Catalog Name');
  expect(state.report).toEqual(scenario.report);
  expect(state.report?.domains.find(d => d.domain === 'history')?.lastCompletedAt).toBe(f.historyAt);
  expect(state.report?.domains.find(d => d.domain === 'catalog')?.lastCompletedAt).toBe(f.observedAt);
  expect(state.status).toBe('metadata fetch partial');
  expect(state.localOffline).toBe(false);
});
named('web-local-list-no-network', async () => {
  expect(fetcher).not.toHaveBeenCalled();
  await controller.load();
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(fetcher.mock.calls[0][0]).toContain('/api/v1/village/collective-observations');
  expect(fetcher.mock.calls[0][1].method).toBeUndefined();
});
named('web-diagnostics-do-not-reset-other-pagers', async () => {
  await controller.load();
  for (const kind of f.categories) { await controller.history(kind); await controller.history(kind, 3); }
  const before = controller.getSnapshot().pagers;
  fetcher.mockResolvedValueOnce(json(f.report, 502)).mockResolvedValueOnce(json({ ...f.catalog, cacheRevision: '9' }));
  await controller.fetch();
  expect(controller.getSnapshot().pagers).toEqual(before);
  await controller.history('receipts', 4);
  const params = new URL(fetcher.mock.calls.at(-1)![0]).searchParams;
  expect(params.get('revision')).toBe(token('receipts'));
  expect(params.get('page')).toBe('4');
  expect(controller.getSnapshot().pagers['remote-attempts'].page).toBe(3);
});
named('web-history-selected-revision-restart', async () => {
  await controller.load();
  await controller.history('receipts');
  await controller.history('remote-attempts');
  const receipts = controller.getSnapshot().pagers.receipts;
  fetcher.mockResolvedValueOnce(json({ currentTraversalRevision: token('remote-attempts', '2'), issue: f.conflictIssue }, 409));
  await controller.history('remote-attempts', 2);
  expect(controller.getSnapshot().pagers['remote-attempts'].conflict).toBe(true);
  const count = fetcher.mock.calls.length;
  await controller.history('remote-attempts', 2);
  expect(fetcher.mock.calls.length).toBe(count);
  fetcher.mockResolvedValueOnce(json(history('remote-attempts', 1, '2')));
  await controller.restart('remote-attempts');
  expect(new URL(fetcher.mock.calls.at(-1)![0]).searchParams.has('revision')).toBe(false);
  expect(controller.getSnapshot().pagers['remote-attempts'].revision).toBe(token('remote-attempts', '2'));
  expect(controller.getSnapshot().pagers.receipts).toEqual(receipts);
});
named('web-other-account', async () => {
  await controller.load(); await controller.history('receipts');
  fetcher.mockResolvedValueOnce(json({ ...f.catalog, scope: f.otherScope }));
  await controller.load();
  expect(controller.getSnapshot().catalog?.scope).toEqual(f.otherScope);
  expect(controller.getSnapshot().pagers.receipts.data).toBeUndefined();
  expect(controller.getSnapshot().report).toBeUndefined();
  fetcher.mockResolvedValueOnce(json({ error: 'sign in to Village to inspect saved observations' }, 401));
  await controller.load();
  expect(controller.getSnapshot().catalog).toBeUndefined();
  expect(controller.getSnapshot().issue).toContain('sign in');
});
named('web-late-old-account-report', async () => {
  await controller.load();
  let complete!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise<Response>(resolve => { complete = resolve; }));
  const pending = controller.fetch();
  controller.clear();
  fetcher.mockResolvedValueOnce(json({ ...f.catalog, scope: f.otherScope }));
  await controller.load();
  complete(json(f.report, 502));
  await pending;
  expect(controller.getSnapshot().catalog?.scope).toEqual(f.otherScope);
  expect(controller.getSnapshot().report).toBeUndefined();
  expect(controller.getSnapshot().fetching).toBe(false);
});
named('web-local-server-offline-vs-village-unavailable', async () => {
  await controller.load();
  fetcher.mockResolvedValueOnce(json({ error: 'Village is unavailable; retry tracking later' }, 502));
  await controller.track(f.collectiveId, true);
  expect(controller.getSnapshot().localOffline).toBe(false);
  expect(controller.getSnapshot().issue).toContain('Village is unavailable');
  fetcher.mockRejectedValueOnce(new TypeError('connection refused'));
  await controller.load();
  expect(controller.getSnapshot().localOffline).toBe(true);
});
named('web-history-filter-isolation', async () => {
  await controller.load(); await controller.history('receipts');
  const receipts = controller.getSnapshot().pagers.receipts;
  await controller.filter('remote-attempts', f.collectiveId, 50);
  const params = new URL(fetcher.mock.calls.at(-1)![0]).searchParams;
  expect(params.get('collectiveId')).toBe(f.collectiveId);
  expect(params.get('page')).toBe('1');
  expect(params.has('revision')).toBe(false);
  expect(controller.getSnapshot().pagers.receipts).toEqual(receipts);
});
named('web-track-online-untrack-offline', async () => {
  await controller.load();
  await controller.track(f.collectiveId, true);
  expect(fetcher.mock.calls.at(-1)![1].method).toBe('PUT');
  await controller.track(f.collectiveId, false);
  expect(fetcher.mock.calls.at(-1)![1].method).toBe('DELETE');
  expect(controller.getSnapshot().status).toContain('membership and publication consent are unchanged');
});
named('web-reselect-same-account-keeps-pagers', async () => {
  await controller.load(); await controller.history('receipts'); await controller.history('receipts', 3);
  const before = controller.getSnapshot().pagers.receipts;
  const pending = controller.reselect();
  expect(controller.getSnapshot().catalog).toBeUndefined();
  await pending;
  expect(controller.getSnapshot().pagers.receipts).toEqual(before);
});
named('web-cancel-keeps-committed-effects', async () => {
  await controller.load();
  fetcher.mockImplementationOnce((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => {
    init.signal?.addEventListener('abort', () => reject(init.signal?.reason), { once: true });
  }));
  const pending = controller.fetch();
  expect(controller.getSnapshot().fetching).toBe(true);
  controller.cancelFetch();
  await pending;
  expect(controller.getSnapshot().fetching).toBe(false);
  expect(controller.getSnapshot().catalog).toEqual(f.catalog);
  expect(controller.getSnapshot().status).toContain('already saved effects are retained');
});
named('web-empty', async () => {
  expect(presentObservations(controller.getSnapshot()).domains.every(d => d.result === 'never fetched')).toBe(true);
  fetcher.mockResolvedValueOnce(json({ ...f.catalog, collectives: [] }));
  await controller.load();
  const presentation = presentObservations(controller.getSnapshot());
  expect(presentation.collectives).toEqual([]);
  expect(presentation.status).toContain('catalog was fetched');
  expect(presentation.domains.find(d => d.id === 'history')?.lastCompletedAt).toBe(f.historyAt);
});
named('web-long-content', async () => {
  await controller.load();
  const presentation = presentObservations(controller.getSnapshot());
  expect(presentation.collectives[0].name).toBe(f.catalog.collectives[0].name);
  expect(presentation.collectives[0].description).toBe(f.catalog.collectives[0].description);
  expect(presentation.collectives[0].state).toContain('not current permission');
  expect(presentation.categories.every(c => c.coverage === 'unknown; not read yet')).toBe(true);
});
for (const scenario of f.historyCases) named(scenario.name, async () => {
  await controller.load();
  fetcher.mockResolvedValueOnce(json({ ...history(scenario.kind), total: 1, [scenario.arrayName]: scenario.rows }));
  await controller.history(scenario.kind);
  const presentation = presentObservations(controller.getSnapshot());
  const category = presentation.categories.find(c => c.id === scenario.kind)!;
  expect(category.rows.map(row => row.outcome)).toEqual([scenario.expectedOutcome]);
  expect(category.rows[0].details).toContainEqual(scenario.expectedDetail);
  expect(category.total).toBe(1);
  expect(controller.getSnapshot().pagers[scenario.kind].issue).toBeUndefined();
});

it('rejects a malformed non-2xx report rather than showing partial fields', async () => {
  fetcher.mockResolvedValueOnce(json({ ...f.report, domains: [] }, 502));
  await expect(fetchCollectiveObservations()).rejects.toThrow('Local API contract');
});
it('preserves an actionable missing-token 400 without silently restarting', async () => {
  fetcher.mockResolvedValueOnce(json({ error: 'page two needs the selected category revision; restart page one' }, 400));
  await expect(readContributionHistory({ kind: 'receipts', page: 2, limit: 50 })).rejects.toMatchObject({ status: 400, message: expect.stringContaining('revision') });
});
