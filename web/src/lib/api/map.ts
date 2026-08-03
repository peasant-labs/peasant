/**
 * REST client for the Map, Review, and Search surfaces.
 *
 * Wire types come directly from the generated `@peasant-labs/schema`
 * contract. Nullable Go slices are normalized here once before any mounted
 * surface receives them.
 */

import type {
  ActivityEdge,
  ChangeDetailPayload,
  ChangeDiffPayload,
  ChangeSession,
  DiffHunk,
  EdgeViolation,
  FileChange,
  FrictionCluster,
  MapEdge,
  MapGraphPayload,
  MapNode,
  MapNodeDetailPayload,
  ProjectResolutionPayload,
  ProjectSummariesPayload,
  ProjectTasksPayload,
  ReviewListPayload,
  SearchPayload,
  TaskSummary,
  TimelineSessionRef,
  UnusualSignal,
} from '@peasant-labs/schema';
import {
  isChangeBinding,
  isDiffLineKind,
  isEdgeViolationKind,
  isFileChangeStatus,
  isHarness,
  isMapNodeKind,
  type ChangeBinding,
  type DiffLine,
  type DiffLineKind,
  type EdgeViolationKind,
  type FileChangeStatus,
  type Harness,
  type MapNodeKind,
} from '@peasant-labs/schema';
import { getApiBaseUrl } from './base';
import { parseDiscoveryError } from './errors';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';

type DecodedMapSlice = {
  nodes: DecodedMapNode[];
  structureEdges: MapEdge[];
  activityEdges: ActivityEdge[];
};

export type DecodedMapNode = Omit<MapNode, 'kind'> & { kind: MapNodeKind };
export type DecodedEdgeViolation = Omit<EdgeViolation, 'kind'> & { kind: EdgeViolationKind };
export type DecodedFileChange = Omit<FileChange, 'status'> & { status: FileChangeStatus };
export type DecodedDiffLine = Omit<DiffLine, 'kind'> & { kind: DiffLineKind };

export type DecodedTaskSummary = Omit<TaskSummary, 'editedFiles' | 'labels'> & {
  editedFiles: string[];
  labels: string[];
};
type DecodedChangeSession = Omit<ChangeSession, 'binding' | 'harness' | 'tasks'> & {
  binding: ChangeBinding;
  harness: Harness;
  tasks: DecodedTaskSummary[];
};
type DecodedDiffHunk = Omit<DiffHunk, 'lines'> & { lines: DecodedDiffLine[] };

/**
 * Selection-state metadata on GET /api/v1/projects/summary: whether the
 * persisted kickstart selection is narrowing this list, and how much of the
 * store it hides, without naming which projects or sessions.
 * This is a peasant-local addition to the response, not part
 * of the schema module's `ProjectSummariesPayload` contract (see
 * internal/codemap/selection_state.go), so it is typed here rather than
 * imported from '@peasant-labs/schema'.
 */
export type ProjectSelectionState = {
  active: boolean;
  hiddenProjects: number;
  hiddenSessions: number;
};

const NO_ACTIVE_SELECTION: ProjectSelectionState = { active: false, hiddenProjects: 0, hiddenSessions: 0 };

/** The server's actual GET /api/v1/projects/summary response shape. */
type ProjectSummariesWireResponse = ProjectSummariesPayload & { selection?: ProjectSelectionState };

export type DecodedProjectSummariesPayload = Omit<ProjectSummariesPayload, 'projects'> & {
  projects: NonNullable<ProjectSummariesPayload['projects']>;
  selection: ProjectSelectionState;
};
export type DecodedMapGraphPayload = Omit<
  MapGraphPayload,
  'nodes' | 'parsedLanguages' | 'structureEdges' | 'activityEdges' | 'violations'
> & {
  nodes: DecodedMapNode[];
  parsedLanguages: string[];
  structureEdges: MapEdge[];
  activityEdges: ActivityEdge[];
  violations: DecodedEdgeViolation[];
};
export type DecodedMapNodeDetailPayload = Omit<
  MapNodeDetailPayload,
  'kind' | 'dependsOn' | 'usedBy' | 'shapedBy' | 'recentCommits'
> & {
  kind: MapNodeKind;
  dependsOn: string[];
  usedBy: string[];
  shapedBy: DecodedTaskSummary[];
  recentCommits: NonNullable<MapNodeDetailPayload['recentCommits']>;
};
export type DecodedProjectTasksPayload = Omit<ProjectTasksPayload, 'tasks'> & {
  tasks: DecodedTaskSummary[];
};
export type DecodedChangeDetailPayload = Omit<
  ChangeDetailPayload,
  | 'files'
  | 'frictions'
  | 'newEdges'
  | 'newNodes'
  | 'removedEdges'
  | 'removedNodes'
  | 'slice'
  | 'unrecordedCommits'
  | 'unusual'
  | 'violations'
  | 'work'
> & {
  files: DecodedFileChange[];
  frictions: FrictionCluster[];
  newEdges: MapEdge[];
  newNodes: string[];
  removedEdges: MapEdge[];
  removedNodes: string[];
  slice: DecodedMapSlice;
  unrecordedCommits: NonNullable<ChangeDetailPayload['unrecordedCommits']>;
  unusual: UnusualSignal[];
  violations: DecodedEdgeViolation[];
  work: DecodedChangeSession[];
};
export type DecodedChangeDiffPayload = Omit<ChangeDiffPayload, 'status' | 'hunks'> & {
  status: FileChangeStatus;
  hunks: DecodedDiffHunk[];
};
export type DecodedReviewListPayload = Omit<ReviewListPayload, 'sessions'> & {
  sessions: Array<Omit<TimelineSessionRef, 'harness'> & { harness: Harness }>;
};
export type DecodedSearchPayload = Omit<SearchPayload, 'results'> & {
  results: NonNullable<SearchPayload['results']>;
};

function projectAPIPath(surface: 'map' | 'review', projectHash: ProjectHash, suffix = ''): string {
  return `/api/v1/${surface}/${encodeURIComponent(projectHash)}${suffix}`;
}

async function getJSON<TWire, TDecoded>(
  path: string,
  decode: (wire: TWire) => TDecoded,
): Promise<TDecoded> {
  const resp = await fetch(`${getApiBaseUrl()}${path}`);
  if (!resp.ok) {
    const body = await resp.text().catch(() => '');
    throw parseDiscoveryError(path, resp.status, body);
  }
  return decode((await resp.json()) as TWire);
}

const decodeProjectSummaries = (
  wire: ProjectSummariesWireResponse,
): DecodedProjectSummariesPayload => ({
  ...wire,
  projects: wire.projects ?? [],
  selection: wire.selection ?? NO_ACTIVE_SELECTION,
});
const decodeTaskSummary = (task: TaskSummary): DecodedTaskSummary => ({
  ...task,
  editedFiles: task.editedFiles ?? [],
  labels: task.labels ?? [],
});

function contractValue<T extends string>(
  value: unknown,
  field: string,
  operation: string,
  predicate: (candidate: unknown) => candidate is T,
): T {
  if (predicate(value)) return value;
  throw new Error(
    `REST data could not be decoded because ${field} contained unknown value ${JSON.stringify(value)} in ${operation} after the Peasant API response was received; this surface has stopped to avoid misrepresenting repository or session data. Regenerate @peasant-labs/schema, update Peasant to support the new contract value, and retry.`,
  );
}

const decodeMapNode = (node: MapNode, field: string, operation: string): DecodedMapNode => ({
  ...node,
  kind: contractValue(node.kind, `${field}.kind`, operation, isMapNodeKind),
});
const decodeEdgeViolation = (
  violation: EdgeViolation,
  field: string,
  operation: string,
): DecodedEdgeViolation => ({
  ...violation,
  kind: contractValue(violation.kind, `${field}.kind`, operation, isEdgeViolationKind),
});
const decodeFileChange = (file: FileChange, field: string): DecodedFileChange => ({
  ...file,
  status: contractValue(file.status, `${field}.status`, 'decodeChangeDetail', isFileChangeStatus),
});
const decodeMapGraph = (wire: MapGraphPayload): DecodedMapGraphPayload => ({
  ...wire,
  nodes: (wire.nodes ?? []).map((node) => decodeMapNode(node, 'nodes[]', 'decodeMapGraph')),
  parsedLanguages: wire.parsedLanguages ?? [],
  structureEdges: wire.structureEdges ?? [],
  activityEdges: wire.activityEdges ?? [],
  violations: (wire.violations ?? []).map((violation) =>
    decodeEdgeViolation(violation, 'violations[]', 'decodeMapGraph'),
  ),
});
const decodeMapNodeDetail = (wire: MapNodeDetailPayload): DecodedMapNodeDetailPayload => ({
  ...wire,
  kind: contractValue(wire.kind, 'kind', 'decodeMapNodeDetail', isMapNodeKind),
  dependsOn: wire.dependsOn ?? [],
  usedBy: wire.usedBy ?? [],
  shapedBy: (wire.shapedBy ?? []).map(decodeTaskSummary),
  recentCommits: wire.recentCommits ?? [],
});
const decodeProjectTasks = (wire: ProjectTasksPayload): DecodedProjectTasksPayload => ({
  ...wire,
  tasks: (wire.tasks ?? []).map(decodeTaskSummary),
});
const decodeChangeDetail = (wire: ChangeDetailPayload): DecodedChangeDetailPayload => {
  if (!wire.slice) {
    throw new Error(
      'Change detail could not be decoded because the required slice object was absent in decodeChangeDetail after the review REST request completed; the Changes page cannot render repository structure safely. Update the Peasant server and generated @peasant-labs/schema contract together, then retry.',
    );
  }
  return {
    ...wire,
    files: (wire.files ?? []).map((file) => decodeFileChange(file, 'files[]')),
    frictions: wire.frictions ?? [],
    newEdges: wire.newEdges ?? [],
    newNodes: wire.newNodes ?? [],
    removedEdges: wire.removedEdges ?? [],
    removedNodes: wire.removedNodes ?? [],
    slice: {
      nodes: (wire.slice.nodes ?? []).map((node) =>
        decodeMapNode(node, 'slice.nodes[]', 'decodeChangeDetail'),
      ),
      structureEdges: wire.slice.structureEdges ?? [],
      activityEdges: wire.slice.activityEdges ?? [],
    },
    unrecordedCommits: wire.unrecordedCommits ?? [],
    unusual: wire.unusual ?? [],
    violations: (wire.violations ?? []).map((violation) =>
      decodeEdgeViolation(violation, 'violations[]', 'decodeChangeDetail'),
    ),
    work: (wire.work ?? []).map((session) => ({
      ...session,
      harness: contractValue(session.harness, 'work[].harness', 'decodeChangeDetail', isHarness),
      binding: contractValue(session.binding, 'work[].binding', 'decodeChangeDetail', isChangeBinding),
      tasks: (session.tasks ?? []).map(decodeTaskSummary),
    })),
  };
};
const decodeChangeDiff = (wire: ChangeDiffPayload): DecodedChangeDiffPayload => ({
  ...wire,
  status: contractValue(wire.status, 'status', 'decodeChangeDiff', isFileChangeStatus),
  hunks: (wire.hunks ?? []).map((hunk) => ({
    ...hunk,
    lines: (hunk.lines ?? []).map((line) => ({
      ...line,
      kind: contractValue(line.kind, 'hunks[].lines[].kind', 'decodeChangeDiff', isDiffLineKind),
    })),
  })),
});
const decodeReviewList = (wire: ReviewListPayload): DecodedReviewListPayload => ({
  ...wire,
  sessions: wire.sessions.map((session) => ({
    ...session,
    harness: contractValue(session.harness, 'sessions[].harness', 'decodeReviewList', isHarness),
  })),
});
const decodeSearch = (wire: SearchPayload): DecodedSearchPayload => ({ ...wire, results: wire.results ?? [] });

let projectSummariesCache: DecodedProjectSummariesPayload | null = null;
let projectSummariesInflight: Promise<DecodedProjectSummariesPayload> | null = null;

export function cachedProjectSummaries(): DecodedProjectSummariesPayload | null {
  return projectSummariesCache;
}

export function fetchProjectSummaries(): Promise<DecodedProjectSummariesPayload> {
  if (!projectSummariesInflight) {
    projectSummariesInflight = getJSON('/api/v1/projects/summary', decodeProjectSummaries)
      .then((payload) => {
        projectSummariesCache = payload;
        return payload;
      })
      .catch((error) => {
        projectSummariesCache = null;
        throw error;
      })
      .finally(() => {
        projectSummariesInflight = null;
      });
  }
  return projectSummariesInflight;
}

export function fetchProjectResolution(
  project: string,
): Promise<ProjectResolutionPayload> {
  const params = new URLSearchParams({ name: project });
  return getJSON<ProjectResolutionPayload, ProjectResolutionPayload>(
    `/api/v1/projects/resolve?${params.toString()}`,
    (wire) => wire,
  );
}

export async function fetchMapGraph(
  projectHash: ProjectHash,
  commit?: string,
): Promise<DecodedMapGraphPayload> {
  const params = new URLSearchParams();
  if (commit) params.set('commit', commit);
  const qs = params.toString();
  return getJSON(`${projectAPIPath('map', projectHash)}${qs ? `?${qs}` : ''}`, decodeMapGraph);
}

export async function fetchMapNodeDetail(
  projectHash: ProjectHash,
  path: string,
): Promise<DecodedMapNodeDetailPayload> {
  const params = new URLSearchParams({ path });
  return getJSON(`${projectAPIPath('map', projectHash, '/node')}?${params.toString()}`, decodeMapNodeDetail);
}

export async function fetchProjectTasks(
  projectHash: ProjectHash,
  file?: string,
): Promise<DecodedProjectTasksPayload> {
  const params = new URLSearchParams();
  if (file) params.set('file', file);
  const qs = params.toString();
  return getJSON(`${projectAPIPath('map', projectHash, '/tasks')}${qs ? `?${qs}` : ''}`, decodeProjectTasks);
}

export async function fetchReviewChanges(
  projectHash: ProjectHash,
): Promise<DecodedReviewListPayload> {
  return getJSON(projectAPIPath('review', projectHash), decodeReviewList);
}

export async function fetchChangeDetail(
  projectHash: ProjectHash,
  branch: string,
): Promise<DecodedChangeDetailPayload> {
  const params = new URLSearchParams({ branch });
  return getJSON(`${projectAPIPath('review', projectHash, '/change')}?${params.toString()}`, decodeChangeDetail);
}

export async function fetchChangeDiff(
  projectHash: ProjectHash,
  branch: string,
  file: string,
): Promise<DecodedChangeDiffPayload> {
  const params = new URLSearchParams({ branch, file });
  return getJSON(`${projectAPIPath('review', projectHash, '/diff')}?${params.toString()}`, decodeChangeDiff);
}

export async function fetchSearch(q: string, limit?: number): Promise<DecodedSearchPayload> {
  const params = new URLSearchParams({ q });
  if (limit) params.set('limit', String(limit));
  return getJSON(`/api/v1/search?${params.toString()}`, decodeSearch);
}
