'use client';

import { useEffect, useState } from 'react';
import { Breadcrumbs } from '@/components/Breadcrumbs';
import { DocsLink } from '@/components/DocsLink';
import { IngestTeach } from '@/components/IngestTeach';
import { FeedbackPanel, Skeleton } from '@/lib/ft-ui';
import {
  cachedProjectSummaries,
  fetchProjectSummaries,
  type DecodedProjectSummariesPayload,
} from '@/lib/api/map';
import type { DocsTopic } from '@/lib/docs';
import { discoveryErrorMessage } from '@/lib/selectionGuidance';
import { ProjectPicker, rowsFromSummaries } from './ProjectPicker';
import { ProjectListState, SelectionRecoveryPanel, projectListState } from './SelectionRecoveryPanel';

/**
 * The page a project surface shows before a project is chosen: /map and
 * /review. Both list the same projects with the same picker; only the title,
 * its docs link and where a row lands differ.
 */
export function ProjectPickerPage({
  title,
  destination,
  docs,
}: {
  title: string;
  destination: 'map' | 'changes';
  docs: { topic: DocsTopic; label: string };
}) {
  // Seed from the module cache so returning to this tab renders the last
  // list instantly; the effect below refreshes it in the background.
  const [summaries, setSummaries] = useState<DecodedProjectSummariesPayload | null>(
    () => cachedProjectSummaries(),
  );
  const [error, setError] = useState<unknown>(null);
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let cancelled = false;
    fetchProjectSummaries()
      .then((payload) => {
        if (!cancelled) {
          setError(null);
          setSummaries(payload);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setSummaries(null);
          setError(err);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [reload]);

  const listState = summaries === null ? null : projectListState(summaries);
  const selectionRecovery =
    listState === ProjectListState.SelectionRecovery ? summaries?.selection ?? null : null;

  return (
    <div className="asb-page animate-fade-up">
      <Breadcrumbs items={[{ label: 'home', href: '/' }, { label: title }]} />
      <div className="flex items-center gap-2">
        <h1 className="iu-page-title page-title">
          {title}
        </h1>
        <DocsLink topic={docs.topic} variant="icon">{docs.label}</DocsLink>
      </div>

      {error !== null && (
        <div className="flex flex-col items-start gap-3">
          <FeedbackPanel variant="error">{discoveryErrorMessage(error)}</FeedbackPanel>
          <button
            type="button"
            className="border border-rule px-3 py-2 font-mono text-sm text-ink focus-mono"
            onClick={() => {
              setSummaries(null);
              setError(null);
              setReload((value) => value + 1);
            }}
          >
            retry project discovery
          </button>
        </div>
      )}

      {!error && summaries === null && (
        <Skeleton avatar={false} lines={3} label="Loading projects" />
      )}

      {!error && selectionRecovery && <SelectionRecoveryPanel {...selectionRecovery} />}

      {!error && listState === ProjectListState.NoData && <IngestTeach />}

      {summaries !== null && summaries.projects.length > 0 && (
        <ProjectPicker rows={rowsFromSummaries(summaries)} destination={destination} />
      )}
    </div>
  );
}
