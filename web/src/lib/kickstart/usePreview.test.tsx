import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import { useLayoutEffect } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { loadCases } from '@/test/fixtures/kickstart';
import { fetchKickstartPreview, fetchKickstartStatus } from './client';
import { useKickstartPreview } from './usePreview';
import { KickstartLineKind } from './values';

const FIXTURE = 'src/lib/kickstart/testdata/preview-privacy.yaml';
const A = { session: 'synthetic-session-a', row: 'row-a' };
const B = { session: 'synthetic-session-a', row: 'row-b' };
const OTHER_SESSION = { session: 'synthetic-session-b', row: 'row-a' };
const CLEAR = { session: 'synthetic-session-a', row: null };
type Selection = { session: string | null; row: string | null };

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason: Error) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

/** A dependency that ignores cancellation, including while its response body is being read. */
function heldFetch() {
  const requests: { url: string; init: RequestInit; headers: Deferred<Response>; body: Deferred<string>; text: ReturnType<typeof vi.fn> }[] = [];
  const fetcher = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const headers = deferred<Response>();
    const body = deferred<string>();
    const text = vi.fn(() => body.promise);
    requests.push({ url: String(input), init: init ?? {}, headers, body, text });
    return headers.promise;
  });
  vi.stubGlobal('fetch', fetcher);
  return { requests, fetcher };
}

type Request = ReturnType<typeof heldFetch>['requests'][number];

function signalOf(request: Request): AbortSignal {
  const signal = request.init.signal;
  expect(signal).toBeInstanceOf(AbortSignal);
  if (!signal) throw new Error('the preview request has no abort signal');
  return signal;
}

async function sendHeaders(request: Request) {
  const response = new Response(null, { status: 200 });
  vi.spyOn(response, 'text').mockImplementation(request.text);
  await act(async () => { request.headers.resolve(response); });
  expect(request.text).toHaveBeenCalledOnce();
}

async function sendBody(request: Request, text: string) {
  const body = JSON.stringify({ lines: [{ kind: KickstartLineKind.Text.wire, text }] });
  await act(async () => { request.body.resolve(body); });
}

async function succeed(request: Request, pending: string, text: string) {
  if (pending === 'headers') await sendHeaders(request);
  await sendBody(request, text);
}

async function settleOld(request: Request, pending: string, outcome: string) {
  if (outcome === 'success') {
    await succeed(request, pending, 'stale synthetic preview');
  } else if (pending === 'headers') {
    await act(async () => { request.headers.reject(new Error('stale header failure')); });
  } else {
    await act(async () => { request.body.reject(new Error('stale body failure')); });
  }
}

function expectRequest(request: Request, selection: Selection) {
  expect(request.url).toContain(`/sessions/${selection.session}/tree/${selection.row}/preview`);
  expect(request.init.method).toBe('GET');
  expect(request.init.cache).toBe('no-store');
  expect(signalOf(request).aborted).toBe(false);
}

function show(selection: Selection) {
  return renderHook(({ session, row }: Selection) => useKickstartPreview(session, row), { initialProps: selection });
}

function expectLoading(state: ReturnType<typeof useKickstartPreview>) {
  expect(state).toEqual({ lines: null, loading: true, error: null });
}

function expectText(state: ReturnType<typeof useKickstartPreview>, text: string) {
  expect(state.loading).toBe(false);
  expect(state.error).toBeNull();
  expect(state.lines?.map((line) => line.text)).toEqual([text]);
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('kickstart preview cancellation through the HTTP client', () => {
  for (const entry of loadCases(FIXTURE, 'cancellations')) {
    it(entry.name, async () => {
      const pending = String(entry.pending);
      const transition = String(entry.transition);
      const outcome = String(entry.outcome);
      const settle = String(entry.settle);
      const { requests } = heldFetch();
      let hook = show(A);
      expectRequest(requests[0], A);
      if (pending === 'body') await sendHeaders(requests[0]);

      if (transition === 'remount') {
        hook.unmount();
        hook = show(A);
      } else {
        hook.rerender(transition === 'row' ? B : transition === 'session' ? OTHER_SESSION : CLEAR);
      }
      expect(signalOf(requests[0]).aborted).toBe(true);
      if (transition === 'clear') {
        expect(requests).toHaveLength(1);
        expect(hook.result.current).toEqual({ lines: null, loading: false, error: null });
      } else {
        expect(requests).toHaveLength(2);
        expectRequest(requests[1], transition === 'row' ? B : transition === 'session' ? OTHER_SESSION : A);
        expectLoading(hook.result.current);
      }

      if (settle === 'before') await settleOld(requests[0], pending, outcome);
      if (transition !== 'clear') {
        expectLoading(hook.result.current);
        await succeed(requests[1], 'headers', 'current synthetic preview');
        await waitFor(() => expectText(hook.result.current, 'current synthetic preview'));
      }
      if (settle === 'after') await settleOld(requests[0], pending, outcome);
      if (transition === 'clear') {
        expect(hook.result.current).toEqual({ lines: null, loading: false, error: null });
      } else {
        expectText(hook.result.current, 'current synthetic preview');
      }

      if (transition !== 'remount') {
        // The cancelled answer was never cached: returning to A starts a fresh read.
        hook.rerender(A);
        const fresh = requests[requests.length - 1];
        expect(requests).toHaveLength(transition === 'clear' ? 2 : 3);
        expectRequest(fresh, A);
        expectLoading(hook.result.current);
        await succeed(fresh, 'headers', 'fresh synthetic preview');
        await waitFor(() => expectText(hook.result.current, 'fresh synthetic preview'));
      }
    });
  }

  for (const entry of loadCases(FIXTURE, 'reentries')) {
    it(entry.name, async () => {
      const pending = String(entry.pending);
      const { requests } = heldFetch();
      const hook = show(A);
      if (pending === 'body') await sendHeaders(requests[0]);
      hook.rerender(B);
      hook.rerender(A);
      expect(requests).toHaveLength(3);
      expect(signalOf(requests[0]).aborted).toBe(true);
      expect(signalOf(requests[1]).aborted).toBe(true);
      expectRequest(requests[2], A);
      if (entry.settle === 'before') {
        await settleOld(requests[0], pending, String(entry.outcome));
        expectLoading(hook.result.current);
      }
      await succeed(requests[2], 'headers', 'new synthetic preview');
      await waitFor(() => expectText(hook.result.current, 'new synthetic preview'));
      if (entry.settle === 'after') await settleOld(requests[0], pending, String(entry.outcome));
      expectText(hook.result.current, 'new synthetic preview');
      hook.rerender(B);
      expect(requests).toHaveLength(4);
      hook.rerender(A);
      expect(signalOf(requests[3]).aborted).toBe(true);
      expect(requests).toHaveLength(4);
      expectText(hook.result.current, 'new synthetic preview');
    });
  }
});

describe('kickstart preview selection changes', () => {
  for (const entry of loadCases(FIXTURE, 'visibility')) {
    it(entry.name, async () => {
      const { requests } = heldFetch();
      const observations: { selection: Selection; state: ReturnType<typeof useKickstartPreview> }[] = [];
      const hook = renderHook((selection: Selection) => {
        const state = useKickstartPreview(selection.session, selection.row);
        useLayoutEffect(() => { observations.push({ selection, state }); });
        return state;
      }, { initialProps: A });
      if (entry.previous === 'error') {
        await act(async () => { requests[0].headers.reject(new Error('previous synthetic failure')); });
        await waitFor(() => expect(hook.result.current.error).not.toBeNull());
      } else {
        await succeed(requests[0], 'headers', 'previous synthetic preview');
        await waitFor(() => expectText(hook.result.current, 'previous synthetic preview'));
      }
      const next = entry.transition === 'row' ? B : OTHER_SESSION;
      hook.rerender(next);
      const firstNextCommit = observations.find((observation) => observation.selection === next);
      expect(firstNextCommit).toBeDefined();
      expectLoading(firstNextCommit!.state);
    });
  }
});

describe('kickstart preview cache and unrelated transports', () => {
  for (const entry of loadCases(FIXTURE, 'cache')) {
    it(entry.name, async () => {
      const { requests } = heldFetch();
      if (entry.scenario === 'other-route') {
        const pending = fetchKickstartStatus();
        expect(requests[0].init.cache).toBeUndefined();
        expect(requests[0].init.signal).toBeUndefined();
        await sendHeaders(requests[0]);
        await act(async () => { requests[0].body.resolve(JSON.stringify({ configured: false })); });
        expect((await pending).configured).toBe(false);
        return;
      }
      if (entry.scenario === 'direct-preview') {
        const pending = fetchKickstartPreview(A.session, A.row);
        expect(requests[0].init.cache).toBe('no-store');
        expect(requests[0].init.signal).toBeUndefined();
        await succeed(requests[0], 'headers', 'direct synthetic preview');
        expect((await pending).map((line) => line.text)).toEqual(['direct synthetic preview']);
        return;
      }
      let hook = show(A);
      if (entry.scenario === 'error') {
        await act(async () => { requests[0].headers.reject(new Error('synthetic network failure')); });
        await waitFor(() => expect(hook.result.current.error).not.toBeNull());
      } else {
        await succeed(requests[0], 'headers', 'cached synthetic preview');
        await waitFor(() => expectText(hook.result.current, 'cached synthetic preview'));
      }
      if (entry.scenario === 'remount') {
        hook.unmount();
        hook = show(A);
        expect(requests).toHaveLength(2);
        expectLoading(hook.result.current);
        await succeed(requests[1], 'headers', 'remounted synthetic preview');
        await waitFor(() => expectText(hook.result.current, 'remounted synthetic preview'));
        return;
      }
      hook.rerender(entry.scenario === 'session' ? OTHER_SESSION : B);
      await succeed(requests[1], 'headers', 'other synthetic preview');
      await waitFor(() => expectText(hook.result.current, 'other synthetic preview'));
      hook.rerender(A);
      if (entry.scenario === 'error') {
        expect(requests).toHaveLength(3);
        expectLoading(hook.result.current);
        await succeed(requests[2], 'headers', 'retried synthetic preview');
        await waitFor(() => expectText(hook.result.current, 'retried synthetic preview'));
      } else {
        expect(requests).toHaveLength(2);
        expectText(hook.result.current, 'cached synthetic preview');
      }
    });
  }
});
