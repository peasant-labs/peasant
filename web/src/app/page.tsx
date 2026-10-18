"use client";

import { lazy, Suspense, useMemo, useState, useEffect } from 'react';
import { Button, FeedbackPanel, Input, PublishStateLabel } from '@/lib/ft-ui';
import { SkeletonList } from '@/lib/skeleton';
import { RootSessionList } from '@/components/home/RootSessionList';
import { AutoPublishTip } from '@/components/home/AutoPublishTip';
import { GroupedSessionsSection } from '@/components/sessions/GroupedSessionsSection';
import { useHomeSessions } from '@/hooks/useHomeSessions';
import { usePublishState } from '@/contexts/PublishContext';
import { fetchPublications } from '@/lib/api/publications';
import type { LocalSessionRow } from '@/lib/api/grouped';
import { type PublishFilter } from '@/lib/home/publishState';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import type { LocalSyncProjectOption } from '@/lib/api/home';

const LegacyHomeBrowse = lazy(() => import('@/components/home/LegacyHomeBrowse'));
type BrowseView = 'projects' | 'grouped' | 'flat';
const browseLabels: Record<BrowseView, string> = {
  projects: 'browse by project', grouped: 'sessions with their helper threads', flat: 'filter and page through every session',
};

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

export default function HomePage() {
  const [filter, setFilter] = useState<PublishFilter>('all');
  const [project, setProject] = useState<LocalSyncProjectOption | null>(null);
  const [query, setQuery] = useState('');
  const [search, setSearch] = useState('');
  const [browse, setBrowse] = useState<ReadonlySet<BrowseView>>(() => new Set());
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
  const selectionFailed = home.selectionError !== null;
  const discoveryError = home.selectionError ?? roots?.error;
  const rootPages = roots?.pages;
  const rows = useMemo(() => [...new Map((rootPages ?? []).flatMap(page => page.items).map(row => [row.sync.id, row])).values()], [rootPages]);
  const children = new Map([...home.groups].filter(([id]) => id !== '').map(([id, group]) => [id, [...new Map(group.pages.flatMap(page => page.items).map(row => [row.sync.id, row])).values()]]));
  const allRows = [...rows, ...[...children.values()].flat()];
  const titles = new Map(allRows.map(row => [row.sync.id, row.title]));
  const previews = new Map(allRows.map(row => [row.sync.id, row.preview]));
  return <div className="mx-auto flex w-full max-w-[1184px] flex-col gap-6 px-6 pt-8 pb-12 animate-fade-up" data-home-bounded="true">
    <header data-tour="home"><h1 className="m-0 font-[family-name:var(--font-display)] text-[length:var(--fs-xl)] font-bold leading-tight lowercase text-[color:var(--ink-strong)]">your sessions</h1></header>
    <AutoPublishTip />
    <Input label="search transcripts" type="search" placeholder="search titles, prompts, tool output" value={query} onChange={event => setQuery(event.target.value)} />
    {query && <Button variant="secondary" size="sm" onClick={() => setQuery('')}>clear search</Button>}
    {discoveryError != null && <div className="flex flex-col items-start gap-3">
      <FeedbackPanel variant="error">{discoveryErrorMessage(discoveryError)}</FeedbackPanel>
      <Button variant="secondary" size="sm" onClick={() => home.retry()}>retry the session list</Button>
    </div>}
    {!selectionFailed && (search ? <GroupedSessionsSection key={`${search}-${home.revision}`} variant="search" query={search} heading="search results" rowDetail={row => <SearchDetail row={row} />} />
      : !roots?.pages.length && roots?.error != null ? null
      : <RootSessionList rows={rows} childGroups={children} titles={titles} previews={previews} home={home} filter={filter} onFilter={setFilter} project={project} onProject={hash => setProject(home.projects?.find(option => option.projectHash === hash) ?? null)} loading={!roots?.pages.length && roots?.error == null} />)}
    <section className="flex flex-col gap-4 border-t border-rule pt-6" aria-label="more ways to browse">
      <h2 className="font-[family-name:var(--font-display)] text-lg font-semibold lowercase text-ink">more ways to browse</h2>
      <div className="flex flex-wrap gap-2">{(Object.keys(browseLabels) as BrowseView[]).map(view => <Button key={view} variant="secondary" size="sm" aria-expanded={browse.has(view)} aria-controls={`home-browse-${view}`} onClick={() => setBrowse(previous => { const next = new Set(previous); if (next.has(view)) next.delete(view); else next.add(view); return next; })}>{browseLabels[view]}</Button>)}</div>
      {(Object.keys(browseLabels) as BrowseView[]).map(view => <div key={view} id={`home-browse-${view}`}>{browse.has(view) && !selectionFailed && <Suspense fallback={<SkeletonList rows={4} />}><LegacyHomeBrowse view={view} /></Suspense>}</div>)}
    </section>
  </div>;
}
