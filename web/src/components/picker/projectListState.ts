import type { DecodedProjectSummariesPayload } from '@/lib/api/map';

/**
 * What a project picker shows. Two states only: the recorded projects, or the
 * genuine no-data teaching state.
 *
 * The summary answer also carries saved-selection metadata. That metadata
 * describes ingestion; it is not consulted here, so an empty project list is
 * never turned into a hidden-by-selection state.
 */
export const ProjectListState = {
  Projects: 'projects',
  NoData: 'no-data',
} as const;

export type ProjectListState = (typeof ProjectListState)[keyof typeof ProjectListState];

export function projectListState(
  payload: Pick<DecodedProjectSummariesPayload, 'projects'>,
): ProjectListState {
  return payload.projects.length > 0 ? ProjectListState.Projects : ProjectListState.NoData;
}
