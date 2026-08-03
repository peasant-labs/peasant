import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { Changes, ChangeDetail } from '@peasant-labs/fairtrade/graph';
import {
  newProjectHash,
  type ReviewListPayload,
} from '@peasant-labs/schema';
import type { DecodedChangeDetailPayload, DecodedChangeDiffPayload } from '@/lib/api/map';
import { adaptChangeDetail, adaptChangeDiff, adaptChanges } from '@/lib/adapters/changes';

/* Test-quality across three axes — (i) interaction/state, (ii) empty/loading,
   (iii) real-data-shape — exercised against the LIFTED surfaces (the components the
   /review route mounts), rendered from the published @peasant-labs/fairtrade/graph.
   The adapter unit tests (changes.test.ts) cover the wire→payload mapping + the
   binary/truncated/many-commit DATA shapes; the production boot run exercised the real
   multi-branch lane derivation end-to-end; these cover the interactive behaviour the
   static side-by-side pixel diff cannot see. */

afterEach(cleanup);

const NOW = Date.UTC(2026, 5, 27, 12, 0, 0);

type TimelineInteractionFixture = {
  payload: Parameters<typeof Changes>[0]['payload'];
  expected: {
    linkedGroupLabel: string;
    overflowAction: string;
    linkedActions: Array<{ name: string; sessionId: string }>;
    unlinkedHeading: string;
    unlinkedAction: { name: string; sessionId: string };
    absentFromUnlinked: string;
    outsideWindowHeading: string;
    outsideWindowAction: { name: string; sessionId: string };
  };
};

const timelineInteractionSource = readFileSync(
    resolve(process.cwd(), 'src/app/review/[[...segments]]/testdata/timeline_interactions.yaml'),
    'utf8',
  );
const mountedChangesSource = readFileSync(
  resolve(process.cwd(), 'src/app/review/[[...segments]]/testdata/mounted_changes.yaml'),
  'utf8',
);

function requireAllowedAndRequired(
  value: Record<string, unknown>,
  allowed: string[],
  required: string[],
  path: string,
) {
  const unknown = Object.keys(value).filter((field) => !allowed.includes(field));
  const missing = required.filter((field) => !(field in value));
  if (unknown.length > 0 || missing.length > 0) {
    throw new Error(`${path} has invalid fields; unknown=${unknown.join(',')} missing=${missing.join(',')}`);
  }
}

function loadTimelineInteractionFixture(source: string): TimelineInteractionFixture {
  const value = requireRecord(parseStrictYAML(source, 'timeline interaction fixture'), 'timeline interaction fixture');
  requireExactRequiredFields(value, ['payload', 'expected'], 'timeline interaction fixture');
  const payload = requireRecord(value.payload, 'timeline interaction fixture.payload');
  requireExactRequiredFields(payload, ['repoFound', 'defaultBranch', 'changes', 'recentCommits', 'rewrittenCommits', 'sessions'], 'timeline interaction fixture.payload');
  if (!Array.isArray(payload.changes) || !Array.isArray(payload.recentCommits) || !Array.isArray(payload.sessions)) throw new Error('timeline interaction fixture payload collections must be arrays');
  for (const [index, rawCommit] of payload.recentCommits.entries()) {
    const commit = requireRecord(rawCommit, `timeline interaction fixture.payload.recentCommits[${index}]`);
    requireExactRequiredFields(commit, ['hash', 'subject', 'timeMs', 'hasSession', 'sessionIds'], `timeline interaction fixture.payload.recentCommits[${index}]`);
  }
  const sessions = payload.sessions.map((rawSession, index) => {
    const session = requireRecord(rawSession, `timeline interaction fixture.payload.sessions[${index}]`);
    requireAllowedAndRequired(session, ['sessionId', 'title', 'harness', 'startMs', 'hasCommitBinding'], ['sessionId', 'title', 'harness', 'startMs'], `timeline interaction fixture.payload.sessions[${index}]`);
    return { name: session.sessionId };
  });
  requireUniqueNames(sessions, 'timeline interaction fixture.payload.sessions');
  const expected = requireRecord(value.expected, 'timeline interaction fixture.expected');
  requireExactRequiredFields(expected, ['linkedGroupLabel', 'overflowAction', 'linkedActions', 'unlinkedHeading', 'unlinkedAction', 'absentFromUnlinked', 'outsideWindowHeading', 'outsideWindowAction'], 'timeline interaction fixture.expected');
  if (!Array.isArray(expected.linkedActions)) throw new Error('timeline interaction fixture.expected.linkedActions must be an array');
  expected.linkedActions.forEach((action, index) => requireExactRequiredFields(requireRecord(action, `timeline interaction fixture.expected.linkedActions[${index}]`), ['name', 'sessionId'], `timeline interaction fixture.expected.linkedActions[${index}]`));
  requireExactRequiredFields(requireRecord(expected.unlinkedAction, 'timeline interaction fixture.expected.unlinkedAction'), ['name', 'sessionId'], 'timeline interaction fixture.expected.unlinkedAction');
  requireExactRequiredFields(requireRecord(expected.outsideWindowAction, 'timeline interaction fixture.expected.outsideWindowAction'), ['name', 'sessionId'], 'timeline interaction fixture.expected.outsideWindowAction');
  return value as unknown as TimelineInteractionFixture;
}

const timelineInteractionFixture = loadTimelineInteractionFixture(timelineInteractionSource);

type MountedChangesFixture = {
  changesPayload: ReviewListPayload;
  detailPayload: DecodedChangeDetailPayload & { filesChanged: number };
  textDiff: DecodedChangeDiffPayload;
  expected: {
    candidateBranch: string;
    mergedBranch: string;
    file: string;
    sessionTitle: string;
    addedLine: string;
  };
};

function requireArrayRecords(value: unknown, path: string): Array<Record<string, unknown>> {
  if (!Array.isArray(value)) throw new Error(`${path} must be an array`);
  return value.map((row, index) => requireRecord(row, `${path}[${index}]`));
}

function loadMountedChangesFixture(source: string): MountedChangesFixture {
  const value = requireRecord(parseStrictYAML(source, 'mounted changes fixture'), 'mounted changes fixture');
  requireExactRequiredFields(value, ['changesPayload', 'detailPayload', 'textDiff', 'expected'], 'mounted changes fixture');
  const changes = requireRecord(value.changesPayload, 'mounted changes fixture.changesPayload');
  requireExactRequiredFields(changes, ['projectHash', 'repoFound', 'defaultBranch', 'recentCommits', 'changes', 'sessions'], 'mounted changes fixture.changesPayload');
  changes.projectHash = newProjectHash(String(changes.projectHash));
  requireArrayRecords(changes.recentCommits, 'mounted changes fixture.changesPayload.recentCommits').forEach((row, index) => {
    requireExactRequiredFields(row, ['hash', 'subject', 'timeMs', 'hasSession'], `mounted changes fixture.changesPayload.recentCommits[${index}]`);
  });
  const changeRows = requireArrayRecords(changes.changes, 'mounted changes fixture.changesPayload.changes');
  changeRows.forEach((row, index) => {
    const graphAnchor = row.merged === true ? ['mergedAtMs', 'mergeCommitHash'] : ['baseHash', 'tipCommitMs'];
    requireExactRequiredFields(row, ['branch', 'merged', ...graphAnchor, 'sessionCount', 'aheadCount', 'behindCount', 'filesChanged', 'taskCount', 'newEdges', 'removedEdges', 'violations'], `mounted changes fixture.changesPayload.changes[${index}]`);
  });
  if (!changeRows.some((row) => row.merged === false) || !changeRows.some((row) => row.merged === true)) {
    throw new Error('mounted changes fixture must include candidate and merged changes');
  }

  const detail = requireRecord(value.detailPayload, 'mounted changes fixture.detailPayload');
  requireExactRequiredFields(detail, ['branch', 'baseRef', 'defaultBranch', 'files', 'slice', 'newEdges', 'removedEdges', 'newNodes', 'removedNodes', 'violations', 'work', 'unrecordedCommits', 'unusual', 'frictions', 'filesChanged', 'linesAdded', 'linesRemoved', 'outputTokens', 'costUsd'], 'mounted changes fixture.detailPayload');
  requireArrayRecords(detail.files, 'mounted changes fixture.detailPayload.files').forEach((row, index) => {
    requireExactRequiredFields(row, ['path', 'status', 'linesAdded', 'linesRemoved'], `mounted changes fixture.detailPayload.files[${index}]`);
  });
  requireExactRequiredFields(requireRecord(detail.slice, 'mounted changes fixture.detailPayload.slice'), ['nodes', 'structureEdges', 'activityEdges'], 'mounted changes fixture.detailPayload.slice');

  const diff = requireRecord(value.textDiff, 'mounted changes fixture.textDiff');
  requireExactRequiredFields(diff, ['branch', 'file', 'status', 'binary', 'truncated', 'hunks'], 'mounted changes fixture.textDiff');
  requireArrayRecords(diff.hunks, 'mounted changes fixture.textDiff.hunks').forEach((row, index) => {
    requireExactRequiredFields(row, ['oldStart', 'oldLines', 'newStart', 'newLines', 'sessionId', 'sessionTitle', 'lines'], `mounted changes fixture.textDiff.hunks[${index}]`);
  });
  const expected = requireRecord(value.expected, 'mounted changes fixture.expected');
  requireExactRequiredFields(expected, ['candidateBranch', 'mergedBranch', 'file', 'sessionTitle', 'addedLine'], 'mounted changes fixture.expected');
  return value as unknown as MountedChangesFixture;
}

const mountedChangesFixture = loadMountedChangesFixture(mountedChangesSource);
const changesPayload = adaptChanges(mountedChangesFixture.changesPayload);
const { filesChanged, ...detailWire } = mountedChangesFixture.detailPayload;
const detailPayload = adaptChangeDetail(detailWire, filesChanged);
const textDiff = adaptChangeDiff(mountedChangesFixture.textDiff);

describe('lifted <Changes> — interaction + state', () => {
  it('rejects mounted shape drift and removal of the merged-change family', () => {
    expect(() => loadMountedChangesFixture(mountedChangesSource.replace('  costUsd: 1.23\n', ''))).toThrow(/costUsd/);
    expect(() => loadMountedChangesFixture(mountedChangesSource.replace('      merged: true\n', '      merged: false\n'))).toThrow();
  });

  it('mounts candidate and merged changes from the strict YAML fixture', () => {
    render(<Changes payload={changesPayload} projectLabel="proj" nowMs={NOW} />);
    expect(screen.getAllByText(mountedChangesFixture.expected.candidateBranch).length).toBeGreaterThan(0);
    expect(screen.getByText(/1 open · 1 merged/)).toBeInTheDocument();
    expect(changesPayload.changes.some((change) => change.branch === mountedChangesFixture.expected.mergedBranch && change.merged)).toBe(true);
  });

  it('rejects nested shape drift and removal of the named linked-session family', () => {
    expect(() => loadTimelineInteractionFixture(timelineInteractionSource.replace('      subject: shared implementation\n', ''))).toThrow(/subject/);
    expect(() => loadTimelineInteractionFixture(timelineInteractionSource.replace('  linkedGroupLabel: sessions linked to shared implementation\n', ''))).toThrow(/linkedGroupLabel/);
  });
  it('renders accessible many-to-many, legacy, and truly-unlinked session actions', async () => {
    const user = userEvent.setup();
    const onOpenSession = vi.fn();
    render(
      <Changes
        payload={timelineInteractionFixture.payload}
        projectLabel="proj"
        nowMs={NOW}
        onOpenSession={onOpenSession}
      />,
    );

    expect(
      screen.getByLabelText(timelineInteractionFixture.expected.linkedGroupLabel),
    ).toBeInTheDocument();
    const overflow = screen.queryByRole('button', { name: timelineInteractionFixture.expected.overflowAction });
    if (overflow) await user.click(overflow);
    for (const action of timelineInteractionFixture.expected.linkedActions) {
      await user.click(screen.getByRole('button', { name: new RegExp(action.name, 'i') }));
      expect(onOpenSession).toHaveBeenLastCalledWith(action.sessionId);
    }
    const unlinked = screen.getByRole('region', {
      name: timelineInteractionFixture.expected.unlinkedHeading,
    });
    expect(unlinked).toBeInTheDocument();
    await user.click(
      within(unlinked).getByRole('button', {
        name: new RegExp(timelineInteractionFixture.expected.unlinkedAction.name, 'i'),
      }),
    );
    expect(onOpenSession).toHaveBeenLastCalledWith(
      timelineInteractionFixture.expected.unlinkedAction.sessionId,
    );
    expect(
      within(unlinked).queryByRole('button', {
        name: new RegExp(timelineInteractionFixture.expected.absentFromUnlinked, 'i'),
      }),
    ).not.toBeInTheDocument();
    const outsideWindow = screen.getByRole('region', {
      name: timelineInteractionFixture.expected.outsideWindowHeading,
    });
    await user.click(within(outsideWindow).getByRole('button', {
      name: new RegExp(timelineInteractionFixture.expected.outsideWindowAction.name, 'i'),
    }));
    expect(onOpenSession).toHaveBeenLastCalledWith(
      timelineInteractionFixture.expected.outsideWindowAction.sessionId,
    );
  });

  it('(i) selecting a commit fires onSelectChange (→ change-detail nav)', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    const { container } = render(<Changes payload={changesPayload} projectLabel="proj" nowMs={NOW} onSelectChange={onSelect} />);
    const row = container.querySelector<HTMLElement>('.gmp-changes-root .cg-row');
    expect(row).toBeTruthy();
    await user.click(row!);
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it('(i) the "open the map" affordance fires onOpenMap', async () => {
    const user = userEvent.setup();
    const onOpenMap = vi.fn();
    render(<Changes payload={changesPayload} projectLabel="proj" nowMs={NOW} onOpenMap={onOpenMap} />);
    await user.click(screen.getByText(/open the map/i));
    expect(onOpenMap).toHaveBeenCalledTimes(1);
  });

  it('(ii) an empty/not-a-repo payload renders without crashing (no commit rows)', () => {
    render(<Changes payload={{ repoFound: false, changes: [], recentCommits: [], sessions: [], rewrittenCommits: [] }} projectLabel="proj" nowMs={NOW} />);
    expect(screen.getByText(/0 open · 0 merged/)).toBeTruthy();
  });
});

describe('lifted <ChangeDetail> — interaction + state + data-shape', () => {
  it('(i) lazy-expand calls getDiff and renders the diff + per-hunk attribution', () => {
    const getDiff = vi.fn(() => textDiff);
    render(<ChangeDetail payload={detailPayload} getDiff={getDiff} initialOpenFiles={{ [mountedChangesFixture.expected.file]: true }} />);
    expect(getDiff).toHaveBeenCalled();
    expect(screen.getByText(mountedChangesFixture.expected.sessionTitle)).toBeTruthy();
    expect(screen.getByText(mountedChangesFixture.expected.addedLine)).toBeTruthy();
  });

  it('(i) annotation popover save fires onSaveAnnotation with the typed value', async () => {
    const user = userEvent.setup();
    const onSave = vi.fn();
    render(<ChangeDetail payload={detailPayload} getDiff={() => null} onSaveAnnotation={onSave} />);
    await user.click(screen.getByText(/add a label/i));
    await user.type(screen.getByLabelText('annotation value'), 'good handoff');
    await user.click(screen.getByText('save'));
    expect(onSave).toHaveBeenCalledWith('good handoff');
  });

  it('(i) annotation chip remove fires onRemoveAnnotation', async () => {
    const user = userEvent.setup();
    const onRemove = vi.fn();
    render(<ChangeDetail payload={detailPayload} getDiff={() => null} annotation="good handoff" onRemoveAnnotation={onRemove} />);
    await user.click(screen.getByLabelText('remove annotation'));
    expect(onRemove).toHaveBeenCalledTimes(1);
  });

  it('(i) a proof-jump caption fragment toggles its active state', async () => {
    const user = userEvent.setup();
    const { container } = render(<ChangeDetail payload={detailPayload} getDiff={() => null} />);
    const frag = container.querySelector('.gmp-frag') as HTMLElement;
    expect(frag.textContent).toMatch(/42 files/);
    await user.click(frag);
    expect(frag.className).toMatch(/gmp-frag-on/);
  });

  it('(ii) shows the loading state while getDiff returns null', () => {
    render(<ChangeDetail payload={detailPayload} getDiff={() => null} initialOpenFiles={{ [mountedChangesFixture.expected.file]: true }} />);
    expect(screen.getByText(/loading diff/i)).toBeTruthy();
  });

  it('(ii) renders an error row (not a spinner) when getDiff returns the error sentinel', () => {
    const errDiff = { ...textDiff, hunks: [], error: 'couldn’t load this file’s diff.' };
    render(<ChangeDetail payload={detailPayload} getDiff={() => errDiff} initialOpenFiles={{ [mountedChangesFixture.expected.file]: true }} />);
    expect(screen.getByText(/couldn.t load this file.s diff/i)).toBeTruthy();
    expect(screen.queryByText(/loading diff/i)).not.toBeInTheDocument();
  });

  it('(iii) a binary file renders the binary state, not a diff (real-data-shape)', () => {
    const binaryDiff = { ...textDiff, binary: true, hunks: [] };
    render(<ChangeDetail payload={detailPayload} getDiff={() => binaryDiff} initialOpenFiles={{ [mountedChangesFixture.expected.file]: true }} />);
    expect(screen.getByText(/binary file/i)).toBeTruthy();
  });

  it('(iii) a truncated large diff still renders its hunks + a truncation note', () => {
    const truncated = { ...textDiff, truncated: true };
    render(<ChangeDetail payload={detailPayload} getDiff={() => truncated} initialOpenFiles={{ [mountedChangesFixture.expected.file]: true }} />);
    expect(screen.getByText(mountedChangesFixture.expected.addedLine)).toBeTruthy();
    expect(screen.getByText(/truncated/i)).toBeTruthy();
  });
});
