import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { NodeRail, ProjectRail } from './MapRail';
import { makeCommitRef, makeSession, NODE_DETAIL, PROJECT_HASH } from '../lib/test-fixtures';
import {
  parseTranscriptRoute,
  parseTranscriptRouteQuery,
  transcriptHref,
  RouteOrigin,
} from '@/lib/navigation/projectRoutes';
import type { SessionSummary } from '@/types/messages';
import type { Harness } from '@peasant-labs/schema';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { strikeMountedWebFixture } from '@/test/fixtures/strikeMountedWeb';
import { assertCommitRowLink, loadCommitRowManifest, proveCommitRowMutation } from './commitRowFixtures';

/**
 * The "Conversations that built this" commit-row section (`MapRail.tsx`
 * `NodeRail`) previously read only `row.commit.hasSession` (a boolean); this
 * proves the production `NodeRail` component now reads `row.commit.sessionIds`
 * and renders a per-session name + DS harness mark + transcript link, sourced
 * from the fixture corpus in ./testdata/commit_row_sessions.yaml.
 */

type ExpectedRow = {
  sessionId: string;
  expectedName: string;
  expectedHarness: string | null;
};

type Case = {
  name: string;
  commitSessionIds: string[];
  knownSessions: Array<{ id: string; harness: string; preview: string }>;
  expectedFallbackText: string | null;
  expectedRows: ExpectedRow[];
};

const manifestSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/[[...segments]]/testdata/commit_row_sessions.manifest.yaml'),
  'utf8',
);
const casesSource = readFileSync(
  resolve(process.cwd(), 'src/app/map/[[...segments]]/testdata/commit_row_sessions.yaml'),
  'utf8',
);

const commitRowManifest = loadCommitRowManifest(
  manifestSource,
  'commit-row session fixtures manifest',
);

function loadCases(source: string = casesSource): { cases: Case[] } {
  const root = requireRecord(parseStrictYAML(source, 'commit-row session fixtures'), 'commit-row session fixtures');
  requireExactRequiredFields(root, ['cases'], 'commit-row session fixtures');
  if (!Array.isArray(root.cases)) throw new Error('commit-row session fixtures.cases must be an array');

  const rows = root.cases.map((row, index) => requireRecord(row, `commit-row session fixtures.cases[${index}]`));
  requireUniqueNames(rows, 'commit-row session fixtures.cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(
      row,
      ['name', 'commitSessionIds', 'knownSessions', 'expectedFallbackText', 'expectedRows'],
      `commit-row session fixtures.cases[${index}]`,
    );
    if (!Array.isArray(row.commitSessionIds) || !Array.isArray(row.knownSessions) || !Array.isArray(row.expectedRows)) {
      throw new Error(`commit-row session fixtures.cases[${index}] requires array fields`);
    }
  });

  const names = rows.map((row) => row.name);
  if (
    rows.length !== commitRowManifest.expectedCount ||
    JSON.stringify(names) !== JSON.stringify(commitRowManifest.requiredNames)
  ) {
    throw new Error('commit-row session fixtures do not match their independent manifest');
  }

  return { cases: rows as unknown as Case[] };
}

const casesFixture = loadCases();
const CASES = casesFixture.cases;

function renderNodeRail(testCase: Case) {
  const detail = {
    ...NODE_DETAIL,
    shapedBy: [],
    recentCommits: [makeCommitRef(
      'deadbeef00',
      'landing commit',
      NODE_DETAIL.lastTouchMs ?? null,
      testCase.commitSessionIds,
    )],
  };
  const sessions: SessionSummary[] = testCase.knownSessions.map((s) =>
    makeSession({ id: s.id, harness: s.harness as Harness, preview: s.preview }),
  );

  render(
    <NodeRail
      projectHash={PROJECT_HASH}
      projectName="alpha-project"
      nodeId="internal/ingest"
      detail={detail}
      error={null}
      sessions={sessions}
      coupling={[]}
      onSelectNode={vi.fn()}
      onClose={vi.fn()}
      nowMs={NODE_DETAIL.lastTouchMs ?? Date.now()}
    />,
  );

  return screen.getByText('landing commit').closest('div') as HTMLElement;
}

function findCommitRowLink(row: HTMLElement, sessionId: string): HTMLAnchorElement {
  const href = transcriptHref(PROJECT_HASH, sessionId, {
    origin: RouteOrigin.Map,
    originNode: 'internal/ingest',
  });
  const link = within(row).getAllByRole('link').find((candidate) => candidate.getAttribute('href') === href);
  if (!link) {
    throw new Error(`commit-row session fixtures row is missing a link for ${sessionId}`);
  }
  return link as HTMLAnchorElement;
}

describe('NodeRail commit-row session list', () => {
  afterEach(() => cleanup());

  it('loads exactly the independently-manifested fixture corpus', () => {
    expect(CASES).toHaveLength(4);
  });

  for (const testCase of CASES) {
    it(testCase.name, () => {
      const row = renderNodeRail(testCase);

      if (testCase.expectedFallbackText) {
        expect(within(row).getByText(testCase.expectedFallbackText)).toBeInTheDocument();
        expect(within(row).queryByRole('list')).not.toBeInTheDocument();
      } else {
        expect(within(row).queryByText('no AI conversation captured')).not.toBeInTheDocument();
      }

      for (const expected of testCase.expectedRows) {
        const link = findCommitRowLink(row, expected.sessionId);
        expect(link).toHaveAttribute(
          'href',
          transcriptHref(PROJECT_HASH, expected.sessionId, {
            origin: RouteOrigin.Map,
            originNode: 'internal/ingest',
          }),
        );
        assertCommitRowLink(link, expected);
      }

      // Exactly the expected count of session rows render; no extra rows,
      // no rows silently dropped.
      expect(within(row).queryAllByRole('listitem')).toHaveLength(testCase.expectedRows.length);
    });
  }

  for (const mutation of commitRowManifest.mutations) {
    it(mutation.name, async () => {
      await proveCommitRowMutation(
        casesSource,
        mutation,
        loadCases,
        (fixture, currentMutation) => {
          const testCase = fixture.cases.find(({ name }) => name === currentMutation.caseName);
          if (!testCase) {
            throw new Error(`commit-row session fixtures mutation ${currentMutation.name} has no case named ${currentMutation.caseName}`);
          }
          const row = renderNodeRail(testCase);
          return findCommitRowLink(row, currentMutation.sessionId);
        },
        (fixture, currentMutation) => {
          const testCase = fixture.cases.find(({ name }) => name === currentMutation.caseName);
          if (!testCase) {
            throw new Error(`commit-row session fixtures mutation ${currentMutation.name} has no case named ${currentMutation.caseName}`);
          }
          const expected = testCase.expectedRows.find(({ sessionId }) => sessionId === currentMutation.sessionId);
          if (!expected) {
            throw new Error(`commit-row session fixtures mutation ${currentMutation.name} has no expected row for ${currentMutation.sessionId}`);
          }
          return expected;
        },
      );
    });
  }

  it('links the mounted Map conversation list to the canonical Strike detail route', () => {
    render(
      <ProjectRail
        projectHash={strikeMountedWebFixture.projectHash}
        projectName={strikeMountedWebFixture.projectName}
        sessions={[strikeMountedWebFixture.mapSession]}
        coverage={null}
        recentTasks={[]}
        recentTasksError={null}
        nowMs={strikeMountedWebFixture.mapCommit.timeMs}
      />,
    );

    const link = screen.getByRole('link', {
      name: `Open the conversation ${strikeMountedWebFixture.sessionDetail.id}`,
    });
    const destination = new URL(link.getAttribute('href') ?? '', 'https://peasant.invalid');
    expect(link).toHaveTextContent(strikeMountedWebFixture.expected.mapConversationTitle);
    expect(parseTranscriptRoute(destination.pathname)).toEqual({
      kind: 'canonical',
      projectHash: strikeMountedWebFixture.projectHash,
      sessionId: strikeMountedWebFixture.sessionDetail.id,
    });
    expect(parseTranscriptRouteQuery(destination.searchParams)).toMatchObject({
      origin: RouteOrigin.Map,
    });
  });

  it('renders canonical Strike identity and navigation in a mounted Map commit row', () => {
    const detail = {
      ...NODE_DETAIL,
      shapedBy: [],
      recentCommits: [makeCommitRef(
        strikeMountedWebFixture.mapCommit.hash,
        strikeMountedWebFixture.mapCommit.subject,
        strikeMountedWebFixture.mapCommit.timeMs,
        [strikeMountedWebFixture.sessionDetail.id],
      )],
    };
    render(
      <NodeRail
        projectHash={strikeMountedWebFixture.projectHash}
        projectName={strikeMountedWebFixture.projectName}
        nodeId="internal/ingest"
        detail={detail}
        error={null}
        sessions={[strikeMountedWebFixture.mapSession]}
        coupling={[]}
        onSelectNode={vi.fn()}
        onClose={vi.fn()}
        nowMs={strikeMountedWebFixture.mapCommit.timeMs}
      />,
    );

    const commit = screen.getByText(strikeMountedWebFixture.mapCommit.subject).closest('div');
    if (!commit) throw new Error('mounted Strike Map fixture did not render its commit row');
    const link = within(commit).getByRole('link', {
      name: new RegExp(strikeMountedWebFixture.expected.mapConversationTitle, 'i'),
    });
    expect(link).toHaveAttribute(
      'href',
      transcriptHref(
        strikeMountedWebFixture.projectHash,
        strikeMountedWebFixture.sessionDetail.id,
        { origin: RouteOrigin.Map, originNode: 'internal/ingest' },
      ),
    );
    expect(within(link).getByText(strikeMountedWebFixture.sessionDetail.harness).closest('.pv-name')).toBeInTheDocument();
    expect(link.querySelector('.brand')).toBeInTheDocument();
  });
});
