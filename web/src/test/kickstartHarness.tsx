import { act, cleanup, fireEvent, render, screen, waitFor, within, type BoundFunctions, type queries } from '@testing-library/react';
import { type ReactElement } from 'react';
import { expect, vi } from 'vitest';
import { WebSocketProvider } from '@/contexts/WebSocketContext';
import { MockWebSocket } from '@/test/mockWebSocket';
import { expandRefs, loadModels, loadTrees, type FixtureCase } from '@/test/fixtures/kickstart';

/**
 * The HTTP and WebSocket boundary of the kickstart pages, played from fixtures.
 *
 * `fetch` and `WebSocket` are the only things replaced. The server's answers
 * are wire JSON from the fixtures (named sessions and forests, checked against
 * the generated contract), scripted per route; this file holds no rule of the
 * server and cannot make a session out of anything but a fixture. A route that
 * no script answers is a 404 and is recorded, so a test can assert that a page
 * made no call it should not have.
 */

type Json = unknown;

export interface RouteAnswer {
  status?: number;
  body?: Json;
}

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
  click?: string;
  /** Move keyboard focus to the checkbox with this name (a forest row, to read its preview). */
  focus?: string;
  choose?: string;
  check?: string;
  /** Answer the next calls of a route differently from here on. */
  script?: { route: string; answers: RouteAnswer | RouteAnswer[] };
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
}

export const API = '/api/v1/kickstart';

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

  script(route: string, answers: RouteAnswer | RouteAnswer[]): void {
    this.scripts.set(route, (Array.isArray(answers) ? answers : [answers]).map((answer) => expandRefs(answer, this.models, this.trees) as RouteAnswer));
    this.used.set(route, 0);
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
      return new Response(JSON.stringify(answer.body ?? {}), { status: answer.status ?? 200 });
    });
  }

  /** The frame data of a topic message built from a fixture model. */
  event(spec: NonNullable<Step['push']>): Json {
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

/** Mount a page for one fixture case: its scripted server answers, then its steps. */
export async function playCase(entry: FixtureCase, element: ReactElement): Promise<Mounted> {
  // Only timeouts are faked, and the clock still moves with real time, so
  // Testing Library's own polling keeps working while a case controls timers.
  if (entry.fakeTimers === true) vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });
  const server = scriptServer(entry);
  const mounted = await mountKickstart(element, server);
  await runSteps((entry.steps ?? []) as Step[], mounted);
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
export async function playTabs(entry: FixtureCase, pages: Readonly<Record<string, ReactElement>>): Promise<Record<string, Mounted>> {
  const order = entry.tabs;
  if (!Array.isArray(order) || order.some((name) => typeof name !== 'string' || pages[name] === undefined)) {
    throw new Error(`case ${entry.name}: tabs must name pages from ${Object.keys(pages).join(', ')}, in mount order`);
  }
  if (entry.fakeTimers === true) vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'], shouldAdvanceTime: true });
  const server = scriptServer(entry);
  const mounted = await mountKickstartTabs((order as string[]).map((name) => ({ name, element: pages[name] })), server);
  for (const [position, raw] of ((entry.steps ?? []) as TabStep[]).entries()) {
    const { tab, ...step } = raw;
    const targets = tab === 'all' ? Object.values(mounted) : mounted[tab] === undefined ? [] : [mounted[tab]];
    if (targets.length === 0) throw new Error(`case ${entry.name} step ${position + 1}: tab ${JSON.stringify(tab)} is not one of ${Object.keys(mounted).join(', ')}, all`);
    for (const target of targets) await runSteps([step], target, `tab ${tab}, step ${position + 1}`);
  }
  return mounted;
}

export function teardown(): void {
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
    if (step.push) {
      await act(async () => {
        socket.simulateMessage({ type: 'kickstart', data: server.event(step.push!) });
      });
    }
    if (step.frame !== undefined) {
      await act(async () => {
        socket.simulateMessage(step.frame);
      });
    }
    if (step.script) server.script(step.script.route, step.script.answers);
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
    if (step.expectNoReload) expect(mounted.reloads.count, `${at}: the page reloaded`).toBe(0);
  }
}

export { within, screen, waitFor, act, fireEvent };
