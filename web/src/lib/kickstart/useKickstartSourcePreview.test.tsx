import { act, cleanup, fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LocalKickstartPreviewOp, MessageType, parseLocalKickstartPreviewRequest, type LocalKickstartPreviewRequest } from '@peasant-labs/schema';
import { FirstRunProvider } from '@/hooks/useFirstRun';
import SelectionPage from '@/app/selection/page';
import { loadCases, readFixture, type FixtureCase } from '@/test/fixtures/kickstart';
import { API, KickstartServer, mountKickstart, runSteps, teardown, type Mounted, type RouteAnswer, type Step } from '@/test/kickstartHarness';
import { MockWebSocket } from '@/test/mockWebSocket';
import { buildFrame, type FrameRequest, type StreamSpec } from '@/test/fixtures/sourcePreviewStream';
import { PreviewNoticeTone, READ_AGAIN } from '@/lib/kickstart/sourcePreview';
import { ERROR_CARD_CLASS } from '@/components/ErrorCard';
import { PreviewSlotContent } from '@/components/kickstart/PreviewSlot';

vi.mock('next/navigation', () => ({
  usePathname: () => '/selection',
  useSearchParams: () => new URLSearchParams(),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));

/**
 * The raw session preview on the mounted /selection page: the production page,
 * preview hook, contract merge and transcript viewer, with only `fetch` and the
 * WebSocket replaced. Cases live in the fixture; this file plays them.
 */

const PATH = 'src/lib/kickstart/testdata/source_preview_stream.yaml';
const fixture = readFixture(PATH) as { streams: Record<string, StreamSpec> };

/** The keys this file reads on a step; every other key goes to the shared harness. */
interface PreviewStep {
  serve?: FrameRequest;
  expectSent?: { op: string; count: number };
  expectLastSent?: Record<string, unknown>;
  scroll?: true;
  unmount?: true;
  expectPrivacy?: PreviewPrivacy[];
  expectTurnText?: { index: number; drawn: boolean };
  clickTwice?: string;
  jump?: true;
  expectHighlight?: string;
  expectSessionMeta?: { reads?: string[]; lacks?: string[] };
  expectTurnLabels?: Record<string, string>;
  expectNoticeTone?: { tone: PreviewNoticeTone; text: string; readAgainInside?: boolean };
  expectStateInPane?: string;
  expectPreviewColumn?: PreviewColumnCheck;
}
/** What the preview column holds: the viewer alone, or one state in the viewer's slot. */
interface PreviewColumnCheck {
  region: string;
  holds: PreviewSlotContent;
  absentText: string[];
}
const PREVIEW_KEYS = Object.freeze({
  serve: true,
  expectSent: true,
  expectLastSent: true,
  scroll: true,
  unmount: true,
  expectPrivacy: true,
  expectTurnText: true,
  clickTwice: true,
  jump: true,
  expectHighlight: true,
  expectSessionMeta: true,
  expectTurnLabels: true,
  expectNoticeTone: true,
  expectStateInPane: true,
  expectPreviewColumn: true,
} satisfies Record<keyof PreviewStep, true>);
/** Where the stream must not reach: the closed set an `expectPrivacy` step names. */
const PreviewPrivacy = Object.freeze({
  Storage: 'storage',
  Url: 'url',
  Clipboard: 'clipboard',
  Requests: 'requests',
  Images: 'images',
});
type PreviewPrivacy = (typeof PreviewPrivacy)[keyof typeof PreviewPrivacy];
const PRIVACY: readonly string[] = Object.values(PreviewPrivacy);
/** The class prefix of the transcript viewer's own elements; its root carries one. */
const VIEWER_ROOT_PREFIX = 'txn';
const SLOT_CONTENTS: readonly string[] = Object.values(PreviewSlotContent);
const CASE_KEYS = ['name', 'stream', 'steps', 'server', 'fakeTimers'];

/** Every preview request the page put on any socket of this test, in order, each checked against the contract. */
function sentPreviews(): LocalKickstartPreviewRequest[] {
  return MockWebSocket.instances.flatMap((socket) => socket.sent.flatMap((text) => {
    const message: unknown = JSON.parse(text);
    if (message === null || typeof message !== 'object' || !('type' in message) || message.type !== MessageType.KickstartPreview) return [];
    return [parseLocalKickstartPreviewRequest('preview' in message ? message.preview : undefined)];
  }));
}

/** The marker word of a stream turn: unique to the stream, so finding it anywhere else is a leak. */
function markerOf(spec: StreamSpec, index: number): string {
  const text = spec.segments.flat()[index]?.text;
  const marker = text?.match(/synthetic-marker-[a-z]+/)?.[0];
  if (marker === undefined) throw new Error(`stream turn ${index} has no synthetic-marker word to look for`);
  return marker;
}

function markers(spec: StreamSpec): string[] {
  return spec.segments.flat().map((_, index) => markerOf(spec, index));
}

function control(name: string): HTMLElement {
  return screen.getByRole('button', { name: new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`, 'i') });
}

/** Where the conversation is: what a frame built now answers. */
interface Conversation {
  seq: number;
  loaded: number;
  unmount: () => void;
  href: string;
  history: { push: ReturnType<typeof vi.spyOn>; replace: ReturnType<typeof vi.spyOn> };
  clipboard: ReturnType<typeof vi.fn>;
  execCommand: ReturnType<typeof vi.fn>;
}

/** Scroll every scroller of the preview to its end, wheel it, and resize the window. */
async function scrollToEnd(mounted: Mounted, at: string): Promise<void> {
  await act(async () => {
    const viewer = mounted.container.querySelector('[data-preview-session]');
    if (viewer === null) throw new Error(`${at}: there is no preview to scroll`);
    for (const element of [viewer, ...viewer.querySelectorAll('*')]) {
      (element as HTMLElement).scrollTop = 1_000_000;
      fireEvent.scroll(element);
      fireEvent.wheel(element, { deltaY: 10_000 });
    }
    window.dispatchEvent(new Event('resize'));
    fireEvent.scroll(window);
  });
}

async function runPreviewStep(step: PreviewStep, spec: StreamSpec, state: Conversation, mounted: Mounted, at: string): Promise<void> {
  if (step.serve !== undefined) {
    const request = step.serve;
    const opens = sentPreviews().filter((sent) => sent.op === LocalKickstartPreviewOp.Open);
    const requestId = opens.at(-1)?.requestId ?? 'unsent';
    const seq = request.opened ? 0 : request.refuse ? 0 : state.seq + 1;
    const frame = buildFrame(spec, request, { seq, requestId, loaded: state.loaded });
    if (!request.otherCapture) {
      state.seq = seq;
      if (request.segment !== undefined && request.segment !== null) state.loaded = request.segment + 1;
    }
    await act(async () => {
      MockWebSocket.latest().simulateMessage(frame);
    });
  }
  if (step.clickTwice !== undefined) {
    const button = control(step.clickTwice);
    await act(async () => {
      fireEvent.click(button);
      fireEvent.click(button);
    });
  }
  if (step.scroll) await scrollToEnd(mounted, at);
  if (step.jump) {
    // A held segment is reached through the viewer's own scrubber, by pointer
    // and by keyboard; every segment the directory names is already held. The
    // scrubber pins once the trace scrolls past its header.
    await scrollToEnd(mounted, at);
    const viewer = mounted.container.querySelector('[data-preview-session]');
    const sliders = viewer === null ? [] : [...viewer.querySelectorAll<HTMLElement>('[role="slider"]')];
    expect(sliders.length, `${at}: the viewer draws a scrubber to jump with`).toBeGreaterThan(0);
    await act(async () => {
      for (const slider of sliders) {
        fireEvent.mouseDown(slider, { button: 0, clientX: 0 });
        fireEvent.mouseUp(window);
        for (const key of ['Home', 'End', 'ArrowLeft', 'ArrowRight']) fireEvent.keyDown(slider, { key });
      }
    });
  }
  if (step.unmount) {
    await act(async () => {
      state.unmount();
    });
  }
  if (step.expectSent !== undefined) {
    const { op, count } = step.expectSent;
    await waitFor(() => expect(sentPreviews().filter((sent) => sent.op === op).length, `${at}: preview ${op} requests sent`).toBe(count));
  }
  if (step.expectLastSent !== undefined) {
    const want = Object.fromEntries(Object.entries(step.expectLastSent).map(([key, value]) => [
      key,
      value === '$capture' ? spec.captureId : value === '$seq' ? state.seq : value,
    ]));
    await waitFor(() => expect(sentPreviews().at(-1), `${at}: the last preview request`).toMatchObject(want));
  }
  if (step.expectTurnText !== undefined) {
    const marker = markerOf(spec, step.expectTurnText.index);
    if (step.expectTurnText.drawn) {
      await waitFor(() => expect(mounted.container.textContent ?? '', `${at}: turn ${step.expectTurnText!.index} is drawn`).toContain(marker));
    } else {
      expect(mounted.container.textContent ?? '', `${at}: turn ${step.expectTurnText.index} is not drawn`).not.toContain(marker);
    }
  }
  if (step.expectHighlight !== undefined) {
    // unstring-allow: copy-query-literal — the tab label is fairtrade's own and fairtrade exports no member for it
    const tab = screen.queryAllByRole('tab', { name: /highlights/i })[0];
    if (tab !== undefined) {
      await act(async () => {
        fireEvent.click(tab);
      });
    }
    await waitFor(() => expect(screen.getAllByText(step.expectHighlight!, { exact: false }).length, `${at}: highlights say ${step.expectHighlight}`).toBeGreaterThan(0));
    const other = step.expectHighlight === 'final response' ? 'latest loaded response' : null;
    if (other !== null) expect(screen.queryAllByText(other, { exact: false }), `${at}: highlights do not say ${other}`).toHaveLength(0);
  }
  if (step.expectSessionMeta !== undefined) {
    const { reads = [], lacks = [] } = step.expectSessionMeta;
    // The viewer's metadata row, by its DOM class: the region's accessible name is Fairtrade's copy.
    const region = await waitFor(() => {
      const found = mounted.container.querySelector('.txn-meta');
      if (found === null) throw new Error(`${at}: the viewer drew no session metadata`);
      return found;
    });
    // Each figure is one item; read them word by word so `2 turns` never matches `12 turns`.
    const items = [...region.children].map((item) => (item.textContent ?? '').replace(/\s+/g, ' ').trim());
    for (const figure of reads) expect(items, `${at}: the header reads ${figure}`).toContain(figure);
    for (const word of lacks) {
      expect(items.filter((item) => item.split(' ').includes(word)), `${at}: the header shows no ${word} figure`).toEqual([]);
    }
  }
  if (step.expectTurnLabels !== undefined) {
    const want = step.expectTurnLabels;
    // Each turn's number, read from its own card: the article Fairtrade keys by
    // the turn's index, and the number in that card's heading.
    const read = () => Object.fromEntries(Object.keys(want).map((index) => {
      const card = mounted.container.querySelector(`[data-preview-session] article[data-turn="${index}"]`);
      return [index, card?.querySelector('.txn-turnnum')?.textContent?.replace(/\s+/g, '') ?? '(not drawn)'];
    }));
    await waitFor(() => expect(read(), `${at}: the turn numbers drawn`).toEqual(want));
  }
  if (step.expectNoticeTone !== undefined) {
    const { tone, text, readAgainInside } = step.expectNoticeTone;
    const pane = mounted.container.querySelector('[data-preview-session]') ?? mounted.container;
    const holder = await waitFor(() => {
      const found = [...mounted.container.querySelectorAll<HTMLElement>('[role="alert"], [role="status"]')]
        .filter((element) => (element.textContent ?? '').includes(text));
      if (found.length === 0) throw new Error(`${at}: no alert or status carries "${text}"`);
      return found.at(-1)!;
    });
    const card = holder.closest<HTMLElement>('[role="alert"]');
    if (tone === PreviewNoticeTone.Error) {
      expect(card, `${at}: "${text}" sits in an alert`).not.toBeNull();
      for (const name of ERROR_CARD_CLASS.split(' ')) expect(card!.classList.contains(name), `${at}: the alert is the app's error card (${name})`).toBe(true);
      if (readAgainInside) {
        expect(card!.querySelector('button')?.textContent, `${at}: read again sits in the error card`).toBe(READ_AGAIN);
      }
    } else if (tone === PreviewNoticeTone.Neutral) {
      expect(card, `${at}: "${text}" is not announced as an alert`).toBeNull();
      expect(holder.getAttribute('role'), `${at}: "${text}" is a status`).toBe('status');
      expect(pane.querySelectorAll('[role="alert"]').length, `${at}: the preview shows no error card`).toBe(0);
    } else {
      throw new Error(`${at}: expectNoticeTone names tone ${String(tone)}; it is one of ${Object.values(PreviewNoticeTone).join(', ')}`);
    }
  }
  if (step.expectStateInPane !== undefined) {
    const text = step.expectStateInPane;
    // jsdom has no layout, so the size is proven by structure: the state sits
    // in the slot the viewer fills, whose size the stylesheet fixes.
    await waitFor(() => {
      const box = mounted.container.querySelector('.ks-session-preview[data-preview-pane-state]');
      if (box === null) throw new Error(`${at}: there is no preview slot holding a state`);
      // The reading skeleton names itself by its accessible label.
      const labels = [...box.querySelectorAll('[aria-label]')].map((element) => element.getAttribute('aria-label') ?? '');
      expect([box.textContent ?? '', ...labels].join('\n'), `${at}: the slot holds "${text}"`).toContain(text);
    });
  }
  if (step.expectPreviewColumn !== undefined) {
    const { region: name, holds, absentText } = step.expectPreviewColumn;
    if (!SLOT_CONTENTS.includes(holds)) throw new Error(`${at}: expectPreviewColumn.holds is ${String(holds)}; it is one of ${SLOT_CONTENTS.join(', ')}`);
    const region = await waitFor(() => screen.getByRole('region', { name }));
    // The region is the column's only content: nothing above or beside it.
    const column = region.parentElement;
    expect(column === null ? [] : [...column.children], `${at}: the preview column holds the region alone`).toEqual([region]);
    if (holds === PreviewSlotContent.Viewer) {
      await waitFor(() => expect(region.querySelector('article[data-turn]'), `${at}: the viewer drew a turn`).not.toBeNull());
      // The viewer's own root is the region's only child: no caption, heading or frame between them.
      expect(region.children.length, `${at}: the region holds one element`).toBe(1);
      const root = region.firstElementChild!;
      expect([...root.classList].some((name) => name.startsWith(VIEWER_ROOT_PREFIX)), `${at}: the region's child is the viewer's root (class ${root.className})`).toBe(true);
    } else {
      expect(region.querySelector('article[data-turn]'), `${at}: no turn is drawn yet`).toBeNull();
    }
    // Text the column must not carry anywhere: a heading or caption that names it.
    const elements = [column ?? region, ...(column ?? region).querySelectorAll('*')];
    for (const text of absentText) {
      const found = elements.filter((element) => element.children.length === 0 && (element.textContent ?? '').trim() === text);
      expect(found.map((element) => element.outerHTML), `${at}: the column shows no "${text}"`).toEqual([]);
    }
  }
  for (const check of step.expectPrivacy ?? []) {
    if (!PRIVACY.includes(check)) throw new Error(`${at}: expectPrivacy names ${check}; it is one of ${PRIVACY.join(', ')}`);
    const words = markers(spec);
    if (check === PreviewPrivacy.Storage) {
      for (const storage of [window.localStorage, window.sessionStorage]) {
        const held = Array.from({ length: storage.length }, (_, index) => {
          const key = storage.key(index) ?? '';
          return `${key}=${storage.getItem(key) ?? ''}`;
        }).join('\n');
        for (const word of [...words, spec.captureId]) expect(held, `${at}: browser storage holds ${word}`).not.toContain(word);
      }
      expect('indexedDB' in window ? window.indexedDB : undefined, `${at}: the page has no IndexedDB to write to in this test, so none was opened`).toBeUndefined();
    }
    if (check === PreviewPrivacy.Url) {
      expect(window.location.href, `${at}: the URL`).toBe(state.href);
      expect(state.history.push, `${at}: history.pushState`).not.toHaveBeenCalled();
      expect(state.history.replace, `${at}: history.replaceState`).not.toHaveBeenCalled();
      const pane = mounted.container.querySelector('[data-preview-session]');
      const links = pane === null ? [] : [...pane.querySelectorAll('a[href]')].map((link) => link.getAttribute('href'));
      expect(links, `${at}: links in the preview, any of which would change the URL`).toEqual([]);
    }
    if (check === PreviewPrivacy.Clipboard) {
      expect(state.clipboard, `${at}: navigator.clipboard.writeText`).not.toHaveBeenCalled();
      expect(state.execCommand, `${at}: document.execCommand`).not.toHaveBeenCalled();
    }
    if (check === PreviewPrivacy.Requests) {
      const outside = mounted.server.calls.filter((call) => !call.path.startsWith(API));
      expect(outside.map((call) => `${call.method} ${call.path}`), `${at}: requests outside the kickstart routes`).toEqual([]);
    }
    if (check === PreviewPrivacy.Images) {
      const images = [...mounted.container.querySelectorAll('img, source, image')].map((node) => node.getAttribute('src') ?? node.getAttribute('srcset') ?? node.getAttribute('href') ?? '');
      expect(images.filter((src) => /^(https?:)?\/\//.test(src)), `${at}: image elements that point off this page`).toEqual([]);
    }
  }
}

function serverFor(entry: FixtureCase, defaults: Record<string, RouteAnswer | RouteAnswer[]>): KickstartServer {
  const server = new KickstartServer();
  const routes = { ...defaults, ...((entry.server ?? {}) as Record<string, RouteAnswer | RouteAnswer[]>) };
  for (const [route, answers] of Object.entries(routes)) server.script(route, answers);
  return server;
}

afterEach(() => {
  teardown();
  vi.restoreAllMocks();
});

describe('the raw session preview on the selection page', () => {
  const raw = readFixture(PATH) as { defaultServer: Record<string, RouteAnswer | RouteAnswer[]> };
  for (const entry of loadCases(PATH)) {
    it(entry.name, async () => {
      const unknownCase = Object.keys(entry).filter((key) => !CASE_KEYS.includes(key));
      expect(unknownCase, `case ${entry.name} has keys nothing reads`).toEqual([]);
      const spec = fixture.streams[entry.stream as string];
      if (spec === undefined) throw new Error(`case ${entry.name} names stream ${String(entry.stream)}, which the fixture does not define`);
      if (entry.fakeTimers === true) vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });

      const clipboard = vi.fn(async () => {});
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: clipboard, write: clipboard } });
      const execCommand = vi.fn(() => false);
      Object.defineProperty(document, 'execCommand', { configurable: true, value: execCommand });

      const mounted = await mountKickstart(<FirstRunProvider><SelectionPage /></FirstRunProvider>, serverFor(entry, raw.defaultServer));
      // The forest is drawn: the rows the steps act on are on the page.
      await waitFor(() => expect(mounted.container.querySelectorAll('[data-node-id]').length).toBeGreaterThan(0));
      const state: Conversation = {
        seq: 0,
        loaded: 0,
        // The harness rendered into the body; cleanup unmounts it through React.
        unmount: () => cleanup(),
        href: window.location.href,
        history: { push: vi.spyOn(window.history, 'pushState'), replace: vi.spyOn(window.history, 'replaceState') },
        clipboard,
        execCommand,
      };

      for (const [position, raw] of (entry.steps as Record<string, unknown>[]).entries()) {
        const at = `${entry.name} step ${position + 1}`;
        const own = Object.fromEntries(Object.entries(raw).filter(([key]) => key in PREVIEW_KEYS)) as PreviewStep;
        const shared = Object.fromEntries(Object.entries(raw).filter(([key]) => !(key in PREVIEW_KEYS))) as Step;
        if (Object.keys(shared).length > 0) await runSteps([shared], mounted, at);
        if (Object.keys(own).length > 0) await runPreviewStep(own, spec, state, mounted, at);
      }
    });
  }
});
