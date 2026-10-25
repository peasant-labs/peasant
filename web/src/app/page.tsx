"use client";

import { lazy, Suspense, useMemo, useState, useEffect } from 'react';
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
import { PUBLISH_FILTERS, PUBLISH_FILTER_LABELS, isPublishFilter, type PublishFilter } from '@/lib/home/publishState';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { displayProject } from '@/lib/quality/utils';
import type { LocalSyncProjectOption } from '@/lib/api/home';
import './home.css';

const LegacyHomeBrowse = lazy(() => import('@/components/home/LegacyHomeBrowse'));

/**
 * What Home lists. `sessions` is the session table; the others are the
 * retained browse flows (the project picker, sessions with their helper
 * threads, the flat filterable list), reached from the same `browse by` menu.
 */
type BrowseView = 'sessions' | 'projects' | 'grouped' | 'flat';
const BROWSE_VIEWS: readonly BrowseView[] = ['sessions', 'projects', 'grouped', 'flat'];
const BROWSE_LABELS: Record<BrowseView, string> = {
  sessions: 'sessions', projects: 'project', grouped: 'helper threads', flat: 'every session',
};
const isBrowseView = (value: string): value is BrowseView => (BROWSE_VIEWS as readonly string[]).includes(value);
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
  const [filter, setFilter] = useState<PublishFilter>('all');
  const [project, setProject] = useState<LocalSyncProjectOption | null>(null);
  const [query, setQuery] = useState('');
  const [search, setSearch] = useState('');
  const [browse, setBrowse] = useState<BrowseView>('sessions');
  const { settled } = usePublishState();
  const home = useHomeSessions({ publication: filter, projectHash: project?.projectHash }, settled);
  useEffect(() => {
    if (project && home.projects !== null && !home.projects.some(option => option.projectHash === project.projectHash)) setProject(null);
  }, [home.projects, project]);
  useEffect(() => {
    const timer = setTimeout(() => setSearch(query.trim()), query ? 250 : 0);
    return () => clearTimeout(timer);
  }, [query]);
  const roots = home.groups.get('');
  const discoveryError = roots?.error;
  const rootPages = roots?.pages;
  const rows = useMemo(() => [...new Map((rootPages ?? []).flatMap(page => page.items).map(row => [row.sync.id, row])).values()], [rootPages]);
  const children = new Map([...home.groups].filter(([id]) => id !== '').map(([id, group]) => [id, [...new Map(group.pages.flatMap(page => page.items).map(row => [row.sync.id, row])).values()]]));
  const allRows = [...rows, ...[...children.values()].flat()];
  const titles = new Map(allRows.map(row => [row.sync.id, row.title]));
  const previews = new Map(allRows.map(row => [row.sync.id, row.preview]));

  // The filters: the server-owned publish-state filter and the project the
  // list is scoped to. Each set filter counts once on the trigger.
  const filterCount = (filter !== 'all' ? 1 : 0) + (project ? 1 : 0);
  const projectOptions = [
    ...(project && !home.projects?.some(option => option.projectHash === project.projectHash) ? [project] : []),
    ...(home.projects ?? []),
  ];
  const filterGroups: ChoiceGroup[] = [
    {
      id: 'status',
      caption: 'status',
      value: filter,
      items: PUBLISH_FILTERS.map(option => ({ value: option, label: PUBLISH_FILTER_LABELS[option] })),
      onChoose: next => { if (isPublishFilter(next)) setFilter(next); },
    },
    {
      id: 'project',
      caption: 'project',
      value: project?.projectHash ?? ALL_PROJECTS,
      items: [
        { value: ALL_PROJECTS, label: 'all projects' },
        ...projectOptions.map(option => projectChoice(option.projectHash, option.projectName)),
      ],
      onChoose: hash => setProject(hash === ALL_PROJECTS ? null : projectOptions.find(option => option.projectHash === hash) ?? null),
      note: home.projectsLoading ? 'loading project choices…' : home.projectError != null ? discoveryErrorMessage(home.projectError) : undefined,
      retry: home.projectError != null ? { label: 'retry project choices', onSelect: () => { void home.loadProjects(); } } : undefined,
    },
  ];

  // The table's controls sit on its header: search, then the filters and the
  // browse menu. Search and filters act on the session table only.
  const browseMenu = <ChoiceMenu
    ariaLabel="browse by"
    align="end"
    trigger={<><Layers aria-hidden="true" />browse by: {BROWSE_LABELS[browse]}</>}
    groups={[{
      id: 'browse',
      value: browse,
      items: BROWSE_VIEWS.map(view => ({ value: view, label: BROWSE_LABELS[view] })),
      onChoose: next => { if (isBrowseView(next)) setBrowse(next); },
    }]}
  />;

  return <div className="asb-page home-page" data-home-bounded="true">
    <header className="home-head" data-tour="home">
      <h1 className="iu-page-title page-title">sessions</h1>
    </header>
    <AutoPublishTip />
    {discoveryError != null && <div className="flex flex-col items-start gap-3">
      <FeedbackPanel variant="error">{discoveryErrorMessage(discoveryError)}</FeedbackPanel>
      <Button variant="secondary" size="sm" onClick={() => home.retry()}>retry the session list</Button>
    </div>}
    <div className="home-board">
      <div className="home-toolbar" data-home-toolbar="true">
        {browse === 'sessions' && <label className="home-search">
          <Search className="home-search-icon" aria-hidden="true" />
          <input type="search" aria-label="search your sessions" placeholder="search titles, prompts, tool output" value={query} onChange={event => setQuery(event.target.value)} />
          {query && <button type="button" className="btn btn-ghost btn-sm btn-icon home-search-clear" aria-label="clear search" onClick={() => setQuery('')}><X aria-hidden="true" /></button>}
        </label>}
        <div className="home-toolbar-menus">
          {browse === 'sessions' && !search && <ChoiceMenu
            ariaLabel="filters"
            align="end"
            closeOnChoose={false}
            onOpen={() => { if (home.projectError == null) void home.loadProjects(); }}
            trigger={<><SlidersHorizontal aria-hidden="true" />filters{filterCount > 0 && <><span className="home-filter-count" aria-hidden="true">{filterCount}</span><span className="sr-only">, {filterCount} set</span></>}</>}
            groups={filterGroups}
            actions={[{ label: 'clear filters', disabled: filterCount === 0, onSelect: () => { setFilter('all'); setProject(null); } }]}
          />}
          {browseMenu}
        </div>
      </div>
      {browse === 'sessions' ? <>
        {search ? <GroupedSessionsSection key={`${search}-${home.revision}`} variant="search" query={search} heading="search results" rowDetail={row => <SearchDetail row={row} />} />
          : !roots?.pages.length && roots?.error != null ? null
          : <RootSessionList rows={rows} childGroups={children} titles={titles} previews={previews} home={home} filter={filter} scope={`${filter}:${project?.projectHash ?? ''}`} loading={!roots?.pages.length && roots?.error == null} />}
      </> : <section aria-label={`browse by ${BROWSE_LABELS[browse]}`} id={`home-browse-${browse}`} className="home-browse">
        <Suspense fallback={<SkeletonList rows={4} />}><LegacyHomeBrowse view={browse} /></Suspense>
      </section>}
    </div>
  </div>;
}
