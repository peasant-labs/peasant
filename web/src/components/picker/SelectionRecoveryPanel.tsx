'use client';

import type {
  DecodedProjectSummariesPayload,
  ProjectSelectionState,
} from '@/lib/api/map';
import { TeachingEmptyState } from '@/lib/ft-ui';

export enum ProjectListState {
  Projects = 'projects',
  SelectionRecovery = 'selection-recovery',
  NoData = 'no-data',
}

export type SelectionRecoveryCounts = Pick<
  ProjectSelectionState,
  'hiddenProjects' | 'hiddenSessions'
>;

export function projectListState(
  payload: Pick<DecodedProjectSummariesPayload, 'projects' | 'selection'>,
): ProjectListState {
  if (payload.projects.length > 0) return ProjectListState.Projects;
  if (
    payload.selection.active &&
    (payload.selection.hiddenProjects > 0 || payload.selection.hiddenSessions > 0)
  ) {
    return ProjectListState.SelectionRecovery;
  }
  return ProjectListState.NoData;
}

function countLabel(count: number, singular: string): string {
  return `${count.toLocaleString()} ${singular}${count === 1 ? '' : 's'}`;
}

export function SelectionRecoveryPanel({
  hiddenProjects,
  hiddenSessions,
}: SelectionRecoveryCounts) {
  return (
    <section role="status" aria-label="project selection recovery">
      <TeachingEmptyState
        title="Your saved selection hides all projects."
        body={
          <span className="flex flex-col gap-2">
            <span>
              Peasant hides{' '}
              <span className="font-mono tabular-nums">
                {countLabel(hiddenProjects, 'project')}
              </span>{' '}
              and{' '}
              <span className="font-mono tabular-nums">
                {countLabel(hiddenSessions, 'session')}
              </span>
              .
            </span>
            <span>The data stays ingested and indexed.</span>
            <span>The web viewer does not list it.</span>
            <span>It is not available for a future push.</span>
            <span>Peasant did not delete data.</span>
            <span>
              To change the selection, run{' '}
              <code className="font-mono">peasant kickstart</code>.
            </span>
          </span>
        }
        command="peasant kickstart"
        privacy={null}
      />
    </section>
  );
}
