"use client";

import { useEffect, useMemo, useState } from 'react';
import { useChannel } from '@/contexts/WebSocketContext';
import type { SessionsPayload } from '@/types/messages';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { DiscoveryErrorCode, discoveryErrorCode } from '@/lib/api/errors';
import { ProjectPicker, rowsFromSummaries, rowsFromSessions } from '@/components/picker/ProjectPicker';
import { ProjectListState, SelectionRecoveryPanel, projectListState } from '@/components/picker/SelectionRecoveryPanel';
import { IngestTeach } from '@/components/IngestTeach';
import { DocsLink } from '@/components/DocsLink';
import { cachedProjectSummaries, fetchProjectSummaries, type DecodedProjectSummariesPayload } from '@/lib/api/map';
import { formatRelative } from '@/app/review/[[...segments]]/format';
import { Button, DataState, FeedbackPanel, StatGrid, type TileSpec } from '@/lib/ft-ui';
import { FolderOpen, MessageSquare, Sparkles, GitBranch, EyeOff, ChevronUp, ChevronDown } from 'lucide-react';
import { AllSessions } from '@/components/sessions/AllSessions';
import { GroupedSessionsSection } from '@/components/sessions/GroupedSessionsSection';
import { useSessionTitles } from '@/hooks/useSessionTitles';

const CHANNELS: ['sessions'] = ['sessions'];

function SelectionNotice({ notice }: { notice: { hiddenProjects: number; hiddenSessions: number } }) {
  const [open, setOpen] = useState(false);
  const parts: string[] = [];
  if (notice.hiddenProjects) parts.push(`${notice.hiddenProjects.toLocaleString()} project${notice.hiddenProjects === 1 ? '' : 's'}`);
  if (notice.hiddenSessions) parts.push(`${notice.hiddenSessions.toLocaleString()} session${notice.hiddenSessions === 1 ? '' : 's'}`);
  return <div role="status" className="flex flex-col items-start gap-2">
    <button type="button" onClick={() => setOpen(value => !value)} aria-expanded={open} className="inline-flex items-center gap-2 border border-rule px-3 py-1.5 font-mono text-sm text-ink-3 hover:text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer">
      <EyeOff size={14} aria-hidden /><span className="tabular-nums">{parts.join(' and ')} hidden by a saved selection</span>{open ? <ChevronUp size={14} aria-hidden /> : <ChevronDown size={14} aria-hidden />}
    </button>
    {open && <div className="flex flex-col gap-1 border border-rule bg-surface-hover px-4 py-3 text-sm text-ink-2"><p>a saved project selection is limiting what’s shown here. the data stays ingested and indexed; it is only hidden from this list.</p><p className="text-sm text-ink-3"><DocsLink topic="selection" variant="inline">change the selection</DocsLink></p></div>}
  </div>;
}

/** Retained full-library browse flows mount only after an explicit disclosure. */
export default function LegacyHomeBrowse({ view }: { view: 'projects' | 'grouped' | 'flat' }) {
  const { data, connected, error, errorCode } = useChannel<SessionsPayload>(CHANNELS);
  const titles = useSessionTitles();
  const sessions = useMemo(() => data?.sessions ?? [], [data]);
  const [summaries, setSummaries] = useState<DecodedProjectSummariesPayload | null>(() => cachedProjectSummaries());
  const [settled, setSettled] = useState(false);
  const [failure, setFailure] = useState<unknown>(null);
  const [selectionFailed, setSelectionFailed] = useState(false);
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    if (view !== 'projects' || error) return;
    let active = true;
    setSettled(false);
    void fetchProjectSummaries().then(next => {
      if (!active) return;
      setSummaries(next);
      setFailure(null);
      setSelectionFailed(false);
    }).catch(cause => {
      if (!active) return;
      setSummaries(null);
      setFailure(cause);
      if (discoveryErrorCode(cause) === DiscoveryErrorCode.SelectionVisibility) setSelectionFailed(true);
    }).finally(() => { if (active) setSettled(true); });
    return () => { active = false; };
  }, [view, revision, error]);
  const recovery = summaries && projectListState(summaries) === ProjectListState.SelectionRecovery ? summaries.selection : null;
  const blocked = !!error || selectionFailed || !!recovery;
  const rows = blocked ? [] : summaries?.projects.length ? rowsFromSummaries(summaries) : rowsFromSessions(sessions);
  const totalSessions = sessions.length || rows.reduce((sum, row) => sum + row.sessions, 0);
  const recorded = rows.reduce((sum, row) => sum + (row.recordedFiles ?? 0), 0);
  const totalFiles = rows.reduce((sum, row) => sum + (row.totalFiles ?? 0), 0);
  const hasCoverage = rows.some(row => row.recordedFiles !== null && row.totalFiles !== null);
  const hasOpen = rows.some(row => row.openChanges !== null);
  const openChanges = rows.reduce((sum, row) => sum + (row.openChanges ?? 0), 0);
  const times = rows.flatMap(row => row.lastWorkMs === null ? [] : [row.lastWorkMs]);
  const tiles: TileSpec[] = [
    { key: 'projects', label: 'projects', value: rows.length.toLocaleString(), icon: FolderOpen },
    { key: 'sessions', label: 'ai conversations', value: totalSessions.toLocaleString(), icon: MessageSquare },
    { key: 'files', label: 'files built with ai', value: hasCoverage && totalFiles > 0 ? `${Math.round(recorded / totalFiles * 100)}%` : '—', sub: hasCoverage ? `${recorded.toLocaleString()} of ${totalFiles.toLocaleString()} files` : undefined, icon: Sparkles },
    { key: 'open', label: 'unmerged branches', value: hasOpen ? openChanges.toLocaleString() : '—', sub: times.length ? `last worked ${formatRelative(Math.max(...times))}` : undefined, icon: GitBranch },
  ];
  return <div className="flex flex-col gap-4" data-browse-view={view}>
    {error && <FeedbackPanel variant="error">{discoveryErrorMessage(error, errorCode)}</FeedbackPanel>}
    {failure != null && <div className="flex flex-col items-start gap-3">
      <FeedbackPanel variant="error">{discoveryErrorMessage(failure)}</FeedbackPanel>
      <Button variant="secondary" size="sm" onClick={() => setRevision(value => value + 1)}>retry project discovery</Button>
    </div>}
    {recovery && <SelectionRecoveryPanel {...recovery} />}
    {!blocked && <DataState loading={view === 'projects' ? !settled && data === undefined : data === undefined} status={rows.length === 0 && !connected && summaries === null ? 'disconnected' : 'live'} empty={view === 'projects' && settled && failure === null && rows.length === 0} emptyState={<IngestTeach />} skeletonRows={4}>
      {view === 'projects' && <>
        {summaries?.selection.active && (summaries.selection.hiddenProjects > 0 || summaries.selection.hiddenSessions > 0) && <SelectionNotice notice={summaries.selection} />}
        <h3 className="font-[family-name:var(--font-display)] text-base font-semibold lowercase text-ink">projects</h3>
        {rows.length > 0 && <StatGrid tiles={tiles} />}
        {failure != null && rows.length > 0 && <p className="text-sm text-ink-3">Per-project stats couldn’t load, so coverage and unmerged-branch counts show “—”. Session counts and last-work are still accurate.</p>}
        <ProjectPicker rows={rows} destination="sessions" statsPending={!settled} />
      </>}
      {view === 'grouped' && <GroupedSessionsSection variant="sessions" invalidationKey={data} titles={titles} heading="all sessions" />}
      {view === 'flat' && <AllSessions sessions={sessions} titles={titles} title="every session" />}
    </DataState>}
  </div>;
}
