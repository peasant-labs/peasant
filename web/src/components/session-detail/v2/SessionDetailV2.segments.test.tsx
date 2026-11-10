import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseTranscriptRouteQuery, type ProjectHash } from '@/lib/navigation/projectRoutes';
import { parseStrictYAML, requireExactRequiredFields, requireRecord } from '@/test/strictYaml';
import { PublishProvider } from '@/contexts/PublishContext';
import { SessionDetailV2 } from './SessionDetailV2';

const PROJECT_HASH = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' as ProjectHash;
const SESSION_ID = 'sess-segment-host';
const CASES_PATH = process.env.PEASANT_TRANSCRIPT_SEGMENTS_CASES
  ?? resolve(process.cwd(), 'scripts/visual/testdata/transcript-segments.yaml');
const MANIFEST_PATH = resolve(process.cwd(), 'scripts/visual/testdata/transcript-segments.manifest.yaml');

type SegmentPlan = { index: number; turns: number[] };
type WindowPlan = { requestedAnchor: number; anchorTurn: number; segments: number[] };
type SegmentCase = {
  name: string;
  revision: string;
  nextRevision?: string;
  turn: number | null;
  totalSegments: number;
  segments: SegmentPlan[];
  window?: WindowPlan;
  actions: string[];
  reads: string[];
  absent: string[];
  boundaries: string | null;
  mountedTurns: number[];
  notIndexed: boolean;
  segmentMode: boolean;
  clippedContent?: string;
  preBoundContent?: string;
  heldTurn?: number;
  expectError?: boolean;
  clearedTurns?: number[];
};

const reads: string[] = [];
const scanRequests = vi.hoisted(() => vi.fn());
let revision = '7';
let servePreBoundInline = false;
let failNextDirectory = false;
let activeCase: SegmentCase | null = null;

vi.mock('@/lib/share/publishing', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/share/publishing')>()),
  ...(await import('@/test/fixtures/publishingIdle')).PUBLISHING_IDLE,
}));
vi.mock('@/lib/share/redactionScan', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/share/redactionScan')>();
  return { ...actual, useRedactionPipeline: (...args: Parameters<typeof actual.useRedactionPipeline>) => {
    scanRequests(args[0]);
    return actual.useRedactionPipeline(...args);
  } };
});
vi.mock('next/navigation', () => ({
  useSearchParams: () => currentSearch,
  usePathname: () => `/projects/${PROJECT_HASH}/${SESSION_ID}`,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
}));
vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({
    data: channelData,
    channelFault: null,
    connectionFault: null,
    connected: true,
    error: null,
    refresh: vi.fn(),
  }),
}));
vi.mock('./lib/useEntryLabels', () => ({
  useEntryLabels: () => ({ entryTypes: [], labelsByEntry: new Map(), addLabel: vi.fn() }),
}));
vi.mock('@/hooks/useTheme', () => ({
  useTheme: () => ({ theme: 'dark', setTheme: vi.fn(), toggle: vi.fn() }),
}));
vi.mock('@peasant-labs/fairtrade/graph', () => ({ TrajectoryGraph: () => null }));

let currentSearch = new URLSearchParams();
let channelData: unknown;

function loadCases(): SegmentCase[] {
  const manifest = requireRecord(parseStrictYAML(readFileSync(MANIFEST_PATH, 'utf8'), 'transcript segment manifest'), 'transcript segment manifest');
  requireExactRequiredFields(manifest, ['expectedCaseCount', 'requiredNames'], 'transcript segment manifest');
  const names = manifest.requiredNames;
  if (!Array.isArray(names) || names.some((name) => typeof name !== 'string' || name.length === 0) || new Set(names).size !== names.length) {
    throw new Error('transcript segment manifest requiredNames must be unique nonempty strings');
  }
  if (manifest.expectedCaseCount !== names.length) throw new Error('transcript segment manifest count does not match requiredNames');
  const root = requireRecord(parseStrictYAML(readFileSync(CASES_PATH, 'utf8'), 'transcript segment cases'), 'transcript segment cases');
  requireExactRequiredFields(root, ['servePreBoundInline', 'requiredNames', 'cases'], 'transcript segment cases');
  if (root.servePreBoundInline !== false && root.servePreBoundInline !== true) {
    throw new Error('transcript segment cases servePreBoundInline must be a boolean');
  }
  servePreBoundInline = root.servePreBoundInline;
  if (JSON.stringify(root.requiredNames) !== JSON.stringify(names)) throw new Error('transcript segment cases requiredNames must match the manifest');
  if (!Array.isArray(root.cases) || root.cases.length !== names.length) throw new Error('transcript segment cases must match the required-name inventory');
  return root.cases.map((value, index) => {
    const row = requireRecord(value, `transcript segment cases[${index}]`);
    const fields = ['name', 'revision', 'nextRevision', 'turn', 'totalSegments', 'segments', 'window', 'actions', 'reads', 'absent', 'boundaries', 'mountedTurns', 'notIndexed', 'segmentMode'];
    if (row.name === 'per-field-bound') fields.push('clippedContent', 'preBoundContent');
    if (row.name === 'load-more-prepends') fields.push('heldTurn');
    if (row.name === 'revision-directory-fails') fields.push('clearedTurns', 'expectError');
    requireExactRequiredFields(row, fields, `transcript segment cases[${index}]`);
    if (row.name !== names[index]) throw new Error(`transcript segment cases[${index}] is ${String(row.name)}, want ${names[index]}`);
    return row as SegmentCase;
  });
}

function turnWire(index: number, plan: SegmentCase) {
  const bounded = plan.name === 'per-field-bound';
  if (bounded && (typeof plan.clippedContent !== 'string' || typeof plan.preBoundContent !== 'string' || !plan.preBoundContent.startsWith(plan.clippedContent) || plan.preBoundContent.length <= plan.clippedContent.length)) {
    throw new Error('per-field bound case must record a clipped prefix and a longer pre-bound body');
  }
  const content = bounded ? (servePreBoundInline ? plan.preBoundContent : plan.clippedContent) : `turn ${index}`;
  return {
    index,
    role: index % 2 === 0 ? 'user' : 'assistant',
    depth: 0,
    content,
    timestamp: '2026-07-15T08:00:00Z',
    toolCalls: [],
    truncated: bounded ? !servePreBoundInline : false,
    byteLength: bounded ? plan.preBoundContent!.length : 32,
  };
}

function segmentBody(plan: SegmentPlan, total: number, source: SegmentCase) {
  const turns = plan.turns.map((index) => turnWire(index, source));
  return {
    index: plan.index,
    revision,
    firstTurn: plan.turns[0],
    lastTurn: plan.turns[plan.turns.length - 1],
    byteLength: turns.reduce((sum, turn) => sum + turn.byteLength, 0),
    hasOlder: plan.index > 0,
    hasNewer: plan.index < total - 1,
    turns,
  };
}

function directoryBody(plan: SegmentCase) {
  return {
    sessionId: SESSION_ID,
    revision,
    totalTurns: plan.segments.reduce((sum, segment) => sum + segment.turns.length, 0),
    totalSegments: plan.totalSegments,
    segments: plan.segments.map((segment) => ({
      index: segment.index,
      firstTurn: segment.turns[0],
      lastTurn: segment.turns[segment.turns.length - 1],
      byteLength: segment.turns.length * 32,
    })),
  };
}

function windowBody(plan: SegmentCase, anchor: number) {
  const declared = plan.window;
  if (!declared) throw new Error(`${plan.name} has no window to answer anchor ${anchor}`);
  const segments = declared.segments;
  const first = plan.segments.find((segment) => segment.index === segments[0]);
  const last = plan.segments.find((segment) => segment.index === segments[segments.length - 1]);
  return {
    sessionId: SESSION_ID,
    revision,
    requestedAnchor: anchor,
    anchorTurn: declared.anchorTurn,
    span: 10,
    firstTurn: first?.turns[0] ?? declared.anchorTurn,
    lastTurn: last?.turns[last.turns.length - 1] ?? declared.anchorTurn,
    segments,
    hasOlder: (segments[0] ?? 0) > 0,
    hasNewer: (segments[segments.length - 1] ?? 0) < plan.totalSegments - 1,
  };
}

function classify(url: string): string | null {
  if (url.includes('/transcript/turns')) return 'turns';
  if (url.includes('/transcript/window')) return 'window';
  const segment = url.match(/\/transcript\/segments\/(\d+)/);
  if (segment) return `segment:${segment[1]}`;
  if (url.includes('/transcript/segments')) return 'directory';
  return null;
}

function detail(plan: SegmentCase) {
  return {
    id: SESSION_ID,
    project: 'alpha-project',
    harness: 'claude-code',
    startTime: '2026-07-15T08:00:00Z',
    endTime: '2026-07-15T08:01:00Z',
    durationMins: 1,
    totalTokens: 12,
    tokensIn: 5,
    tokensOut: 7,
    turnCount: Math.max(plan.totalSegments, 1),
    toolCallCount: 0,
    revision,
    turns: [],
    title: 'segment host',
  };
}

function TestDetail({ turn }: { turn: number | null }) {
  const params = new URLSearchParams();
  if (turn != null) params.set('turn', String(turn));
  if (activeCase?.name === 'revision-refetch-segment-zero') params.set('publish', 'open');
  currentSearch = params;
  const routeQuery = parseTranscriptRouteQuery(params);
  if (!routeQuery) throw new Error('route query must be valid');
  return (
    <PublishProvider>
      <SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} />
    </PublishProvider>
  );
}

async function answer(input: RequestInfo | URL): Promise<Response> {
  const url = String(input);
  const kind = classify(url);
  if (kind) reads.push(kind);
  const plan = activeCase;
  if (!plan) return new Response(JSON.stringify({ error: 'no case' }), { status: 500 });
  if (kind === 'directory') {
    if (failNextDirectory) {
      failNextDirectory = false;
      return new Response(JSON.stringify({ error: 'the directory read failed' }), { status: 500 });
    }
    return Response.json(directoryBody(plan));
  }
  if (kind === 'window') {
    const anchor = Number(new URL(url).searchParams.get('anchor'));
    return Response.json(windowBody(plan, anchor));
  }
  const segment = url.match(/\/transcript\/segments\/(\d+)/);
  if (segment) {
    const found = plan.segments.find((item) => item.index === Number(segment[1]));
    if (!found) return new Response(JSON.stringify({ error: `segment ${segment[1]} is not in ${plan.name}` }), { status: 404 });
    return Response.json(segmentBody(found, plan.totalSegments, plan));
  }
  return new Response(JSON.stringify({ error: 'not a transcript read' }), { status: 404 });
}

function assertReads(plan: SegmentCase) {
  expect([...reads].sort(), `${plan.name} reads`).toEqual([...plan.reads].sort());
  for (const absent of plan.absent) expect(reads, `${plan.name} must not read ${absent}`).not.toContain(absent);
}

describe('SessionDetailV2 segmented transcript', () => {
  const cases = loadCases();

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    reads.length = 0;
    failNextDirectory = false;
    activeCase = null;
    scanRequests.mockClear();
  });

  it('covers the required-name inventory exactly', () => {
    expect(cases.map((row) => row.name)).toEqual(loadCases().map((row) => row.name));
  });

  for (const plan of cases) {
    it(plan.name, async () => {
      activeCase = plan;
      revision = plan.revision;
      channelData = detail(plan);
      vi.stubGlobal('fetch', vi.fn(answer));
      const view = render(<TestDetail turn={plan.turn} />);
      if (plan.notIndexed) {
        await waitFor(() => expect(view.getByTestId('session-not-indexed')).toBeInTheDocument());
        expect(view.container.querySelector('[data-segment-mode]')).toBeNull();
        assertReads(plan);
        return;
      }
      await waitFor(() => expect(view.container.querySelector('[data-segment-mode="true"]')).not.toBeNull());
      if (plan.name === 'revision-refetch-segment-zero') {
        await waitFor(() => expect(scanRequests).toHaveBeenCalledWith([{ id: SESSION_ID, version: `revision:${plan.revision}` }]));
      }
      for (const action of plan.actions) {
        if (action === 'click-newer' || action === 'click-older') {
          const side = action === 'click-newer' ? 'newer' : 'older';
          const button = view.container.querySelector(`[data-segment-boundary="${side}"] button`);
          if (!(button instanceof HTMLButtonElement)) throw new Error(`${plan.name} has no ${side} load more control`);
          fireEvent.click(button);
          continue;
        }
        if (action === 'repeat-jump') {
          const before = reads.filter((read) => read.startsWith('segment:')).length;
          const again = plan.mountedTurns.find((turn) => turn !== plan.turn) ?? plan.turn;
          view.rerender(<TestDetail turn={again} />);
          await waitFor(() => expect(reads.filter((read) => read === 'window').length).toBeGreaterThan(1));
          expect(reads.filter((read) => read.startsWith('segment:')).length, plan.name).toBe(before);
          continue;
        }
        if (action === 'advance-revision' || action === 'fail-revision') {
          if (action === 'fail-revision') failNextDirectory = true;
          revision = plan.nextRevision ?? revision;
          channelData = detail(plan);
          view.rerender(<TestDetail turn={plan.turn} />);
          if (plan.name === 'revision-refetch-segment-zero') {
            await waitFor(() => expect(scanRequests).toHaveBeenCalledWith([{ id: SESSION_ID, version: `revision:${plan.nextRevision}` }]));
          }
        }
      }
      if (plan.expectError) {
        await waitFor(() => expect(view.getByText('transcript unavailable')).toBeInTheDocument());
        for (const turn of plan.clearedTurns ?? []) {
          expect(view.container.querySelector(`[data-turn="${turn}"]`), `${plan.name} cleared turn ${turn}`).toBeNull();
        }
        assertReads(plan);
        return;
      }
      await waitFor(() => {
        const node = view.container.querySelector('[data-segment-mode]');
        expect(node?.getAttribute('data-segment-boundaries')).toBe(plan.boundaries);
      });
      const viewer = view.container.querySelector('[data-segment-mode="true"]');
      expect(viewer).not.toBeNull();
      expect(view.queryByTestId('session-not-indexed')).toBeNull();
      const bound = Number(viewer?.getAttribute('data-mounted-turn-bound'));
      expect(bound, `${plan.name} mounted bound`).toBe(plan.mountedTurns.length);
      const cards = view.container.querySelectorAll('[data-turn]');
      expect(cards.length).toBeLessThanOrEqual(bound);
      for (const turn of plan.mountedTurns) {
        expect(view.container.querySelector(`[data-turn="${turn}"]`), `${plan.name} turn ${turn}`).not.toBeNull();
      }
      if (plan.name === 'jump-window') {
        expect(reads, `${plan.name} order`).toEqual(plan.reads);
      }
      if (plan.heldTurn != null) {
        expect(view.container.querySelector(`[data-turn="${plan.heldTurn}"]`), `${plan.name} held turn`).not.toBeNull();
      }
      if (plan.name === 'per-field-bound') {
        const card = view.container.querySelector(`[data-turn="${plan.mountedTurns[0]}"]`);
        const text = card?.textContent ?? '';
        const clipped = plan.clippedContent ?? '';
        const hidden = (plan.preBoundContent ?? '').slice(clipped.length);
        expect(text, 'per-field bound').toContain(clipped);
        expect(text, 'per-field bound').not.toContain(hidden);
        expect(view.getByText(/this turn's text is truncated/), 'per-field bound').toBeTruthy();
      }
      assertReads(plan);
    });
  }
});

type HistoryCase = { name: string; segmentCase: string; actions: string[]; reads: number };
const historyFixture = parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/earlier_history.yaml'), 'utf8'), 'earlier history') as {
  requiredNames: string[]; cases: HistoryCase[]; history: { state: string; content: string };
};
const historyManifest = parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/testdata/earlier_history.manifest.yaml'), 'utf8'), 'earlier history manifest') as { requiredNames: string[] };

describe('explicit earlier history on the mounted transcript', () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); reads.length = 0; activeCase = null; });

  it('keeps every required scenario', () => {
    expect(historyFixture.requiredNames).toEqual(historyManifest.requiredNames);
    expect(historyFixture.cases.map((row) => row.name)).toEqual(historyManifest.requiredNames);
  });

  for (const testCase of historyFixture.cases) {
    it(testCase.name, async () => {
      const plan = loadCases().find((candidate) => candidate.name === testCase.segmentCase);
      if (!plan) throw new Error(`missing segment case ${testCase.segmentCase}`);
      activeCase = plan;
      revision = plan.revision;
      channelData = detail(plan);
      let durableReads = 0;
      let fail = false;
      let releaseRead: (() => void) | undefined;
      let oldSignal: AbortSignal | null | undefined;
      let search = plan.turn == null ? '' : `turn=${plan.turn}`;
      const component = () => {
        currentSearch = new URLSearchParams(search);
        const routeQuery = parseTranscriptRouteQuery(currentSearch);
        if (!routeQuery) throw new Error('invalid history route');
        return <PublishProvider><SessionDetailV2 sessionId={SESSION_ID} projectHash={PROJECT_HASH} projectName="alpha-project" routeQuery={routeQuery} /></PublishProvider>;
      };
      vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input).endsWith(`/api/v1/sessions/${SESSION_ID}`)) {
          durableReads += 1;
          if (fail) { fail = false; return Response.json({ error: 'fixture read refused' }, { status: 503 }); }
          if (testCase.name === 'history-disclosure' || (testCase.name === 'stale-response-after-revision' && durableReads === 1)) {
            oldSignal = init?.signal;
            await new Promise<void>((resolve) => { releaseRead = resolve; });
          }
          return Response.json({
            ...detail(plan),
            turns: [turnWire(0, plan)],
            earlierHistory: [{ state: historyFixture.history.state, turns: [{ ...turnWire(40, plan), content: historyFixture.history.content, truncated: undefined, byteLength: undefined }] }],
          });
        }
        return answer(input);
      }));
      const view = render(component());
      await waitFor(() => expect(view.container.querySelector('[data-segment-mode="true"]')).not.toBeNull());
      expect(durableReads).toBe(0);
      for (const action of testCase.actions) {
        if (action === 'fail') fail = true;
        if (action === 'load-pending') {
          fireEvent.click(view.getByRole('button', { name: 'show earlier history' }));
          expect(view.getByText(/loading the session to read earlier history/)).toBeVisible();
        } else if (action === 'finish-stale') {
          expect(oldSignal?.aborted).toBe(true);
          if (!releaseRead) throw new Error('history read did not start');
          await act(async () => { releaseRead?.(); });
          expect(view.container.querySelector('[data-earlier-section]')).toBeNull();
        } else if (action === 'load' || action === 'fail' || action === 'retry') {
          fireEvent.click(view.getByRole('button', { name: action === 'retry' ? 'try loading earlier history again' : 'show earlier history' }));
          if (testCase.name === 'history-disclosure') {
            expect(view.getByText(/loading the session to read earlier history/)).toBeVisible();
            const loadingButton = view.getByRole('button', { name: 'loading earlier history…' });
            expect(loadingButton).toBeDisabled();
            fireEvent.click(loadingButton);
            expect(durableReads).toBe(1);
            if (!releaseRead) throw new Error('history read did not start');
            releaseRead();
          }
          if (action === 'fail') await waitFor(() => expect(view.getByRole('alert')).toHaveTextContent('fixture read refused'));
          else await waitFor(() => expect(view.container.querySelector('[data-earlier-section="earlier-0"]')).not.toBeNull());
        } else if (action === 'unrelated-segment') {
          const button = view.container.querySelector('[data-segment-boundary="newer"] button');
          if (!(button instanceof HTMLButtonElement)) throw new Error('missing load-more control');
          fireEvent.click(button);
          await waitFor(() => expect(reads).toContain('segment:1'));
          expect(view.getByRole('alert')).toHaveTextContent('fixture read refused');
        } else if (action === 'revision') {
          revision = '8'; channelData = detail(plan); view.rerender(component());
          await waitFor(() => expect(view.getByRole('button', { name: 'show earlier history' })).toBeInTheDocument());
        } else if (action === 'disclose' || action === 'disclose-again') {
          search = action === 'disclose' ? 'earlier=earlier-0' : '';
          view.rerender(component());
          if (action === 'disclose') {
            expect(view.getByText(historyFixture.history.content)).toBeVisible();
            expect(view.getByText(/state: uncertain_unresolved/)).toBeVisible();
            const stream = view.container.querySelector('.txn-stream');
            const history = stream?.querySelector('[data-earlier-section]');
            const turn = stream?.querySelector('[data-turn="0"]');
            expect(history && turn && (history.compareDocumentPosition(turn) & Node.DOCUMENT_POSITION_FOLLOWING)).toBeTruthy();
          }
        } else if (action === 'missing') {
          search = 'earlier=earlier-1'; view.rerender(component());
          expect(view.getByText(/section earlier-1 was not returned/)).toBeVisible();
        } else if (action === 'limitation') {
          expect(view.getByText(/transcript page cannot load more of a truncated turn/)).toBeVisible();
          expect(view.getByText('the transcript page cannot load more of this turn.')).toBeVisible();
          expect(view.queryByRole('button', { name: 'load this turn in full' })).toBeNull();
        }
      }
      expect(durableReads).toBe(testCase.reads);
    });
  }
});
