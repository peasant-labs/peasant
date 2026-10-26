import type {
  CollectiveFetchReport, CollectiveObservationResponse, CollectiveObservationScope,
  LocalContributionEvidenceKind, LocalContributionHistoryResponse,
} from '@peasant-labs/schema';
import {
  ContributionTraversalError, fetchCollectiveObservations, readCollectiveObservations,
  readContributionHistory, setCollectiveTracking,
} from '@/lib/api/collective-observations';
import { LocalRequestError } from '@/lib/api/validated-local';

export interface EvidencePager {
  page: number;
  limit: number;
  collectiveId?: string;
  revision?: string;
  data?: LocalContributionHistoryResponse;
  loading: boolean;
  issue?: string;
  conflict: boolean;
}
export interface ObservationState {
  catalog?: CollectiveObservationResponse;
  report?: CollectiveFetchReport;
  pagers: Record<LocalContributionEvidenceKind, EvidencePager>;
  loading: boolean;
  fetching: boolean;
  status?: string;
  issue?: string;
  localOffline: boolean;
}
const newPager = (): EvidencePager => ({ page: 1, limit: 50, loading: false, conflict: false });
function emptyState(): ObservationState {
  return { loading: false, fetching: false, localOffline: false, pagers: {
    'remote-attempts': newPager(), receipts: newPager(), 'operation-diagnostics': newPager(),
  } };
}
function sameScope(a: CollectiveObservationScope, b: CollectiveObservationScope): boolean {
  return a.userId === b.userId && a.villageUrl === b.villageUrl;
}
function message(error: unknown): string { return error instanceof Error ? error.message : String(error); }

/** App-owned orchestration. All authority, validation and reconciliation stay on the server/schema. */
export class ObservationController {
  private state = emptyState();
  private listeners = new Set<() => void>();
  private epoch = 0;
  private catalogRequest = 0;
  private pagerRequests: Record<LocalContributionEvidenceKind, number> = { 'remote-attempts': 0, receipts: 0, 'operation-diagnostics': 0 };
  private requests = new Set<AbortController>();
  private fetchRequest?: AbortController;
  getSnapshot = (): ObservationState => this.state;
  subscribe = (listener: () => void): (() => void) => { this.listeners.add(listener); return () => this.listeners.delete(listener); };
  private emit(patch: Partial<ObservationState>): void {
    this.state = { ...this.state, ...patch };
    this.listeners.forEach(listener => listener());
  }
  private pager(kind: LocalContributionEvidenceKind, patch: Partial<EvidencePager>): void {
    this.emit({ pagers: { ...this.state.pagers, [kind]: { ...this.state.pagers[kind], ...patch } } });
  }
  private request(): AbortController { const c = new AbortController(); this.requests.add(c); return c; }
  /** Called on account selection/logout/unmount before any new local reads. */
  clear = (): void => {
    this.epoch++;
    this.requests.forEach(c => c.abort());
    this.requests.clear();
    this.fetchRequest = undefined;
    this.state = emptyState();
    this.listeners.forEach(listener => listener());
  };
  private failure(error: unknown): void {
    if (error instanceof LocalRequestError && error.status === 401) this.clear();
    this.emit({ issue: message(error), localOffline: error instanceof LocalRequestError && error.status === 0 });
  }
  private acceptCatalog(catalog: CollectiveObservationResponse): void {
    if (this.state.catalog && !sameScope(this.state.catalog.scope, catalog.scope)) {
      this.clear();
    }
    const report = catalog.fetch.latestReport ?? (
      this.state.catalog?.cacheRevision === catalog.cacheRevision ? this.state.report : undefined);
    this.emit({ catalog, report, localOffline: false });
  }
  /** Saved local projection only. Opening the page never POSTs to Village. */
  load = async (): Promise<void> => {
    const epoch = this.epoch, serial = ++this.catalogRequest, request = this.request();
    this.emit({ loading: true, issue: undefined });
    try {
      const catalog = await readCollectiveObservations(request.signal);
      if (epoch === this.epoch && serial === this.catalogRequest) this.acceptCatalog(catalog);
    } catch (error) {
      if (!request.signal.aborted && epoch === this.epoch && serial === this.catalogRequest) this.failure(error);
    } finally {
      this.requests.delete(request);
      if (serial === this.catalogRequest) this.emit({ loading: false });
    }
  };
  /** Hide private state while rechecking selection; unchanged accounts retain independent traversals. */
  reselect = async (): Promise<void> => {
    const previous = this.state;
    this.clear();
    const epoch = this.epoch;
    await this.load();
    if (epoch === this.epoch && previous.catalog && this.state.catalog && sameScope(previous.catalog.scope, this.state.catalog.scope)) {
      const pagers = { ...previous.pagers };
      for (const kind of Object.keys(pagers) as LocalContributionEvidenceKind[]) {
        pagers[kind] = { ...pagers[kind], loading: false };
      }
      this.emit({ pagers, report: this.state.catalog.fetch.latestReport ?? (
        previous.catalog.cacheRevision === this.state.catalog.cacheRevision ? previous.report : undefined) });
    }
  };
  fetch = async (): Promise<void> => {
    if (this.state.fetching || !this.state.catalog) return;
    const epoch = this.epoch, scope = this.state.catalog.scope, request = this.request();
    this.fetchRequest = request;
    this.emit({ fetching: true, issue: undefined, status: 'fetching online metadata; saved evidence remains available' });
    try {
      const answer = await fetchCollectiveObservations(request.signal);
      if (epoch !== this.epoch) return;
      // Re-read the selected saved scope before rendering a possibly late old-account report.
      const catalog = await readCollectiveObservations(request.signal);
      if (epoch !== this.epoch) return;
      this.acceptCatalog(catalog);
      if (!sameScope(scope, catalog.scope) || !sameScope(answer.report.scope, catalog.scope)) return;
      this.emit({ report: catalog.fetch.latestReport ?? answer.report, status: `metadata fetch ${answer.report.outcome}`,
        issue: answer.report.issues.length ? answer.report.issues.map(issue => `${issue.message} ${issue.retry}`).join('\n') : undefined });
      // No pager reset: each subsequent request uses that category's own opaque token.
      await Promise.all((Object.keys(this.state.pagers) as LocalContributionEvidenceKind[])
        .filter(kind => this.state.pagers[kind].data)
        .map(kind => this.history(kind)));
    } catch (error) {
      if (epoch !== this.epoch) return;
      if (request.signal.aborted) {
        this.emit({ status: 'fetch cancelled; already saved effects are retained' });
        await this.load();
      } else {
        this.failure(error);
        if (epoch === this.epoch) {
          await this.load();
          if (epoch === this.epoch) this.emit({ issue: message(error) });
        }
      }
    } finally {
      this.requests.delete(request);
      if (this.fetchRequest === request) this.fetchRequest = undefined;
      if (epoch === this.epoch) this.emit({ fetching: false });
    }
  };
  cancelFetch = (): void => { this.fetchRequest?.abort(); };
  track = async (id: string, tracking: boolean): Promise<void> => {
    const epoch = this.epoch, request = this.request();
    try {
      const catalog = await setCollectiveTracking(id, tracking, request.signal);
      if (epoch === this.epoch) {
        this.acceptCatalog(catalog);
        this.emit({ status: tracking ? 'tracking saved; membership and publication consent are unchanged' : 'tracking removed; membership and publication consent are unchanged', issue: undefined });
      }
    } catch (error) {
      if (!request.signal.aborted && epoch === this.epoch) this.failure(error);
    } finally { this.requests.delete(request); }
  };
  history = async (kind: LocalContributionEvidenceKind, page?: number): Promise<void> => {
    if (!this.state.catalog || this.state.pagers[kind].conflict) return;
    const epoch = this.epoch, scope = this.state.catalog.scope;
    const serial = ++this.pagerRequests[kind], before = this.state.pagers[kind], request = this.request();
    const nextPage = page ?? before.page;
    this.pager(kind, { loading: true, issue: undefined });
    try {
      const data = await readContributionHistory({ kind, page: nextPage, limit: before.limit,
        collectiveId: before.collectiveId, revision: before.revision }, request.signal);
      if (epoch !== this.epoch || serial !== this.pagerRequests[kind]) return;
      if (!sameScope(scope, data.scope)) { this.clear(); await this.load(); return; }
      this.pager(kind, { data, page: data.page, revision: data.traversalRevision, loading: false, conflict: false });
    } catch (error) {
      if (request.signal.aborted || epoch !== this.epoch || serial !== this.pagerRequests[kind]) return;
      if (error instanceof LocalRequestError && error.status === 401) { this.failure(error); return; }
      if (error instanceof ContributionTraversalError) {
        try {
          // A token mismatch can mean account selection changed, not just evidence mutation.
          const catalog = await readCollectiveObservations(request.signal);
          if (epoch !== this.epoch || serial !== this.pagerRequests[kind]) return;
          this.acceptCatalog(catalog);
          if (!sameScope(scope, catalog.scope)) return;
        } catch (selectionError) {
          if (!request.signal.aborted && epoch === this.epoch) this.failure(selectionError);
          if (epoch !== this.epoch) return;
        }
      }
      this.pager(kind, { loading: false, issue: message(error), conflict: error instanceof ContributionTraversalError });
      if (error instanceof LocalRequestError && error.status === 0) this.emit({ localOffline: true });
    } finally { this.requests.delete(request); }
  };
  restart = async (kind: LocalContributionEvidenceKind): Promise<void> => {
    this.pagerRequests[kind]++;
    this.pager(kind, { ...newPager(), collectiveId: this.state.pagers[kind].collectiveId, limit: this.state.pagers[kind].limit,
      data: undefined, revision: undefined, issue: undefined });
    await this.history(kind);
  };
  filter = async (kind: LocalContributionEvidenceKind, collectiveId: string | undefined, limit: number): Promise<void> => {
    this.pager(kind, { collectiveId, limit });
    await this.restart(kind);
  };
}
