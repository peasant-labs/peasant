import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
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

const NEXT_COMMIT = {
  hash: '1b2c3d4e5f60',
  message: 'fix(api): keep the refusal reason',
  authorName: 'Ada Lovelace',
  authorEmail: 'ada@example.test',
  commitTime: 1752566520000,
  authorTime: 1752566520000,
};

const NEXT_PULL_REQUEST = {
  owner: 'storytold',
  repo: 'photocraft',
  number: 7,
  url: 'https://github.com/storytold/photocraft/pull/7',
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

/** A response the test decides when to deliver, so a read can stay in flight. */
function deferred(): { promise: Promise<Response>; resolve: (response: Response) => void } {
  let deliver!: (response: Response) => void;
  const promise = new Promise<Response>((resolve) => {
    deliver = resolve;
  });
  return { promise, resolve: deliver };
}

/** Let every queued microtask (and the response body parse) finish. */
async function settle(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
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

  it('discards the previous session rows before the next reads land, and ignores its late answer', async () => {
    const latePullRequests = deferred();
    const nextCommits = deferred();
    const nextPullRequests = deferred();
    const fetcher = vi.fn((url: string) => {
      if (url.includes('sess-extras-3')) {
        return url.includes('/commits')
          ? Promise.resolve(json({ commits: [COMMIT] }))
          : latePullRequests.promise;
      }
      return url.includes('/commits') ? nextCommits.promise : nextPullRequests.promise;
    });
    vi.stubGlobal('fetch', fetcher);

    const { result, rerender } = renderHook(({ sessionId }) => useSessionExtras(sessionId), {
      initialProps: { sessionId: 'sess-extras-3' },
    });

    // The first session's commits are on screen; its pull request read is still
    // in flight, so the switch below starts from a non-empty state.
    await waitFor(() => expect(result.current.commits).toEqual([COMMIT]));
    expect(result.current.pullRequests).toEqual([]);

    rerender({ sessionId: 'sess-extras-4' });

    // The switch clears the previous session's rows as its effect runs, before
    // the next session's reads (still pending) can deliver anything.
    expect(result.current.commits).toEqual([]);
    expect(result.current.pullRequests).toEqual([]);
    expect(fetcher).toHaveBeenCalledTimes(4);

    // The abandoned read finishes after the switch; its rows must not appear
    // under the session the reader is now looking at.
    await act(async () => {
      latePullRequests.resolve(json({ pullRequests: [PULL_REQUEST] }));
      await settle();
    });
    expect(result.current.commits).toEqual([]);
    expect(result.current.pullRequests).toEqual([]);

    await act(async () => {
      nextCommits.resolve(json({ commits: [NEXT_COMMIT] }));
      nextPullRequests.resolve(json({ pullRequests: [NEXT_PULL_REQUEST] }));
    });
    await waitFor(() => {
      expect(result.current.commits).toEqual([NEXT_COMMIT]);
      expect(result.current.pullRequests).toEqual([NEXT_PULL_REQUEST]);
    });
  });

  it('abandons the previous session reads before reading the next session', async () => {
    const pending = new Promise<Response>(() => {});
    const fetcher = stubEndpoints(() => pending, () => pending);

    const { rerender, unmount } = renderHook(({ sessionId }) => useSessionExtras(sessionId), {
      initialProps: { sessionId: 'sess-extras-5' },
    });
    const firstSignals = fetcher.mock.calls.map(([, init]) => init?.signal as AbortSignal);
    expect(firstSignals).toHaveLength(2);
    expect(firstSignals.every((signal) => !signal.aborted)).toBe(true);

    rerender({ sessionId: 'sess-extras-6' });
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
    expect(firstSignals.every((signal) => signal.aborted)).toBe(true);

    const secondSignals = fetcher.mock.calls.slice(2).map(([, init]) => init?.signal as AbortSignal);
    expect(secondSignals.every((signal) => !signal.aborted)).toBe(true);

    unmount();
    expect(secondSignals.every((signal) => signal.aborted)).toBe(true);
  });
});
