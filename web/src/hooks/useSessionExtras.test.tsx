import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import { useSessionExtras } from './useSessionExtras';

// These extras are additive: the transcript is complete without them, so the two
// reads must stay independent and silent, and neither may outlive its session.

const COMMIT = {
  hash: '9f3c1ad0aa11',
  message: 'feat(canvas): port TurnRow',
  authorName: 'Ada Lovelace',
  authorEmail: 'ada@example.test',
  commitTime: 1752566460000,
  authorTime: 1752566400000,
};

const PULL_REQUEST = {
  owner: 'storytold',
  repo: 'photocraft',
  number: 42,
  url: 'https://github.com/storytold/photocraft/pull/42',
  source: 'gh_pr_create',
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

function stubEndpoints(commits: () => Promise<Response>, pullRequests: () => Promise<Response>) {
  const fetcher = vi.fn((url: string, _init?: RequestInit) => (
    url.includes('/commits') ? commits() : pullRequests()
  ));
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}

afterEach(() => vi.unstubAllGlobals());

describe('useSessionExtras', () => {
  it('reads the session commits and detected pull requests independently', async () => {
    const fetcher = stubEndpoints(
      () => Promise.resolve(json({ commits: [COMMIT] })),
      () => Promise.resolve(json({ pullRequests: [PULL_REQUEST] })),
    );

    const { result } = renderHook(() => useSessionExtras('sess-extras-1'));

    await waitFor(() => {
      expect(result.current.commits).toEqual([COMMIT]);
      expect(result.current.pullRequests).toEqual([PULL_REQUEST]);
    });
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([
      expect.stringContaining('/api/v1/sessions/sess-extras-1/commits'),
      expect.stringContaining('/api/v1/sessions/sess-extras-1/pull-requests'),
    ]);
  });

  it('leaves one piece empty when its read fails, without failing the other', async () => {
    stubEndpoints(
      () => Promise.resolve(json({ error: 'store not available' }, 503)),
      () => Promise.resolve(json({ pullRequests: [PULL_REQUEST] })),
    );

    const { result } = renderHook(() => useSessionExtras('sess-extras-2'));

    await waitFor(() => expect(result.current.pullRequests).toEqual([PULL_REQUEST]));
    expect(result.current.commits).toEqual([]);
  });

  it('abandons the previous session reads before reading the next session', async () => {
    const pending = new Promise<Response>(() => {});
    const fetcher = stubEndpoints(() => pending, () => pending);

    const { rerender, unmount } = renderHook(({ sessionId }) => useSessionExtras(sessionId), {
      initialProps: { sessionId: 'sess-extras-3' },
    });
    const firstSignals = fetcher.mock.calls.map(([, init]) => init?.signal as AbortSignal);
    expect(firstSignals).toHaveLength(2);
    expect(firstSignals.every((signal) => !signal.aborted)).toBe(true);

    rerender({ sessionId: 'sess-extras-4' });
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
    expect(firstSignals.every((signal) => signal.aborted)).toBe(true);

    const secondSignals = fetcher.mock.calls.slice(2).map(([, init]) => init?.signal as AbortSignal);
    expect(secondSignals.every((signal) => !signal.aborted)).toBe(true);

    unmount();
    expect(secondSignals.every((signal) => signal.aborted)).toBe(true);
  });
});
