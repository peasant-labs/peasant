'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { fetchHomeSessions, fetchHomeProjects, SYNC_HOME_PATH, type LocalSyncSessionPage, type SyncSessionsQuery, type LocalSyncProjectOption, type PageToken } from '@/lib/api/home';
import { DiscoveryErrorCode, DiscoveryRequestError } from '@/lib/api/errors';
import { fetchVillageAuth } from '@/lib/api/settings';

export interface HomePages {
  pages: LocalSyncSessionPage[];
  loading: boolean;
  error: unknown;
  retryToken?: PageToken;
}
const empty = (): HomePages => ({ pages: [], loading: false, error: null });
const ROOT = '';

function visibleExpanded(groups: ReadonlyMap<string, HomePages>, opened: ReadonlySet<string>) {
  const visible = new Set<string>();
  const pending = (groups.get(ROOT)?.pages ?? []).flatMap(page => page.items.map(row => row.sync.id));
  while (pending.length) {
    const id = pending.pop()!;
    if (!opened.has(id) || visible.has(id)) continue;
    visible.add(id);
    for (const page of groups.get(id)?.pages ?? []) for (const row of page.items) pending.push(row.sync.id);
  }
  return visible;
}

/** Home's loaded window: only explicit expansion/load-more grows it. */
export function useHomeSessions(query: SyncSessionsQuery, publicationRevision: unknown) {
  const [groups, setGroups] = useState<Map<string, HomePages>>(() => new Map([[ROOT, empty()]]));
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const [projects, setProjects] = useState<LocalSyncProjectOption[] | null>(null);
  const [projectError, setProjectError] = useState<unknown>(null);
  const [projectsLoading, setProjectsLoading] = useState(false);
  const scopeRestarted = useRef(false);
  const state = useRef(groups);
  const opened = useRef(expanded);
  const generation = useRef(0);
  const controllers = useRef(new Map<string, AbortController>());
  const refreshing = useRef(false);
  const publishPending = useRef(false);
  const account = useRef<string | undefined>(undefined);
  const queryKey = JSON.stringify(query);
  const loadedQueryKey = useRef(queryKey);
  const currentQuery = useRef(query);
  currentQuery.current = query;
  const update = useCallback((next: Map<string, HomePages>) => { state.current = next; setGroups(next); }, []);
  const reset = useCallback(() => {
    generation.current++;
    for (const controller of controllers.current.values()) controller.abort();
    controllers.current.clear();
    refreshing.current = false;
    publishPending.current = false;
    setProjects(null);
    setProjectError(null);
    setProjectsLoading(false);
    opened.current = new Set();
    setExpanded(opened.current);
    update(new Map([[ROOT, empty()]]));
  }, [update]);
  const requestQuery = useCallback((parent: string, pageToken: PageToken): SyncSessionsQuery => parent
    ? { maxItems: 25, pageToken, parentSessionId: parent }
    : { ...currentQuery.current, maxItems: 25, pageToken }, []);
  // A failed read keeps the loaded window on screen and records the failure on
  // its own continuation. It never resets the list: a saved ingestion selection
  // is not a browsing boundary, and an error's code does not change that.
  const fail = useCallback((error: unknown, parent: string, retryToken?: PageToken) => {
    const next = new Map(state.current);
    next.set(parent, { ...(next.get(parent) ?? empty()), loading: false, error, retryToken });
    update(next);
  }, [update]);
  const load = useCallback(async function loadPage(parent = ROOT, pageToken: PageToken = '') {
    if (controllers.current.has(parent) || (parent === ROOT && refreshing.current)) return;
    const token = generation.current;
    const controller = new AbortController();
    controllers.current.set(parent, controller);
    const next = new Map(state.current);
    next.set(parent, { ...(next.get(parent) ?? empty()), loading: true, error: null });
    update(next);
    try {
      const page = await fetchHomeSessions(requestQuery(parent, pageToken), controller.signal);
      if (generation.current !== token || controller.signal.aborted) return;
      const result = new Map(state.current);
      const old = result.get(parent) ?? empty();
      result.set(parent, { pages: [...old.pages, page], loading: false, error: null });
      update(result);
    } catch (error) {
      if (generation.current === token && !controller.signal.aborted) {
        if (error instanceof DiscoveryRequestError && error.status === 400 && error.code === DiscoveryErrorCode.SyncPageScopeChanged && !scopeRestarted.current) {
          scopeRestarted.current = true;
          reset();
          await loadPage();
        } else fail(error, parent, pageToken);
      }
    } finally {
      if (controllers.current.get(parent) === controller) controllers.current.delete(parent);
    }
  }, [update, requestQuery, fail, reset]);

  useEffect(() => {
    reset();
    scopeRestarted.current = false;
    loadedQueryKey.current = queryKey;
    const token = generation.current;
    void fetchVillageAuth().then(auth => {
      if (token === generation.current) account.current = JSON.stringify(auth);
    }).catch(() => {});
    void load();
    return reset;
  }, [queryKey, reset, load]);

  const refresh = useCallback(async function refreshLoaded() {
    if (refreshing.current) return;
    // A refresh supersedes page appends, not the visible content.
    for (const controller of controllers.current.values()) controller.abort();
    controllers.current.clear();
    refreshing.current = true;
    setProjects(null);
    setProjectsLoading(false);
    setProjectError(null);
    const token = ++generation.current;
    const controller = new AbortController();
    controllers.current.set('refresh', controller);
    const active = visibleExpanded(state.current, opened.current);
    update(new Map([...state.current].map(([parent, group]) => [parent, { ...group, loading: parent === ROOT || active.has(parent) }])));
    try {
      // Auth is a small local read. Never retain another account's receipts.
      const auth = await fetchVillageAuth();
      if (token !== generation.current || controller.signal.aborted) return;
      const key = JSON.stringify(auth);
      if (account.current !== undefined && account.current !== key) {
        account.current = key;
        scopeRestarted.current = false;
        reset();
        await load();
        return;
      }
      account.current = key;
      const expandedNow = visibleExpanded(state.current, opened.current);
      const targets = [...state.current].filter(([parent]) => parent === ROOT || expandedNow.has(parent));
      const replacements = await Promise.all(targets.map(async ([parent, group]) => {
        const target = parent === ROOT ? controller : new AbortController();
        if (parent) controllers.current.set(parent, target);
        try {
          const pages: LocalSyncSessionPage[] = [];
          let next: PageToken = '';
          const budget = Math.max(1, group.pages.length);
          for (let index = 0; index < budget; index++) {
            const page = await fetchHomeSessions(requestQuery(parent, next), target.signal);
            if (target.signal.aborted || token !== generation.current) return null;
            pages.push(page);
            next = page.next;
            if (next === '') break;
          }
          if (target.signal.aborted) return null;
          return [parent, { pages, loading: false, error: null }] as const;
        } catch (error) {
          if (parent && target.signal.aborted) return null;
          if (parent && error instanceof DiscoveryRequestError && error.path === SYNC_HOME_PATH
            && error.status === 404 && error.code === DiscoveryErrorCode.HomeParentNotFound) return null;
          throw error;
        } finally {
          if (parent && controllers.current.get(parent) === target) controllers.current.delete(parent);
        }
      }));
      if (token !== generation.current || controller.signal.aborted) return;
      // Only successful reads are current visibility evidence. In particular,
      // collapsed/cancelled groups cannot survive a successful selection refresh.
      const validated = new Map(replacements.filter(replacement => replacement !== null));
      const stillVisible = visibleExpanded(validated, opened.current);
      const next = new Map([...validated].filter(([parent]) => parent === ROOT || stillVisible.has(parent)));
      const nextExpanded = new Set([...opened.current].filter(parent => next.has(parent)));
      for (const [parent, pending] of controllers.current) {
        if (parent !== 'refresh' && parent !== 'project-options' && !next.has(parent)) { pending.abort(); controllers.current.delete(parent); }
      }
      opened.current = nextExpanded;
      setExpanded(nextExpanded);
      update(next);
    } catch (error) {
      if (token === generation.current && !controller.signal.aborted) {
        if (error instanceof DiscoveryRequestError && error.status === 400 && error.code === DiscoveryErrorCode.SyncPageScopeChanged && !scopeRestarted.current) {
          scopeRestarted.current = true;
          reset();
          await load();
          return;
        }
        for (const pending of controllers.current.values()) pending.abort();
        update(new Map([...state.current].map(([parent, group]) => [parent, { ...group, loading: false }])));
        fail(error, ROOT);
      }
    } finally {
      if (token === generation.current) {
        refreshing.current = false;
        controllers.current.delete('refresh');
        if (publishPending.current) {
          publishPending.current = false;
          void refreshLoaded();
        }
      }
    }
  }, [reset, load, fail, requestQuery, update]);

  useEffect(() => {
    const focus = () => { void refresh(); };
    window.addEventListener('focus', focus);
    return () => window.removeEventListener('focus', focus);
  }, [refresh]);
  const previousPublish = useRef(publicationRevision);
  useEffect(() => {
    if (previousPublish.current !== publicationRevision) {
      previousPublish.current = publicationRevision;
      if (refreshing.current) publishPending.current = true;
      else void refresh();
    }
  }, [publicationRevision, refresh]);

  const toggle = (parent: string) => {
    const next = new Set(opened.current);
    if (next.has(parent)) {
      next.delete(parent);
      const nextGroups = new Map(state.current);
      const pending = [parent];
      const visited = new Set<string>();
      while (pending.length) {
        const id = pending.pop()!;
        if (visited.has(id)) continue;
        visited.add(id);
        controllers.current.get(id)?.abort();
        controllers.current.delete(id);
        const group = nextGroups.get(id);
        if (group) {
          nextGroups.set(id, { ...group, loading: false });
          for (const page of group.pages) for (const row of page.items) pending.push(row.sync.id);
        }
      }
      update(nextGroups);
    } else {
      next.add(parent);
      if (!state.current.get(parent)?.pages.length) void load(parent);
    }
    opened.current = next;
    setExpanded(next);
  };
  const more = (parent = ROOT) => {
    const pages = state.current.get(parent)?.pages ?? [];
    const next = pages[pages.length - 1]?.next ?? '';
    if (next !== '') void load(parent, next);
  };
  const loadProjects = useCallback(async () => {
    if (controllers.current.has('project-options') || projects !== null) return;
    const controller = new AbortController();
    const token = generation.current;
    controllers.current.set('project-options', controller);
    setProjectsLoading(true);
    setProjectError(null);
    try {
      const options = await fetchHomeProjects(controller.signal);
      if (!controller.signal.aborted && token === generation.current) setProjects(options);
    } catch (error) {
      if (!controller.signal.aborted && token === generation.current) setProjectError(error);
    } finally {
      if (controllers.current.get('project-options') === controller) {
        controllers.current.delete('project-options');
        setProjectsLoading(false);
      }
    }
  }, [projects]);
  const hasRootPage = (groups.get(ROOT)?.pages.length ?? 0) > 0;
  useEffect(() => {
    if (hasRootPage && projects === null && !projectsLoading && projectError === null) void loadProjects();
  }, [hasRootPage, projects, projectsLoading, projectError, loadProjects]);
  const retry = (parent = ROOT) => {
    const group = state.current.get(parent);
    if (group?.retryToken !== undefined) return load(parent, group.retryToken);
    return parent ? load(parent, group?.pages.at(-1)?.next ?? '') : refresh();
  };
  return { groups: loadedQueryKey.current === queryKey ? groups : new Map<string, HomePages>([[ROOT, empty()]]), expanded, toggle, more, refresh, revision: generation.current, retry, projects: loadedQueryKey.current === queryKey ? projects : null, projectError, projectsLoading, loadProjects };
}
