import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { act, cleanup, fireEvent, render, screen, waitFor, within, type BoundFunctions, type queries } from '@testing-library/react';
import { type ReactElement } from 'react';
import { expect, vi } from 'vitest';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { MockWebSocket } from '@/test/mockWebSocket';
import { checkPushedEvent, checkScriptedAnswer, expandRefs, loadModels, loadTrees, type FixtureCase, type ScriptedAnswer } from '@/test/fixtures/kickstart';

/**
 * The HTTP and WebSocket boundary of the kickstart pages, played from fixtures.
 *
 * `fetch` and `WebSocket` are the only things replaced. The server's answers
 * are wire JSON from the fixtures (named sessions and forests, scripted
 * answers, pushed events), each checked against the generated contract unless
 * the fixture marks it `malformed: true`; this file holds no rule of the server
 * and cannot make a session out of anything but a fixture. A route that no
 * script answers is a 404 and is recorded, so a test can assert that a page
 * made no call it should not have.
 *
 * A fixture is read strictly: a step key or a case key that nothing reads is
 * an error, never an assertion that silently does not run.
 */

type Json = unknown;

export type RouteAnswer = ScriptedAnswer;

export interface Call {
  method: string;
  path: string;
  body: Json;
}

export interface Step {
  /** Push a message on the kickstart topic. */
  push?: { kind: string; model?: string; sessionId?: string; revision?: number; reason?: string; scan?: Json; ingest?: Json; committed?: Json; withView?: boolean };
  /** Push a raw frame as the server would send it. */
  frame?: Json;
  /**
   * The event or frame this step pushes is a deliberate breach of the contract.
   * Without it, a pushed kickstart event that the contract forbids is an error.
   */
  malformed?: true;
  click?: string;
  /** Move keyboard focus to the checkbox with this name (a forest row, to read its preview). */
  focus?: string;
  choose?: string;
  check?: string;
  /** Answer the next calls of a route differently from here on. */
  script?: { route: string; answers: RouteAnswer | RouteAnswer[] };
  /** Send the answers a route has been holding (`held: true`), and let its later calls through at once. */
  release?: string;
  socket?: 'close' | 'open';
  advance?: number;
  /** Let pending promises settle. */
  settle?: true;
  expectText?: string[];
  expectNoText?: string[];
  /** Assert a control by role and accessible name. */
  expectControl?: { role: string; name: string; present?: boolean; disabled?: boolean; checked?: boolean; valueNow?: number };
  /** No control of this role has an accessible name matching the pattern. */
  expectNoControlMatching?: { role: string; pattern: string };
  /** No request was made whose `METHOD path` matches the pattern. */
  expectNoCalls?: string;
  expectCalls?: { route: string; count: number };
  /** Assert the nth (default last) body sent to a route contains this subset. */
  expectBody?: { route: string; nth?: number; subset: Json };
  /**
   * Every body sent to a route names only these keys, and at least one was
   * sent. The check cannot pass because nothing was sent.
   */
  expectBodyKeys?: { route: string; only: string[] };
  /** The forest rows on the page, by node id, in the order they are drawn. */
  expectRows?: string[];
  /**
   * The structured-value blocks on the page, in order: the lines of each as
   * drawn, and whether it sits inside a collapsed `<details>`. These are all of
   * them, so a value drawn as a block that is not listed fails.
   */
  expectBlocks?: { lines: string[]; collapsed: boolean }[];
  expectLink?: { name: string; href: string };
  expectNoReload?: true;
  /** Texts that must all appear, in this order in the document. */
  expectOrder?: string[];
  /** A <details> by its summary text: whether it is open. */
  expectDetails?: { summary: string; open: boolean };
  /**
   * The step rail's labels: these texts, one per step in step order, each
   * placed by where its step sits (first, last, or between) and kept inside the
   * rail's frame. jsdom has no layout engine, so the placement is read from the
   * real stylesheet the labels are given: their computed alignment, shift and
   * wrapping.
   */
  expectRailLabels?: { labels: string[]; first: RailPlacement; middle: RailPlacement; last: RailPlacement };
}

/** How a rail label sits under its marker, as the stylesheet resolves it. */
export interface RailPlacement {
  textAlign: string;
  /** Whether the label is moved so its edge meets its marker's edge. */
  shifted: boolean;
}

export const API = '/api/v1/kickstart';

/**
 * Every key a step may have: the closed set the runner reads. Declared against
 * the interface in both directions, so a step kind added to one and not the
 * other stops the build, and a step that names anything else fails when it is
 * played instead of asserting nothing.
 */
const STEP_KEYS = Object.freeze({
  push: true,
  frame: true,
  malformed: true,
  click: true,
  focus: true,
  choose: true,
  check: true,
  script: true,
  release: true,
  socket: true,
  advance: true,
  settle: true,
  expectText: true,
  expectNoText: true,
  expectControl: true,
  expectNoControlMatching: true,
  expectNoCalls: true,
  expectCalls: true,
  expectBody: true,
  expectBodyKeys: true,
  expectRows: true,
  expectBlocks: true,
  expectLink: true,
  expectNoReload: true,
  expectOrder: true,
  expectDetails: true,
  expectRailLabels: true,
} satisfies Record<keyof Step, true>);

const STEP_KEY_NAMES: readonly string[] = Object.keys(STEP_KEYS);
const PUSH_KEYS: readonly string[] = ['kind', 'model', 'sessionId', 'revision', 'reason', 'scan', 'ingest', 'committed', 'withView'];
const SOCKET_ACTIONS: readonly string[] = ['close', 'open'];

/** Refuse a step that names a key the runner does not read, names nothing, or names an action it does not know. */
function checkStep(step: unknown, where: string): void {
  if (step === null || typeof step !== 'object' || Array.isArray(step)) {
    throw new Error(`${where} is not a mapping of step keys; steps are ${STEP_KEY_NAMES.join(', ')}`);
  }
  const keys = Object.keys(step);
  if (keys.length === 0) throw new Error(`${where} is empty, so it does nothing and asserts nothing; give it one of ${STEP_KEY_NAMES.join(', ')}`);
  const unknown = keys.filter((key) => !STEP_KEY_NAMES.includes(key));
  if (unknown.length > 0) {
    throw new Error(`${where} has ${unknown.map((key) => JSON.stringify(key)).join(', ')}, which the runner does not read; a misspelt key would let the step pass without running. the keys are ${STEP_KEY_NAMES.join(', ')}`);
  }
  const { socket, malformed } = step as Step;
  if (socket !== undefined && !SOCKET_ACTIONS.includes(socket)) {
    throw new Error(`${where} has socket ${JSON.stringify(socket)}; a socket step is ${SOCKET_ACTIONS.join(' or ')}`);
  }
  if (malformed !== undefined && (malformed !== true || (!('push' in step) && !('frame' in step)))) {
    throw new Error(`${where} is marked malformed, which means that the event or frame it pushes breaks the contract on purpose; it must be \`malformed: true\` on a step that has a push or a frame`);
  }
}

/** Refuse a case that has a key neither the harness nor the test that plays it reads. */
function checkCaseKeys(entry: FixtureCase, played: readonly string[], own: readonly string[]): void {
  const unknown = Object.keys(entry).filter((key) => !played.includes(key) && !own.includes(key));
  if (unknown.length > 0) {
    throw new Error(
      `case ${entry.name} has ${unknown.map((key) => JSON.stringify(key)).join(', ')}, which neither the harness nor the test that plays it reads; ` +
      `the harness reads ${played.join(', ')}${own.length > 0 ? ` and this test reads ${own.join(', ')}` : ''}. a key nothing reads asserts nothing.`,
    );
  }
}

const CASE_KEYS: readonly string[] = ['name', 'server', 'steps', 'fakeTimers'];
const TAB_CASE_KEYS: readonly string[] = [...CASE_KEYS, 'tabs'];

function isSubset(actual: Json, subset: Json): boolean {
  if (subset !== null && typeof subset === 'object' && !Array.isArray(subset)) {
    if (actual === null || typeof actual !== 'object') return false;
    return Object.entries(subset as Record<string, Json>).every(([key, value]) => isSubset((actual as Record<string, Json>)[key], value));
  }
  if (Array.isArray(subset)) {
    return Array.isArray(actual) && subset.length === actual.length && subset.every((entry, index) => isSubset(actual[index], entry));
  }
  return Object.is(actual, subset);
}

export class KickstartServer {
  readonly calls: Call[] = [];
  readonly models = loadModels();
  readonly trees = loadTrees();
  private readonly scripts = new Map<string, RouteAnswer[]>();
  private readonly used = new Map<string, number>();
  private readonly holding = new Map<string, (() => void)[]>();
  private readonly released = new Set<string>();

  script(route: string, answers: RouteAnswer | RouteAnswer[]): void {
    const expanded = (Array.isArray(answers) ? answers : [answers]).map((answer) => expandRefs(answer, this.models, this.trees) as RouteAnswer);
    for (const answer of expanded) checkScriptedAnswer(route, answer);
    this.scripts.set(route, expanded);
    this.used.set(route, 0);
    this.released.delete(route);
  }

  /** Send what a route holds and stop holding it. Releasing a route nothing holds is an error. */
  release(route: string): void {
    if (!(this.scripts.get(route) ?? []).some((answer) => answer.held === true)) {
      throw new Error(`a step releases ${route}, which no scripted answer holds; script it with held: true`);
    }
    this.released.add(route);
    for (const send of this.holding.get(route) ?? []) send();
    this.holding.delete(route);
  }

  model(name: string): Record<string, Json> {
    const found = this.models[name];
    if (found === undefined) throw new Error(`fixture names model ${JSON.stringify(name)}, which is not defined`);
    return structuredClone(found) as Record<string, Json>;
  }

  count(route: string): number {
    return this.calls.filter((call) => `${call.method} ${call.path}` === route).length;
  }

  bodies(route: string): Json[] {
    return this.calls.filter((call) => `${call.method} ${call.path}` === route).map((call) => call.body);
  }

  install(): void {
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, 'http://localhost');
      const method = (init?.method ?? 'GET').toUpperCase();
      const path = url.pathname;
      const body = typeof init?.body === 'string' && init.body !== '' ? JSON.parse(init.body) : undefined;
      this.calls.push({ method, path, body });
      const route = `${method} ${path}`;
      const answers = this.scripts.get(route);
      if (answers === undefined) {
        return new Response(JSON.stringify({ error: `no fixture answers ${route}` }), { status: 404 });
      }
      const index = Math.min(this.used.get(route) ?? 0, answers.length - 1);
      this.used.set(route, (this.used.get(route) ?? 0) + 1);
      const answer = answers[index];
      if (answer.held === true && !this.released.has(route)) {
        await new Promise<void>((send) => {
          this.holding.set(route, [...(this.holding.get(route) ?? []), send]);
        });
      }
      return new Response(answer.text ?? JSON.stringify(answer.body ?? {}), { status: answer.status ?? 200 });
    });
  }

  /** The frame data of a topic message built from a fixture model. */
  event(spec: NonNullable<Step['push']>): Json {
    const unknown = Object.keys(spec).filter((key) => !PUSH_KEYS.includes(key));
    if (unknown.length > 0) {
      throw new Error(`a pushed event has ${unknown.map((key) => JSON.stringify(key)).join(', ')}, which a push does not read; it reads ${PUSH_KEYS.join(', ')}`);
    }
    const model = spec.model === undefined ? undefined : this.model(spec.model);
    const sessionId = spec.sessionId ?? (model?.id as string | undefined);
    if (sessionId === undefined) throw new Error('a pushed event names a model or a sessionId');
    const revision = spec.revision ?? (model?.revision as number | undefined) ?? 0;
    const data: Record<string, Json> = { kind: spec.kind, sessionId, revision };
    if (model !== undefined && spec.withView !== false) data.view = model;
    if (spec.reason !== undefined) data.reason = spec.reason;
    if (spec.scan !== undefined) data.scan = spec.scan;
    if (spec.ingest !== undefined) data.ingest = spec.ingest;
    if (spec.committed !== undefined) data.committed = spec.committed;
    return data;
  }
}

type Queries = BoundFunctions<typeof queries>;

export interface Mounted {
  server: KickstartServer;
  socket: MockWebSocket;
  reloads: { count: number };
  /** Where this page is drawn. A single page owns the document body. */
  container: HTMLElement;
  /** Queries limited to this page, so two pages in one document do not collide. */
  root: Queries;
}

function stubBrowser(server: KickstartServer): { count: number } {
  MockWebSocket.reset();
  vi.stubGlobal('WebSocket', MockWebSocket);
  server.install();
  const reloads = { count: 0 };
  const original = window.location;
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: { ...original, href: original.href, origin: original.origin, pathname: '/kickstart', search: '', reload: () => { reloads.count += 1; } },
  });
  return reloads;
}

/** Mount a page under the real WebSocket provider with the boundary played from fixtures. */
export async function mountKickstart(element: ReactElement, server: KickstartServer): Promise<Mounted> {
  const reloads = stubBrowser(server);
  render(<WebSocketProvider>{element}</WebSocketProvider>);
  const socket = MockWebSocket.latest();
  await act(async () => {
    socket.simulateOpen();
  });
  return { server, socket, reloads, container: document.body, root: screen };
}

/**
 * Mount several pages of one browser, each with its own socket on the one
 * server: two tabs, or the wizard beside the selection page. They mount in
 * order, each once it has drawn its first stop, so the server's scripted
 * answers go to the tabs in the order they are named.
 */
export async function mountKickstartTabs(pages: readonly { name: string; element: ReactElement }[], server: KickstartServer): Promise<Record<string, Mounted>> {
  const reloads = stubBrowser(server);
  const tabs: Record<string, Mounted> = {};
  for (const page of pages) {
    const container = document.body.appendChild(document.createElement('div'));
    render(<WebSocketProvider>{page.element}</WebSocketProvider>, { container, baseElement: document.body });
    const socket = MockWebSocket.latest();
    await act(async () => {
      socket.simulateOpen();
    });
    const root = within(container);
    await waitFor(() => expect(root.queryAllByRole('heading', { level: 2 }).length, `tab ${page.name} draws its first stop`).toBeGreaterThan(0));
    tabs[page.name] = { server, socket, reloads, container, root };
  }
  return tabs;
}

function scriptServer(entry: FixtureCase): KickstartServer {
  const server = new KickstartServer();
  for (const [route, answers] of Object.entries((entry.server ?? {}) as Record<string, RouteAnswer | RouteAnswer[]>)) {
    server.script(route, answers);
  }
  return server;
}

/** The steps of a case, which must exist: a case with none mounts a page and asserts nothing. */
function stepsOf(entry: FixtureCase): Step[] {
  if (!Array.isArray(entry.steps) || entry.steps.length === 0) {
    throw new Error(`case ${entry.name} has no steps, so it would mount a page and assert nothing`);
  }
  return entry.steps as Step[];
}

/**
 * Mount a page for one fixture case: its scripted server answers, then its
 * steps. `own` names the case keys the calling test reads itself; any other
 * key the harness does not read is an error.
 */
export async function playCase(entry: FixtureCase, element: ReactElement, own: readonly string[] = []): Promise<Mounted> {
  checkCaseKeys(entry, CASE_KEYS, own);
  const steps = stepsOf(entry);
  // Only timeouts are faked, and the clock still moves with real time, so
  // Testing Library's own polling keeps working while a case controls timers.
  if (entry.fakeTimers === true) vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });
  const server = scriptServer(entry);
  const mounted = await mountKickstart(element, server);
  await runSteps(steps, mounted);
  return mounted;
}

/** A step of a case with several pages: it names the page it acts on, or `all`. */
export type TabStep = Step & { tab: string };

/**
 * Mount the pages a case names, in the order its `tabs` lists them, then run its
 * steps, each on the page it names (`all` runs it on every page: a topic push
 * reaches every subscriber). The server and its call log are shared, as one
 * browser's are.
 */
export async function playTabs(entry: FixtureCase, pages: Readonly<Record<string, ReactElement>>, own: readonly string[] = []): Promise<Record<string, Mounted>> {
  checkCaseKeys(entry, TAB_CASE_KEYS, own);
  const steps = stepsOf(entry);
  const order = entry.tabs;
  if (!Array.isArray(order) || order.some((name) => typeof name !== 'string' || pages[name] === undefined)) {
    throw new Error(`case ${entry.name}: tabs must name pages from ${Object.keys(pages).join(', ')}, in mount order`);
  }
  if (entry.fakeTimers === true) vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });
  const server = scriptServer(entry);
  const mounted = await mountKickstartTabs((order as string[]).map((name) => ({ name, element: pages[name] })), server);
  for (const [position, raw] of (steps as TabStep[]).entries()) {
    const { tab, ...step } = raw;
    const targets = tab === 'all' ? Object.values(mounted) : mounted[tab] === undefined ? [] : [mounted[tab]];
    if (targets.length === 0) throw new Error(`case ${entry.name} step ${position + 1}: tab ${JSON.stringify(tab)} is not one of ${Object.keys(mounted).join(', ')}, all`);
    for (const target of targets) await runSteps([step], target, `tab ${tab}, step ${position + 1}`);
  }
  return mounted;
}

const STYLESHEET_MARK = 'data-kickstart-stylesheet';

/**
 * Give the document the wizard's real stylesheet. The test runner does not
 * apply imported CSS, so a check of how something is styled has to load the
 * file the page would ship.
 */
function loadWizardStylesheet(): void {
  if (document.head.querySelector(`style[${STYLESHEET_MARK}]`) !== null) return;
  const style = document.createElement('style');
  style.setAttribute(STYLESHEET_MARK, '');
  style.textContent = readFileSync(resolve(process.cwd(), 'src/components/kickstart/kickstart.css'), 'utf8');
  document.head.appendChild(style);
}

export function teardown(): void {
  document.head.querySelectorAll(`style[${STYLESHEET_MARK}]`).forEach((style) => style.remove());
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
}

function control(q: Queries, role: string, name: string): HTMLElement {
  return q.getByRole(role as never, { name: new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`, 'i') });
}

/**
 * Run the steps of a fixture case against a mounted page. `where` names the
 * page and position in a failure message when the steps are a slice of a longer
 * case.
 */
export async function runSteps(steps: readonly Step[], mounted: Mounted, where?: string): Promise<void> {
  const { server, socket, container, root: q } = mounted;
  for (const [position, step] of steps.entries()) {
    const at = where ?? `step ${position + 1}`;
    checkStep(step, at);
    if (step.push) {
      const data = server.event(step.push);
      checkPushedEvent(data, step.malformed, at);
      await act(async () => {
        socket.simulateMessage({ type: 'kickstart', data });
      });
    }
    if (step.frame !== undefined) {
      const frame = step.frame as { type?: unknown; data?: unknown } | null;
      if (frame !== null && typeof frame === 'object' && frame.type === 'kickstart') checkPushedEvent(frame.data, step.malformed, at);
      await act(async () => {
        socket.simulateMessage(step.frame);
      });
    }
    if (step.script) server.script(step.script.route, step.script.answers);
    if (step.release !== undefined) {
      server.release(step.release);
      await act(async () => {
        await Promise.resolve();
      });
    }
    if (step.click) {
      await act(async () => {
        fireEvent.click(control(q, 'button', step.click!));
      });
    }
    if (step.focus) {
      await act(async () => {
        control(q, 'checkbox', step.focus!).focus();
      });
    }
    if (step.choose) {
      await act(async () => {
        fireEvent.click(control(q, 'radio', step.choose!));
      });
    }
    if (step.check) {
      await act(async () => {
        fireEvent.click(control(q, 'checkbox', step.check!));
      });
    }
    if (step.socket === 'close') {
      await act(async () => {
        socket.simulateClose();
      });
    }
    if (step.socket === 'open') {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1100);
        MockWebSocket.latest().simulateOpen();
      });
    }
    if (step.advance !== undefined) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(step.advance!);
      });
    }
    if (step.settle) {
      await act(async () => {
        await Promise.resolve();
      });
    }
    for (const text of step.expectText ?? []) {
      await waitFor(() => expect(q.getAllByText(text, { exact: false }).length, `${at}: expected text ${JSON.stringify(text)}`).toBeGreaterThan(0));
    }
    for (const text of step.expectNoText ?? []) {
      expect(q.queryAllByText(text, { exact: false }), `${at}: expected no text ${JSON.stringify(text)}`).toHaveLength(0);
    }
    if (step.expectControl) {
      const { role, name, present = true, disabled, checked, valueNow } = step.expectControl;
      const found = q.queryAllByRole(role as never, { name: new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`, 'i') });
      if (!present) {
        expect(found, `${at}: expected no ${role} named ${name}`).toHaveLength(0);
      } else {
        expect(found.length, `${at}: expected a ${role} named ${name}`).toBeGreaterThan(0);
        if (disabled !== undefined) {
          if (disabled) expect(found[0]).toBeDisabled();
          else expect(found[0]).toBeEnabled();
        }
        if (checked !== undefined) {
          if (checked) expect(found[0]).toBeChecked();
          else expect(found[0]).not.toBeChecked();
        }
        if (valueNow !== undefined) expect(found[0]).toHaveAttribute('aria-valuenow', String(valueNow));
      }
    }
    if (step.expectNoControlMatching) {
      const pattern = new RegExp(step.expectNoControlMatching.pattern, 'i');
      const found = q.queryAllByRole(step.expectNoControlMatching.role as never, { name: pattern });
      expect(found, `${at}: no ${step.expectNoControlMatching.role} matching ${pattern}`).toHaveLength(0);
    }
    if (step.expectNoCalls) {
      const pattern = new RegExp(step.expectNoCalls);
      const made = server.calls.filter((call) => pattern.test(`${call.method} ${call.path}`)).map((call) => `${call.method} ${call.path}`);
      expect(made, `${at}: no request should match ${pattern}`).toEqual([]);
    }
    if (step.expectCalls) {
      await waitFor(() => expect(server.count(step.expectCalls!.route), `${at}: calls to ${step.expectCalls!.route}`).toBe(step.expectCalls!.count));
    }
    if (step.expectBody) {
      const { route, nth, subset } = step.expectBody;
      await waitFor(() => {
        const bodies = server.bodies(route);
        const body = bodies.at(nth ?? -1);
        expect(body, `${at}: a body sent to ${route}`).toBeDefined();
        expect(isSubset(body, subset), `${at}: body ${JSON.stringify(body)} should contain ${JSON.stringify(subset)}`).toBe(true);
      });
    }
    if (step.expectBodyKeys) {
      const { route, only } = step.expectBodyKeys;
      await waitFor(() => expect(server.bodies(route).length, `${at}: a body was sent to ${route}`).toBeGreaterThan(0));
      for (const body of server.bodies(route)) {
        const keys = body !== null && typeof body === 'object' ? Object.keys(body as Record<string, Json>) : [];
        expect(keys.filter((key) => !only.includes(key)), `${at}: keys sent to ${route} beyond ${only.join(', ')}`).toEqual([]);
      }
    }
    if (step.expectRows) {
      await waitFor(() => {
        const drawn = [...container.querySelectorAll('[data-node-id]')].map((row) => row.getAttribute('data-node-id'));
        expect(drawn, `${at}: forest rows on the page`).toEqual(step.expectRows);
      });
    }
    if (step.expectBlocks) {
      await waitFor(() => {
        const drawn = [...container.querySelectorAll('pre.ks-value')].map((block) => ({
          lines: (block.textContent ?? '').split('\n'),
          collapsed: block.closest('details') !== null,
        }));
        expect(drawn, `${at}: structured-value blocks on the page`).toEqual(step.expectBlocks);
      });
    }
    if (step.expectLink) {
      const link = q.getByRole('link', { name: new RegExp(`^${step.expectLink.name}$`, 'i') });
      expect(link.getAttribute('href'), `${at}: link ${step.expectLink.name}`).toBe(step.expectLink.href);
    }
    if (step.expectOrder) {
      const positions = step.expectOrder.map((text) => {
        const found = q.getAllByText(text, { exact: false })[0];
        expect(found, `${at}: ${text} is on the page`).toBeDefined();
        return found;
      });
      positions.slice(1).forEach((node, i) => {
        const before = positions[i];
        expect(
          Boolean(before.compareDocumentPosition(node) & Node.DOCUMENT_POSITION_FOLLOWING),
          `${at}: ${step.expectOrder![i + 1]} should follow ${step.expectOrder![i]}`,
        ).toBe(true);
      });
    }
    if (step.expectDetails) {
      const summary = q.getAllByText(step.expectDetails.summary, { exact: false })[0];
      const details = summary.closest('details');
      expect(details, `${at}: ${step.expectDetails.summary} is inside <details>`).not.toBeNull();
      expect(details!.open, `${at}: ${step.expectDetails.summary} open state`).toBe(step.expectDetails.open);
    }
    if (step.expectRailLabels) {
      loadWizardStylesheet();
      const { labels, first, middle, last } = step.expectRailLabels;
      const rail = q.getByRole('navigation', { name: /progress$/i });
      const drawn = [...rail.querySelectorAll<HTMLElement>('.ks-rail-label')];
      expect(drawn.map((label) => label.textContent), `${at}: the rail's labels`).toEqual(labels);
      // The cap each label wraps inside is the rail's width over its step count.
      expect(rail.parentElement?.style.getPropertyValue('--ks-rail-steps'), `${at}: steps the rail shares between its labels`).toBe(String(labels.length));
      drawn.forEach((label, position) => {
        const where = `${at}: label ${JSON.stringify(labels[position])}`;
        const want = position === 0 ? first : position === drawn.length - 1 ? last : middle;
        const style = getComputedStyle(label);
        expect(style.textAlign, `${where} is aligned by its place`).toBe(want.textAlign);
        const shifted = style.left !== '' && style.left !== 'auto';
        expect(shifted, `${where} is ${want.shifted ? '' : 'not '}moved onto its marker's edge`).toBe(want.shifted);
        if (want.shifted) expect(style.position, `${where} is moved by position`).toBe('relative');
        // Capped and wrapping: a long label breaks inside its share of the rail.
        expect(style.whiteSpace, `${where} wraps`).toBe('normal');
        expect(style.maxWidth, `${where} is capped`).not.toMatch(/^(|none)$/);
      });
    }
    if (step.expectNoReload) expect(mounted.reloads.count, `${at}: the page reloaded`).toBe(0);
  }
}

export { within, screen, waitFor, act, fireEvent };
