import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { SessionDetailV2, UNTITLED_SESSION_TITLE } from './SessionDetailV2';

// The mounted hero heading. The shared viewer renders `session.title` when the
// host supplies one and otherwise derives a heading from the first recorded
// prompt. A recorded prompt frequently opens with harness markup, so the
// derived heading rendered that markup to the reader. These cases mount the
// real viewer and assert the rendered heading text, so the host's decision to
// always supply a title is measured on the production surface.

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const SESSION_ID = 'sess-hero-title';
const PATHNAME = `/projects/alpha-project/${SESSION_ID}`;

const caseFields = ['name', 'qualitySessionPresent', 'generatedTitle', 'firstTurnContent', 'expectedTitle'] as const;
const loaderMutationFields = ['name', 'target', 'find', 'replace', 'expectedError'] as const;

type HeroTitleCase = {
  name: string;
  qualitySessionPresent: boolean;
  generatedTitle: string;
  firstTurnContent: string;
  expectedTitle: string;
};
type LoaderMutation = { name: string; target: 'manifest' | 'cases'; find: string; replace: string; expectedError: string };
type HeroTitleFixture = { cases: HeroTitleCase[]; loaderMutations: LoaderMutation[] };

const manifestSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/hero_title.manifest.yaml'), 'utf8');
const casesSource = readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/hero_title.yaml'), 'utf8');

function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const count = source.split(find).length - 1;
  if (count !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${count}`);
  return source.replace(find, replacement);
}

function loadFixture(manifestText = manifestSource, casesText = casesSource): HeroTitleFixture {
  const manifest = requireRecord(parseStrictYAML(manifestText, 'hero title manifest'), 'hero title manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames', 'expectedLoaderMutationCount', 'loaderMutations'], 'hero title manifest');
  if (!Number.isSafeInteger(manifest.expectedCaseCount) || !Array.isArray(manifest.requiredNames) || !Number.isSafeInteger(manifest.expectedLoaderMutationCount) || !Array.isArray(manifest.loaderMutations)) {
    throw new Error('hero title manifest requires independent case and loader-mutation inventories');
  }
  const requiredNames = manifest.requiredNames as unknown[];
  if (requiredNames.some((value) => typeof value !== 'string' || value.length === 0) || new Set(requiredNames).size !== requiredNames.length) {
    throw new Error('hero title manifest requiredNames must be unique nonempty strings');
  }
  const loaderMutations = manifest.loaderMutations.map((row, index) => {
    const mutation = requireRecord(row, `hero title manifest.loaderMutations[${index}]`);
    requireExactRequiredFields(mutation, loaderMutationFields, `hero title manifest.loaderMutations[${index}]`);
    if (!['manifest', 'cases'].includes(mutation.target as string) || ['name', 'find', 'replace', 'expectedError'].some((field) => typeof mutation[field] !== 'string' || (mutation[field] as string).length === 0)) {
      throw new Error(`hero title manifest.loaderMutations[${index}] has invalid values`);
    }
    return mutation as LoaderMutation;
  });
  if (loaderMutations.length !== manifest.expectedLoaderMutationCount) throw new Error('hero title manifest loader-mutation count does not match its independent inventory');

  const root = requireRecord(parseStrictYAML(casesText, 'hero title cases'), 'hero title cases');
  requireExactRequiredFields(root, ['cases'], 'hero title cases');
  if (!Array.isArray(root.cases)) throw new Error('hero title cases must be an array');
  const rows = root.cases.map((row, index) => requireRecord(row, `hero title cases[${index}]`));
  requireUniqueNames(rows, 'hero title cases');
  rows.forEach((row, index) => {
    requireExactRequiredFields(row, caseFields, `hero title cases[${index}]`);
    if (typeof row.qualitySessionPresent !== 'boolean' || typeof row.generatedTitle !== 'string' || typeof row.firstTurnContent !== 'string' || typeof row.expectedTitle !== 'string' || (row.expectedTitle as string).length === 0) {
      throw new Error(`hero title cases[${index}] has invalid field types`);
    }
  });
  const names = rows.map((row) => row.name as string);
  if (rows.length !== manifest.expectedCaseCount || requiredNames.length !== names.length || requiredNames.some((name) => !names.includes(name as string)) || names.some((name) => !requiredNames.includes(name))) {
    throw new Error('hero title cases do not match their independent manifest');
  }
  return { cases: rows as HeroTitleCase[], loaderMutations };
}

const fixture = loadFixture();

let sessionDetailData: unknown;
let qualityData: unknown;

vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => PATHNAME,
  useRouter: () => ({ replace: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: (subscription: { topic: string }) => ({
    data: subscription.topic === 'session_detail' ? sessionDetailData : qualityData,
    connected: true,
    error: null,
  }),
}));
vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));
vi.mock('@peasant-labs/transcript-browser', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@peasant-labs/transcript-browser')>();
  return { ...actual, TrajectoryGraph: () => null, annotateTranscript: () => [], computePersonalMedians: () => undefined };
});

const BASE_QUALITY_SESSION = {
  id: SESSION_ID,
  project: 'alpha-project',
  date: '2026-07-16',
  durationMinutes: 1,
  totalTokens: 1,
  inputTokens: 1,
  outputTokens: 0,
  turnCount: 1,
  toolCalls: 0,
  filesTouched: 0,
  linesChanged: 0,
  retryLoops: 0,
  retryTokensWasted: 0,
  withinSessionReverts: 0,
  explorationRatio: 0,
  discoveryTurns: 0,
  scope: 'focused',
  scopeBreadth: 0,
  signalDensity: 0,
  specQualityScore: 0,
  outcome: 'resolved',
};

function TestDetail() {
  const routeQuery = parseTranscriptRouteQuery(new URLSearchParams());
  if (!routeQuery) throw new Error('hero title route query must be valid');
  return <SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />;
}

beforeEach(() => {
  window.localStorage.clear();
  if (typeof HTMLElement.prototype.scrollIntoView !== 'function') {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => undefined });
  }
});

afterEach(() => {
  cleanup();
  sessionDetailData = undefined;
  qualityData = undefined;
});

describe('SessionDetailV2 — the mounted hero heading', () => {
  it('rejects every strict loader mutation from the independent manifest', () => {
    for (const mutation of fixture.loaderMutations) {
      const original = mutation.target === 'manifest' ? manifestSource : casesSource;
      const mutated = replaceExactlyOnce(original, mutation.find, mutation.replace, mutation.name);
      expect(() => loadFixture(mutation.target === 'manifest' ? mutated : manifestSource, mutation.target === 'cases' ? mutated : casesSource), mutation.name).toThrow(new RegExp(mutation.expectedError));
    }
  });

  it('pins the placeholder heading the cases expect', () => {
    expect(UNTITLED_SESSION_TITLE).toBe('Untitled session');
  });

  for (const row of fixture.cases) {
    it(row.name, async () => {
      sessionDetailData = {
        id: SESSION_ID,
        project: 'alpha-project',
        harness: 'claude-code',
        startTime: '2026-07-16T08:00:00Z',
        endTime: '2026-07-16T08:01:00Z',
        durationMins: 1,
        totalTokens: 12,
        tokensIn: 5,
        tokensOut: 7,
        turnCount: 2,
        toolCallCount: 0,
        turns: [
          { index: 0, role: 'user', depth: 0, content: row.firstTurnContent, timestamp: '2026-07-16T08:00:00Z', toolCalls: [] },
          { index: 1, role: 'assistant', depth: 0, content: 'the branch has no staged changes', timestamp: '2026-07-16T08:00:30Z', toolCalls: [] },
        ],
      };
      qualityData = row.qualitySessionPresent
        ? { sessions: [{ ...BASE_QUALITY_SESSION, title: row.generatedTitle }] }
        : { sessions: [] };

      const view = render(<TestDetail />);
      await waitFor(() => expect(view.container.querySelector('.txn-title')).toBeInTheDocument());
      const heading = view.container.querySelector('.txn-title');
      expect(heading?.textContent).toBe(row.expectedTitle);
      // The recorded prompt is still in the transcript; it must never be the
      // heading, so the heading itself may carry no markup delimiter.
      expect(heading?.textContent ?? '').not.toContain('<');
    });
  }
});
