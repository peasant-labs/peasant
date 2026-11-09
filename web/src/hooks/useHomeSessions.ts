'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  fetchHomeSessions,
  fetchHomeProjects,
  SYNC_HOME_PATH,
  type LocalSyncSessionPage,
  type LocalSyncProjectOption,
  type SyncSessionsQuery,
} from '@/lib/api/home';
import { DiscoveryErrorCode, DiscoveryRequestError } from '@/lib/api/errors';
import { fetchVillageAuth } from '@/lib/api/settings';
import { faultFromDiscoveryError, FaultOperation, HttpEvidence, type WebFault } from '@/lib/faults';
import type { ProjectHash, PageSize as PageSizeValue } from '@peasant-labs/schema';
import { createHomePageCache, type HomePageCache } from '@/lib/home/pageCache';
import {
  homeCacheKey,
  type BrowseView,
  type HomeCacheScope,
  type PublishFilter,
} from '@/lib/home/viewState';

/** One child family's loaded pages: explicit expansion or load-more grows it. */
export interface HomePages {
  pages: LocalSyncSessionPage[];
  loading: boolean;
  error: unknown;
  /** The typed fault for this read, with its code kept on `HttpEvidence`. */
  fault: WebFault | null;
  retryPage?: number;
}

const empty = (): HomePages => ({ pages: [], loading: false, error: null, fault: null });
const ROOT = '';

export interface HomeSessionsOptions {
  /** The publish-state filter the URL carries. */
  readonly publication: PublishFilter;
  /** The project hash the URL carries, or null for all projects. */
  readonly projectHash: ProjectHash | null;
  /** The browse view the URL carries. It changes the cache identity. */
  readonly browse: BrowseView;
  /** The settled search query the URL carries. It changes the cache identity. */
  readonly query: string;
  /** The one-based page the URL carries. */
  readonly page: number;
  /** The page size the URL carries. 25, 50, or 100. */
  readonly pageSize: PageSizeValue;
  /** Called when the server answered a different page (a clamp), so the host rewrites the URL. */
  readonly onServedPage?: (page: number) => void;
  /** Tests inject an isolated cache; the app uses the tab's `sessionStorage`. */
  readonly cache?: HomePageCache;
}

/**
 * Home's read path. The URL is the source of truth for the view, the page, and
 * the size. This hook reads only the page the URL names:
 *
 * - A warm re-render inside the tab (the page is already in memory) makes no
 *   request; going back to a page read earlier is warm.
 * - A cold document load with a matching tab cache renders the cached page and
 *   then makes one quiet revalidation request for the visible page and totals.
 * - A cold document load with no cache makes one request.
 *
 * Child families keep their explicit expansion and load-more. A focus refresh
 * revalidates the visible page and totals and the expanded families; it never
 * reloads every root page.
 */
export function useHomeSessions(options: HomeSessionsOptions, publicationRevision: unknown) {
  const [rootPage, setRootPage] = useState<LocalSyncSessionPage | null>(null);
  const [children, setChildren] = useState<Map<string, HomePages>>(() => new Map());
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [fault, setFault] = useState<WebFault | null>(null);
  const [retryPage, setRetryPage] = useState<number | undefined>(undefined);
  const [refreshing, setRefreshing] = useState(false);
  const [projects, setProjects] = useState<LocalSyncProjectOption[] | null>(null);
  const [projectError, setProjectError] = useState<unknown>(null);
  const [projectFault, setProjectFault] = useState<WebFault | null>(null);
  const [projectsLoading, setProjectsLoading] = useState(false);

  const optionsRef = useRef(options);
  optionsRef.current = options;
  const onServedRef = useRef(options.onServedPage);
  onServedRef.current = options.onServedPage;

  const childrenRef = useRef(children);
  childrenRef.current = children;
  const opened = useRef(expanded);
  opened.current = expanded;
  const rootRef = useRef(rootPage);
  rootRef.current = rootPage;
  const revisionRef = useRef<string | null>(null);
  const memory = useRef(new Map<number, LocalSyncSessionPage>());
  /** child id -> its parent's session id, learned from the rows as they load. */
  const parentOf = useRef(new Map<string, string>());
  const cache = useRef<HomePageCache>(options.cache ?? createHomePageCache());
  const account = useRef<string | undefined>(undefined);
  const accountPromise = useRef<Promise<string> | null>(null);
  const booted = useRef(false);
  const scopeRef = useRef<string | null>(null);
  const scopeRestarted = useRef(false);
  const rootToken = useRef(0);
  const rootController = useRef<AbortController | null>(null);
  const childControllers = useRef(new Map<string, AbortController>());
  const refreshingRef = useRef(false);
  const publishPending = useRef(false);

  const scopeSignature = [
    options.browse.wire,
    options.publication,
    options.projectHash ?? '',
    options.query,
    String(options.pageSize),
  ].join('\u0000');

  // The cache key is the data scope, the page size, and the page. It never has
  // to invent a grouping or a device view the key does not use.
  const cacheScope = useCallback((page: number): HomeCacheScope => ({
    browse: optionsRef.current.browse,
    publication: optionsRef.current.publication,
    project: optionsRef.current.projectHash,
    query: optionsRef.current.query,
    pageSize: optionsRef.current.pageSize,
    page,
  }), []);

  const writeCache = useCallback((page: LocalSyncSessionPage) => {
    cache.current.write(homeCacheKey(cacheScope(page.page), account.current ?? ''), page);
  }, [cacheScope]);

  const requestQuery = useCallback((parent: string, page: number, size: PageSizeValue): SyncSessionsQuery => parent
    ? { maxItems: size, page, parentSessionId: parent }
    : {
        ...(optionsRef.current.publication !== 'all' ? { publication: optionsRef.current.publication } : {}),
        ...(optionsRef.current.projectHash !== null ? { projectHash: optionsRef.current.projectHash } : {}),
        maxItems: size,
        page,
      }, []);

  const rememberParents = useCallback((parent: string, page: LocalSyncSessionPage) => {
    for (const row of page.items) {
      const parentId = row.sync.parentSessionId ?? (parent === ROOT ? undefined : parent);
      if (parentId) parentOf.current.set(row.sync.id, parentId);
    }
  }, []);

  /**
   * An expanded family is visible only while every ancestor is still open: a
   * collapsed parent hides its descendants without forgetting that they were
   * open, so re-expanding the parent restores them.
   */
  const isVisible = useCallback((id: string): boolean => {
    let parent = parentOf.current.get(id);
    const seen = new Set<string>();
    while (parent !== undefined) {
      if (seen.has(parent)) return false;
      seen.add(parent);
      if (!opened.current.has(parent)) return false;
      parent = parentOf.current.get(parent);
    }
    return true;
  }, []);

  const settleRevision = useCallback((served: LocalSyncSessionPage) => {
    // The cached value carries the revision. A new revision means every cached
    // page may be stale, so clear the tab cache and the in-tab pages before
    // writing the fresh page.
    if (revisionRef.current !== null && revisionRef.current !== served.revision) {
      cache.current.clear();
      memory.current.clear();
    }
    revisionRef.current = served.revision;
    writeCache(served);
  }, [writeCache]);

  const loadRootRef = useRef<((page: number, opts: { quiet: boolean; cached: LocalSyncSessionPage | null }) => Promise<void>) | null>(null);

  const loadRoot = useCallback(async (page: number, opts: { quiet: boolean; cached: LocalSyncSessionPage | null }) => {
    const token = ++rootToken.current;
    rootController.current?.abort();
    const controller = new AbortController();
    rootController.current = controller;
    if (opts.cached !== null) {
      memory.current.set(opts.cached.page, opts.cached);
      setRootPage(opts.cached);
      setLoading(false);
    } else if (!opts.quiet) {
      setRootPage(null);
      setLoading(true);
    }
    setError(null);
    setFault(null);
    setRetryPage(undefined);
    try {
      const served = await fetchHomeSessions(requestQuery(ROOT, page, optionsRef.current.pageSize), controller.signal);
      if (token !== rootToken.current || controller.signal.aborted) return;
      rememberParents(ROOT, served);
      settleRevision(served);
      memory.current.set(served.page, served);
      setRootPage(served);
      setLoading(false);
      setError(null);
      setFault(null);
      setRetryPage(undefined);
      if (served.page !== page) onServedRef.current?.(served.page);
    } catch (cause) {
      if (token !== rootToken.current || controller.signal.aborted) return;
      const failure = faultFromDiscoveryError(FaultOperation.SessionsRead, cause);
      if (failure?.evidence instanceof HttpEvidence && failure.evidence.status === 400 && failure.evidence.code === DiscoveryErrorCode.SyncPageScopeChanged && !scopeRestarted.current) {
        scopeRestarted.current = true;
        cache.current.clear();
        memory.current.clear();
        await loadRootRef.current?.(page, { quiet: false, cached: null });
        return;
      }
      // A failed read keeps the loaded page on screen and reports the failure.
      setLoading(false);
      setError(cause);
      setFault(failure);
      setRetryPage(page);
    } finally {
      if (rootController.current === controller) rootController.current = null;
    }
  }, [requestQuery, settleRevision, rememberParents]);
  loadRootRef.current = loadRoot;

  const resetChildren = useCallback(() => {
    parentOf.current.clear();
    for (const controller of childControllers.current.values()) controller.abort();
    childControllers.current.clear();
    opened.current = new Set();
    setExpanded(opened.current);
    childrenRef.current = new Map();
    setChildren(new Map());
  }, []);

  useEffect(() => {
    let cancelled = false;
    const page = options.page;
    const scopeChanged = scopeRef.current !== null && scopeRef.current !== scopeSignature;
    scopeRef.current = scopeSignature;
    if (scopeChanged) {
      cache.current.clear();
      memory.current.clear();
      revisionRef.current = null;
      rootController.current?.abort();
      rootController.current = null;
      resetChildren();
      setRootPage(null);
      setError(null);
      setFault(null);
      setRetryPage(undefined);
      setLoading(true);
      setProjects(null);
      setProjectError(null);
      setProjectFault(null);
      setProjectsLoading(false);
    }
    const run = async () => {
      let cached = memory.current.get(page) ?? null;
      if (!booted.current) {
        accountPromise.current ??= fetchVillageAuth()
          .then((auth) => JSON.stringify(auth))
          .catch(() => '');
        const key = await accountPromise.current;
        if (cancelled) return;
        account.current = key;
        if (cached === null) {
          cached = cache.current.read(homeCacheKey(cacheScope(page), key));
          if (cached !== null) {
            memory.current.set(page, cached);
            // Seed the known revision from the cached page, so a revalidation
            // that answers a different revision clears every cached page.
            if (revisionRef.current === null) revisionRef.current = cached.revision;
          }
        }
        if (cached !== null) {
          setRootPage(cached);
          setLoading(false);
        }
      }
      if (cancelled) return;
      if (booted.current && cached !== null && !scopeChanged) {
        // Warm re-render: the page is already in this tab. No request.
        setRootPage(cached);
        setLoading(false);
        setError(null);
        setFault(null);
        setRetryPage(undefined);
        return;
      }
      booted.current = true;
      await loadRoot(page, { quiet: cached !== null && !scopeChanged, cached });
    };
    void run();
    return () => { cancelled = true; };
  }, [scopeSignature, options.page, cacheScope, loadRoot, resetChildren]);

  const loadChild = useCallback(async (parent: string, page: number) => {
    if (childControllers.current.has(parent)) return;
    const controller = new AbortController();
    childControllers.current.set(parent, controller);
    const result = new Map(childrenRef.current);
    result.set(parent, { ...(result.get(parent) ?? empty()), loading: true, error: null, fault: null });
    childrenRef.current = result;
    setChildren(result);
    try {
      const served = await fetchHomeSessions(requestQuery(parent, page, optionsRef.current.pageSize), controller.signal);
      if (controller.signal.aborted) return;
      rememberParents(parent, served);
      const next = new Map(childrenRef.current);
      const old = next.get(parent) ?? empty();
      next.set(parent, { pages: [...old.pages, served], loading: false, error: null, fault: null });
      childrenRef.current = next;
      setChildren(next);
    } catch (cause) {
      if (!controller.signal.aborted) {
        const next = new Map(childrenRef.current);
        next.set(parent, { ...(next.get(parent) ?? empty()), loading: false, error: cause, fault: faultFromDiscoveryError(FaultOperation.SessionsRead, cause), retryPage: page });
        childrenRef.current = next;
        setChildren(next);
      }
    } finally {
      if (childControllers.current.get(parent) === controller) childControllers.current.delete(parent);
    }
  }, [requestQuery, rememberParents]);

  const toggle = (parent: string) => {
    const next = new Set(opened.current);
    if (next.has(parent)) {
      next.delete(parent);
      const nextGroups = new Map(childrenRef.current);
      const pending = [parent];
      const visited = new Set<string>();
      while (pending.length) {
        const id = pending.pop()!;
        if (visited.has(id)) continue;
        visited.add(id);
        childControllers.current.get(id)?.abort();
        childControllers.current.delete(id);
        const group = nextGroups.get(id);
        if (group) {
          nextGroups.set(id, { ...group, loading: false });
          for (const page of group.pages) for (const row of page.items) pending.push(row.sync.id);
        }
      }
      childrenRef.current = nextGroups;
      setChildren(nextGroups);
    } else {
      next.add(parent);
      if (!childrenRef.current.get(parent)?.pages.length) void loadChild(parent, 1);
    }
    opened.current = next;
    setExpanded(next);
  };

  const more = (parent: string) => {
    const pages = childrenRef.current.get(parent)?.pages ?? [];
    const last = pages[pages.length - 1];
    if (last && last.nextItems.length > 0) void loadChild(parent, last.page + 1);
  };

  const retryChild = (parent: string) => {
    const group = childrenRef.current.get(parent);
    if (group?.retryPage !== undefined) return loadChild(parent, group.retryPage);
    return loadChild(parent, (group?.pages.at(-1)?.page ?? 0) + 1);
  };

  const refreshRef = useRef<() => Promise<void>>(async () => {});
  const retry = () => {
    if (rootRef.current !== null) return refreshRef.current();
    return loadRoot(retryPage ?? optionsRef.current.page, { quiet: false, cached: null });
  };

  const refresh = useCallback(async () => {
    if (refreshingRef.current) return;
    refreshingRef.current = true;
    setRefreshing(true);
    rootController.current?.abort();
    for (const [key, controller] of childControllers.current) {
      if (key !== 'project-options') controller.abort();
    }
    childControllers.current.clear();
    const page = optionsRef.current.page;
    const pageSize = optionsRef.current.pageSize;
    const openNow = [...opened.current].filter(isVisible);
    const childBudget = new Map([...childrenRef.current].map(([parent, group]) => [parent, Math.max(1, group.pages.length)]));
    const controller = new AbortController();
    rootController.current = controller;
    try {
      const auth = await fetchVillageAuth().catch(() => null);
      const key = auth === null ? '' : JSON.stringify(auth);
      if (account.current !== undefined && account.current !== key) {
        // A different account is a genuine boundary: never show another
        // account's rows from the tab cache.
        account.current = key;
        cache.current.clear();
        memory.current.clear();
        revisionRef.current = null;
        resetChildren();
        setProjects(null);
        setProjectError(null);
        setProjectFault(null);
        setProjectsLoading(false);
        setRootPage(null);
        setError(null);
        setFault(null);
        await loadRoot(page, { quiet: false, cached: null });
        return;
      }
      account.current = key;
      const rootPromise = fetchHomeSessions(requestQuery(ROOT, page, pageSize), controller.signal);
      type ChildResult = { parent: string; pages: LocalSyncSessionPage[] } | { removed: string };
      const childPromises = openNow.filter(parent => opened.current.has(parent)).map(async (parent): Promise<ChildResult> => {
        const budget = childBudget.get(parent) ?? 1;
        const pages: LocalSyncSessionPage[] = [];
        const childController = new AbortController();
        childControllers.current.set(parent, childController);
        try {
          for (let index = 0; index < budget; index++) {
            const served = await fetchHomeSessions(requestQuery(parent, index + 1, pageSize), childController.signal);
            if (childController.signal.aborted || controller.signal.aborted) return { removed: parent };
            pages.push(served);
            if (served.nextItems.length === 0) break;
          }
          return { parent, pages };
        } catch (cause) {
          if (cause instanceof DiscoveryRequestError && cause.path === SYNC_HOME_PATH
            && cause.status === 404 && cause.code === DiscoveryErrorCode.HomeParentNotFound) {
            // The parent is no longer a visible session: its family goes with it.
            return { removed: parent };
          }
          throw cause;
        } finally {
          if (childControllers.current.get(parent) === childController) childControllers.current.delete(parent);
        }
      });
      const [rootServed, ...childResults] = await Promise.all([rootPromise, ...childPromises]);
      if (controller.signal.aborted) return;
      rememberParents(ROOT, rootServed);
      for (const result of childResults) {
        if ('parent' in result) for (const childPage of result.pages) rememberParents(result.parent, childPage);
      }
      settleRevision(rootServed);
      memory.current.set(rootServed.page, rootServed);
      setRootPage(rootServed);
      setLoading(false);
      setError(null);
      setFault(null);
      setRetryPage(undefined);
      if (rootServed.page !== page) onServedRef.current?.(rootServed.page);
      const next = new Map(childrenRef.current);
      for (const result of childResults) {
        if ('removed' in result) {
          next.delete(result.removed);
          opened.current.delete(result.removed);
        }
      }
      for (const result of childResults) {
        if ('parent' in result) next.set(result.parent, { pages: result.pages, loading: false, error: null, fault: null });
      }
      // A refresh proves which families are still open; a collapsed family
      // keeps no stale pages, so reopening it reads fresh.
      for (const parent of [...next.keys()]) {
        if (!opened.current.has(parent) || !isVisible(parent)) next.delete(parent);
      }
      childrenRef.current = next;
      setChildren(next);
      setExpanded(new Set(opened.current));
    } catch (cause) {
      if (controller.signal.aborted) return;
      const failure = faultFromDiscoveryError(FaultOperation.SessionsRead, cause);
      if (failure?.evidence instanceof HttpEvidence && failure.evidence.status === 400 && failure.evidence.code === DiscoveryErrorCode.SyncPageScopeChanged && !scopeRestarted.current) {
        scopeRestarted.current = true;
        cache.current.clear();
        memory.current.clear();
        await loadRoot(page, { quiet: false, cached: null });
        return;
      }
      // A failed refresh keeps the loaded window on screen and reports the
      // failure where the reader can retry it.
      setError(cause);
      setFault(failure);
      setRetryPage(page);
    } finally {
      if (rootController.current === controller) rootController.current = null;
      refreshingRef.current = false;
      setRefreshing(false);
      if (publishPending.current) {
        publishPending.current = false;
        void refreshRef.current();
      }
    }
  }, [loadRoot, requestQuery, resetChildren, settleRevision, rememberParents, isVisible]);
  refreshRef.current = refresh;

  useEffect(() => {
    const focus = () => { void refresh(); };
    window.addEventListener('focus', focus);
    return () => window.removeEventListener('focus', focus);
  }, [refresh]);

  const previousPublish = useRef(publicationRevision);
  useEffect(() => {
    if (previousPublish.current !== publicationRevision) {
      previousPublish.current = publicationRevision;
      if (refreshingRef.current) publishPending.current = true;
      else void refresh();
    }
  }, [publicationRevision, refresh]);

  const loadProjects = useCallback(async () => {
    if (childControllers.current.has('project-options') || projects !== null) return;
    const controller = new AbortController();
    childControllers.current.set('project-options', controller);
    setProjectsLoading(true);
    setProjectError(null);
    setProjectFault(null);
    try {
      const optionsLoaded = await fetchHomeProjects(controller.signal);
      if (!controller.signal.aborted) {
        setProjects(optionsLoaded);
        setProjectFault(null);
      }
    } catch (cause) {
      if (!controller.signal.aborted) {
        setProjectError(cause);
        setProjectFault(faultFromDiscoveryError(FaultOperation.ProjectRead, cause));
      }
    } finally {
      if (childControllers.current.get('project-options') === controller) {
        childControllers.current.delete('project-options');
        setProjectsLoading(false);
      }
    }
  }, [projects]);

  const hasRootPage = rootPage !== null;
  useEffect(() => {
    if (hasRootPage && projects === null && !projectsLoading && projectError === null) void loadProjects();
  }, [hasRootPage, projects, projectsLoading, projectError, loadProjects]);

  return {
    rootPage,
    total: rootPage?.total ?? 0,
    revision: rootPage?.revision ?? null,
    loading,
    refreshing,
    error,
    fault,
    retry,
    children,
    expanded,
    toggle,
    more,
    retryChild,
    refresh,
    projects,
    projectError,
    projectFault,
    projectsLoading,
    loadProjects,
  };
}
