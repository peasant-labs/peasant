/** Map generated Peasant wire payloads onto Fairtrade's cooked graph props. */

import {
  type ChangeDetailPayload,
  type ChangeDiffPayload,
  type ChangesPayload,
} from '@peasant-labs/fairtrade/graph';
import type { DecodedChangeDetailPayload, DecodedChangeDiffPayload, DecodedReviewListPayload } from '@/lib/api/map';

export function adaptChanges(wire: DecodedReviewListPayload): ChangesPayload {
  return {
    repoFound: wire.repoFound,
    defaultBranch: wire.defaultBranch,
    changes: wire.changes,
    recentCommits: wire.recentCommits,
    sessions: wire.sessions,
    rewrittenCommits: wire.rewrittenCommits,
  };
}

export function adaptChangeDetail(
  wire: DecodedChangeDetailPayload,
  filesChanged: number,
): ChangeDetailPayload {
  return {
    branch: wire.branch,
    baseRef: wire.baseRef,
    defaultBranch: wire.defaultBranch,
    files: wire.files,
    slice: {
      nodes: wire.slice.nodes,
      structureEdges: wire.slice.structureEdges,
      activityEdges: wire.slice.activityEdges,
    },
    newEdges: wire.newEdges,
    removedEdges: wire.removedEdges,
    newNodes: wire.newNodes,
    removedNodes: wire.removedNodes,
    violations: wire.violations,
    work: wire.work,
    unrecordedCommits: wire.unrecordedCommits,
    unusual: wire.unusual,
    frictions: wire.frictions,
    insights: wire.insights,
    filesChanged,
    linesAdded: wire.linesAdded,
    linesRemoved: wire.linesRemoved,
    outputTokens: wire.outputTokens,
    costUsd: wire.costUsd,
  };
}

export function adaptChangeDiff(
  wire: DecodedChangeDiffPayload,
): ChangeDiffPayload {
  return {
    branch: wire.branch,
    file: wire.file,
    oldPath: wire.oldPath ?? null,
    status: wire.status,
    binary: wire.binary,
    truncated: wire.truncated,
    hunks: wire.hunks.map((hunk) => ({
      oldStart: hunk.oldStart,
      oldLines: hunk.oldLines,
      newStart: hunk.newStart,
      newLines: hunk.newLines,
      header: hunk.header,
      lines: hunk.lines.map((line) => ({
        kind: line.kind,
        text: line.text,
      })),
      sessionId: hunk.sessionId,
      sessionTitle: hunk.sessionTitle,
    })),
  };
}
