'use client';

import { useEffect, useState, useSyncExternalStore } from 'react';
import { zLocalContributionEvidenceKind, type LocalContributionEvidenceKind } from '@peasant-labs/schema';
import { VillageObservations } from '@peasant-labs/fairtrade/ui';
import { Button, Input } from '@/lib/ft-ui';
import { fetchVillageAuth } from '@/lib/api/settings';
import { ObservationController } from '@/lib/village/observations';
import { presentObservations } from '@/lib/village/presentation';
import './village.css';

export default function VillagePageClient() {
  const [controller] = useState(() => new ObservationController());
  const state = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot);
  const [view, setView] = useState<'catalog' | 'history'>('catalog');
  const [category, setCategory] = useState<LocalContributionEvidenceKind>('remote-attempts');
  const [filter, setFilter] = useState('');
  const [limit, setLimit] = useState('50');
  // The signed-in account's name, so the page names it instead of its id.
  const [handle, setHandle] = useState<string | null>(null);
  useEffect(() => {
    const readHandle = () => {
      fetchVillageAuth()
        .then(auth => setHandle(auth.authenticated && auth.username ? auth.username : null))
        .catch(() => setHandle(null));
    };
    readHandle();
    void controller.load();
    // A return from login/logout elsewhere re-reads only the selected LOCAL projection.
    const readSelected = () => { readHandle(); void controller.reselect(); };
    window.addEventListener('focus', readSelected);
    return () => { window.removeEventListener('focus', readSelected); controller.clear(); };
  }, [controller]);
  useEffect(() => { setFilter(''); setLimit('50'); }, [state.catalog?.scope.userId, state.catalog?.scope.villageUrl]);
  useEffect(() => {
    if (view === 'history' && state.catalog && !state.pagers[category].data) void controller.history(category);
    // Selection changes, not report/cache revision changes, initiate an evidence read.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [controller, view, category, state.catalog?.scope.userId, state.catalog?.scope.villageUrl]);
  function selectCategory(id: string) {
    const kind = zLocalContributionEvidenceKind.parse(id);
    setCategory(kind);
    setFilter(state.pagers[kind].collectiveId ?? '');
    setLimit(String(state.pagers[kind].limit));
  }
  return <div className="asb-page village-frame"><div className="village-page">
    <div className="village-page-context">
      <h1 className="iu-page-title page-title">online observations</h1>
    </div>
    <div className="village-page-tools">
      <Button onClick={() => { void controller.load(); }} disabled={state.loading}>reload saved observations</Button>
      {state.fetching && <Button onClick={controller.cancelFetch}>cancel metadata fetch</Button>}
      {view === 'history' && <details className="village-page-filter-tools">
        <summary className="btn btn-secondary btn-sm">filter this category</summary>
        <form className="village-page-filter" onSubmit={event => {
          event.preventDefault();
          void controller.filter(category, filter.trim() || undefined, Number(limit));
        }}>
          <label htmlFor="history-collective">collective id (optional; includes history-only collectives)</label>
          <Input id="history-collective" value={filter} onChange={event => setFilter(event.target.value)} />
          <label htmlFor="history-limit">items per page</label>
          <Input id="history-limit" type="number" min={1} max={200} required value={limit} onChange={event => setLimit(event.target.value)} />
          <Button type="submit" disabled={!state.catalog}>apply to this category</Button>
        </form>
      </details>}
    </div>
    <VillageObservations
      key={state.catalog ? JSON.stringify([state.catalog.scope.villageUrl, state.catalog.scope.userId]) : 'unselected'}
      {...presentObservations(state, handle)}
      view={view} categoryId={category}
      onFetch={() => { void controller.fetch(); }}
      onTrack={(id: string, tracking: boolean) => { void controller.track(id, tracking); }}
      onViewChange={setView}
      onCategoryChange={selectCategory}
      onPageChange={(id: string, page: number) => { void controller.history(zLocalContributionEvidenceKind.parse(id), page); }}
      onRestart={(id: string) => { void controller.restart(zLocalContributionEvidenceKind.parse(id)); }}
    />
  </div></div>;
}
