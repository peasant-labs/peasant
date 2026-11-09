"use client";

import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Layers, Search, SlidersHorizontal, X } from 'lucide-react';
import { Button, FeedbackPanel, PublishStateLabel } from '@/lib/ft-ui';
import { SkeletonList } from '@/lib/skeleton';
import { RootSessionList } from '@/components/home/RootSessionList';
import { NotFoundState } from '@/components/NotFoundState';
import { AutoPublishTip } from '@/components/home/AutoPublishTip';
import { ChoiceMenu, type ChoiceGroup, type ChoiceItem } from '@/components/ChoiceMenu';
import { GroupedSessionsSection } from '@/components/sessions/GroupedSessionsSection';
import { useHomeSessions } from '@/hooks/useHomeSessions';
import { usePublishState } from '@/contexts/PublishContext';
import { fetchPublications } from '@/lib/api/publications';
import type { LocalSessionRow } from '@/lib/api/grouped';
import type { LocalSyncProjectOption } from '@/lib/api/home';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { displayProject } from '@/lib/quality/utils';
import { newProjectHash, type ProjectHash } from '@peasant-labs/schema';
import { clearHomeOpenGroups, readHomeDeviceView } from '@/lib/home/deviceView';
import { HOME_COLUMN_IDS, type HomeSavedView } from '@/components/home/homeColumns';
import {
  ALL_BROWSE_VIEWS,
  BrowseView,
  decodeHomeViewState,
  encodeHomeViewState,
  HOME_GROUP_BYS,
  isPublishFilter,
  PUBLISH_FILTERS,
  PUBLISH_FILTER_LABELS,
  type HomeViewState,
} from '@/lib/home/viewState';
import './home.css';

const LegacyHomeBrowse = lazy(() => import('@/components/home/LegacyHomeBrowse'));

const ALL_PROJECTS = '';

/** Search rows can be off-page. Enrich only the mounted results, not the library. */
function SearchDetail({ row }: { row: LocalSessionRow }) {
  const [publication, setPublication] = useState<Awaited<ReturnType<typeof fetchPublications>>[number]>();
  useEffect(() => {
    let active = true;
    setPublication(undefined);
    void fetchPublications([row.session.id]).then(items => { if (active) setPublication(items[0]); }).catch(() => {});
    return () => { active = false; };
  }, [row.session.id]);
  const snippet = row.matches?.[0]?.snippet;
  return <div className="mt-1 flex flex-col gap-1">
    {snippet && <p className="m-0 text-base text-ink-2 [overflow-wrap:anywhere]"><span className="font-mono text-sm text-ink-3">{row.matches?.[0]?.role === 'user' ? 'you' : row.matches?.[0]?.role}: </span>{snippet}</p>}
    {publication && <PublishStateLabel state={publication.state === 'published' ? 'published' : publication.autoPublish ? 'auto-publish' : 'not-published'} />}
  </div>;
}

/**
 * The local server answers an address it has no page for with this page (its
 * nearest index.html), so Home checks the address it was opened at: anything
 * but the root shows the not-found state rather than a session list that
 * hides the mistake. Read after mount, so the exported HTML and hydration
 * agree; nothing renders, and nothing is read, until the address is known.
 */
// A project choice reads as the table does: its short name, with the folder
// it stands for as a quieter line under it when the two differ.
function projectChoice(value: string, projectName: string): ChoiceItem {
  const name = displayProject(projectName);
  return {
    value,
    name,
    keepCase: true,
    label: name === projectName ? name : <span className="choice-menu-project"><span>{name}</span><span className="choice-menu-path" title={projectName}>{projectName}</span></span>,
  };
}

function readInitialView(): HomeViewState {
  return decodeHomeViewState(typeof window === 'undefined' ? '' : window.location.search);
}

function sameView(a: HomeViewState, b: HomeViewState): boolean {
  return a.browse === b.browse
    && a.publication === b.publication
    && a.project === b.project
    && a.query === b.query
    && a.pageSize === b.pageSize
    && a.page === b.page
    && a.groupBy === b.groupBy;
}

export default function HomePage() {
  const [address, setAddress] = useState<'unread' | 'home' | 'unknown'>('unread');
  useEffect(() => {
    const path = window.location.pathname.replace(/\/index\.html$/, '/').replace(/\/+$/, '');
    setAddress(path === '' ? 'home' : 'unknown');
  }, []);
  if (address === 'unread') return null;
  return address === 'unknown' ? <NotFoundState /> : <HomeView />;
}

function HomeView() {
  const [view, setView] = useState<HomeViewState>(readInitialView);
  const viewRef = useRef(view);
  viewRef.current = view;
  const [queryInput, setQueryInput] = useState(view.query);
  const { settled } = usePublishState();

  // The URL owns the data view, the page, the size, and the grouping. A scope,
  // size, or grouping change starts at page one; a page change keeps the scope.
  const commit = useCallback((patch: Partial<HomeViewState>, mode: 'push' | 'replace' = 'push') => {
    const current = viewRef.current;
    let next: HomeViewState = { ...current, ...patch };
    const scopeChanged = (patch.browse !== undefined && patch.browse !== current.browse)
      || (patch.publication !== undefined && patch.publication !== current.publication)
      || (patch.project !== undefined && patch.project !== current.project)
      || (patch.query !== undefined && patch.query !== current.query)
      || (patch.pageSize !== undefined && patch.pageSize !== current.pageSize);
    const groupingChanged = patch.groupBy !== undefined && patch.groupBy !== current.groupBy;
    if ((scopeChanged || groupingChanged) && patch.page === undefined) next = { ...next, page: 1 };
    if (sameView(next, current)) return;
    if (groupingChanged) {
      // The grouping is URL-owned; the open groups are device-owned. A new
      // grouping names different groups, so its collapsed set starts empty.
      clearHomeOpenGroups();
    }
    viewRef.current = next;
    setView(next);
    if (typeof window === 'undefined') return;
    const search = encodeHomeViewState(next);
    const url = `${window.location.pathname}${search ? `?${search}` : ''}`;
    window.history[mode === 'push' ? 'pushState' : 'replaceState']({}, '', url);
  }, []);

  // Back and forward restore the prior view and page from the URL.
  useEffect(() => {
    const onPop = () => {
      const next = decodeHomeViewState(window.location.search);
      viewRef.current = next;
      setView(next);
      setQueryInput(next.query);
    };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  // The settled search query lives in the URL. Typing is local until it settles.
  useEffect(() => {
    const settledQuery = queryInput.trim();
    if (settledQuery === viewRef.current.query) return;
    const timer = setTimeout(() => { commit({ query: settledQuery }, 'replace'); }, settledQuery ? 250 : 0);
    return () => clearTimeout(timer);
  }, [queryInput, commit]);
  useEffect(() => { setQueryInput(view.query); }, [view.query]);

  const onServedPage = useCallback((page: number) => { commit({ page }, 'replace'); }, [commit]);
  const home = useHomeSessions({
    publication: view.publication,
    projectHash: view.project,
    browse: view.browse,
    query: view.query,
    page: view.page,
    pageSize: view.pageSize,
    onServedPage,
  }, settled);

  const projectOptions = home.projects ?? [];
  const currentProject = view.project === null
    ? null
    : projectOptions.find(option => option.projectHash === view.project)
      ?? ({ projectHash: view.project, projectName: view.project } satisfies LocalSyncProjectOption);
  useEffect(() => {
    const project = view.project;
    if (project === null || home.projects === null) return;
    if (!home.projects.some(option => option.projectHash === project)) commit({ project: null }, 'replace');
  }, [home.projects, view.project, commit]);

  // The table receives ONE saved view: the URL-owned grouping composed with
  // the device-owned columns and open groups.
  const tableState = useMemo<HomeSavedView>(() => {
    const device = readHomeDeviceView();
    return {
      groupBy: view.groupBy,
      visibleColumns: device.visibleColumns ?? HOME_COLUMN_IDS,
      open: device.open,
    };
  }, [view.groupBy]);

  const rows = useMemo(() => {
    const seen = new Set<string>();
    return (home.rootPage?.items ?? []).filter(row => {
      if (seen.has(row.sync.id)) return false;
      seen.add(row.sync.id);
      return true;
    });
  }, [home.rootPage]);
  const children = useMemo(() => new Map([...home.children].map(([id, group]) => [id, [...new Map(group.pages.flatMap(page => page.items).map(row => [row.sync.id, row])).values()]])), [home.children]);
  const allRows = [...rows, ...[...children.values()].flat()];
  const titles = new Map(allRows.map(row => [row.sync.id, row.title]));
  const previews = new Map(allRows.map(row => [row.sync.id, row.preview]));

  // The filters: the server-owned publish-state filter and the project the
  // list is scoped to. Each set filter counts once on the trigger.
  const filterCount = (view.publication !== 'all' ? 1 : 0) + (view.project ? 1 : 0);
  const projectChoices = currentProject && !projectOptions.some(option => option.projectHash === currentProject.projectHash)
    ? [currentProject, ...projectOptions]
    : projectOptions;
  const filterGroups: ChoiceGroup[] = [
    {
      id: 'status',
      caption: 'status',
      value: view.publication,
      items: PUBLISH_FILTERS.map(option => ({ value: option, label: PUBLISH_FILTER_LABELS[option] })),
      onChoose: next => { if (isPublishFilter(next)) commit({ publication: next }); },
    },
    {
      id: 'project',
      caption: 'project',
      value: view.project ?? ALL_PROJECTS,
      items: [
        { value: ALL_PROJECTS, label: 'all projects' },
        ...projectChoices.map(option => projectChoice(option.projectHash, option.projectName)),
      ],
      onChoose: hash => commit({ project: hash === ALL_PROJECTS ? null : newProjectHash(hash) }),
      note: home.projectsLoading ? 'loading project choices…' : home.projectError != null ? discoveryErrorMessage(home.projectError) : undefined,
      retry: home.projectError != null ? { label: 'retry project choices', onSelect: () => { void home.loadProjects(); } } : undefined,
    },
  ];

  // The table's controls sit on its header: search, then the filters and the
  // browse menu. Search and filters act on the session table only.
  const browseMenu = <ChoiceMenu
    ariaLabel="browse by"
    align="end"
    trigger={<><Layers aria-hidden="true" />browse by: {view.browse.label}</>}
    groups={[{
      id: 'browse',
      value: view.browse.wire,
      items: ALL_BROWSE_VIEWS.map(member => ({ value: member.wire, label: member.label })),
      onChoose: next => {
        const chosen = ALL_BROWSE_VIEWS.find(member => member.wire === next);
        if (chosen) commit({ browse: chosen });
      },
    }]}
  />;

  // The grouping is URL state; the open groups are device state. Changing the
  // grouping resets to page one and clears the device's collapsed set.
  const groupMenu = <ChoiceMenu
    ariaLabel="group by"
    align="end"
    trigger={<>group by: {view.groupBy.label}</>}
    groups={[{
      id: 'groupBy',
      value: view.groupBy.wire,
      items: HOME_GROUP_BYS.map(member => ({ value: member.wire, label: member.label })),
      onChoose: next => {
        const chosen = HOME_GROUP_BYS.find(member => member.wire === next);
        if (chosen) commit({ groupBy: chosen });
      },
    }]}
  />;
  const sessionsView = view.browse === BrowseView.Sessions;
  const searching = sessionsView && view.query !== '';
  return <div className="asb-page home-page" data-home-bounded="true">
    <header className="home-head" data-tour="home">
      <h1 className="iu-page-title page-title">sessions</h1>
    </header>
    <AutoPublishTip />
    {home.error != null && <div className="flex flex-col items-start gap-3">
      <FeedbackPanel variant="error">{discoveryErrorMessage(home.error)}</FeedbackPanel>
      <Button variant="secondary" size="sm" onClick={() => home.retry()}>retry the session list</Button>
    </div>}
    <div className="home-board">
      <div className="home-toolbar" data-home-toolbar="true">
        {sessionsView && <label className="home-search">
          <Search className="home-search-icon" aria-hidden="true" />
          <input type="search" aria-label="search your sessions" placeholder="search titles, prompts, tool output" value={queryInput} onChange={event => setQueryInput(event.target.value)} />
          {queryInput && <button type="button" className="btn btn-ghost btn-sm btn-icon home-search-clear" aria-label="clear search" onClick={() => { setQueryInput(''); commit({ query: '' }, 'replace'); }}><X aria-hidden="true" /></button>}
        </label>}
        <div className="home-toolbar-menus">
          {sessionsView && !searching && <ChoiceMenu
            ariaLabel="filters"
            className="home-filters-menu"
            align="end"
            closeOnChoose={false}
            onOpen={() => { if (home.projectError == null) void home.loadProjects(); }}
            trigger={<><SlidersHorizontal aria-hidden="true" />filters{filterCount > 0 && <><span className="home-filter-count" aria-hidden="true">{filterCount}</span><span className="sr-only">, {filterCount} set</span></>}</>}
            groups={filterGroups}
            actions={[{ label: 'clear filters', disabled: filterCount === 0, onSelect: () => commit({ publication: 'all', project: null }) }]}
          />}
          {browseMenu}
          {sessionsView && groupMenu}
        </div>
      </div>
      {sessionsView ? <>
        {searching ? <GroupedSessionsSection key={`${view.query}-${home.revision ?? ''}`} variant="search" query={view.query} heading="search results" rowDetail={row => <SearchDetail row={row} />} />
          : home.error != null && home.rootPage === null ? null
          : <RootSessionList
            rows={rows}
            childGroups={children}
            titles={titles}
            previews={previews}
            home={home}
            filter={view.publication}
            page={view.page}
            pageSize={view.pageSize}
            total={home.total}
            viewState={tableState}
            onPageChange={next => commit({ page: next })}
            onPageSizeChange={size => commit({ pageSize: size, page: 1 })}
            loading={home.loading && home.rootPage === null}
          />}
      </> : <section aria-label={`browse by ${view.browse.label}`} id={`home-browse-${view.browse.wire}`} className="home-browse">
        <Suspense fallback={<SkeletonList rows={4} />}><LegacyHomeBrowse view={view.browse.wire as 'projects' | 'grouped' | 'flat'} /></Suspense>
      </section>}
    </div>
  </div>;
}
