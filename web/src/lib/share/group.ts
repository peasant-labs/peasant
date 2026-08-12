// Pure helpers for project-primary selection. No React, no I/O — trivially
// unit-testable and reused by the Choose step.

import type { ShareSession, ShareStatus, ShareProject, ShareHierarchyProject, ShareHierarchySession } from './types';

function emptyRollup(): Record<ShareStatus, number> {
  return { new: 0, updated: 0, shared: 0, held: 0, error: 0, pushing: 0 };
}

/** A session can be contributed only if it's new or updated. */
export function isSelectable(s: ShareSession): boolean {
  return s.shareStatus === 'new' || s.shareStatus === 'updated';
}

/**
 * Group sessions into projects. The grouping key is `projectHash` when present
 * (the real backend doesn't emit one yet) and `projectName` otherwise, so the
 * UI behaves identically against mock and real data.
 *
 * Projects are ordered by their most recent activity (newest first). Sessions
 * inside a project keep the same ordering.
 */
export function groupByProject(sessions: ShareSession[]): ShareProject[] {
  const order: string[] = [];
  const map = new Map<string, ShareSession[]>();

  for (const s of sessions) {
    const key = s.projectHash !== '' ? s.projectHash : s.projectName;
    let arr = map.get(key);
    if (!arr) {
      arr = [];
      map.set(key, arr);
      order.push(key);
    }
    arr.push(s);
  }

  const projects: ShareProject[] = order.map((key) => {
    const projectSessions = map.get(key)!;
    const first = projectSessions[0];

    let totalTokens = 0;
    let selectableCount = 0;
    let start = first.startTime;
    let end = first.startTime;
    const statusRollup = emptyRollup();

    for (const s of projectSessions) {
      totalTokens += s.totalTokens;
      if (isSelectable(s)) selectableCount += 1;
      if (s.startTime < start) start = s.startTime;
      if (s.startTime > end) end = s.startTime;
      statusRollup[s.shareStatus] += 1;
    }

    return {
      projectName: first.projectName,
      projectHash: first.projectHash,
      key,
      sessions: projectSessions,
      sessionCount: projectSessions.length,
      selectableCount,
      totalTokens,
      dateRange: { start, end },
      statusRollup,
    };
  });

  // Most recently active project first.
  projects.sort((a, b) => (a.dateRange.end < b.dateRange.end ? 1 : -1));
  return projects;
}

export function groupShareHierarchy(sessions: ShareHierarchySession[]): ShareHierarchyProject[] {
  const projects = new Map<string, ShareHierarchyProject>();
  for (const session of sessions) {
    const key = session.projectHash || session.projectName;
    let project = projects.get(key);
    if (!project) {
      project = { key, projectName: session.projectName, locations: [] };
      projects.set(key, project);
    }
    let location = project.locations.find((item) => item.repositoryLocationId === session.repositoryLocationId);
    if (!location) {
      location = { repositoryLocationId: session.repositoryLocationId, locationLabel: session.locationLabel, branches: [] };
      project.locations.push(location);
    }
    let branch = location.branches.find((item) => item.branch === session.branch);
    if (!branch) {
      branch = { branch: session.branch, sessions: [] };
      location.branches.push(branch);
    }
    branch.sessions.push(session);
  }
  return Array.from(projects.values());
}
