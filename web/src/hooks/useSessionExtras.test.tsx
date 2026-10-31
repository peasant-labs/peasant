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

/** The decoded row: the contract carries millisecond times as bigint. */
const DECODED_COMMIT = {
  ...COMMIT,
  commitTime: BigInt(COMMIT.commitTime),
  authorTime: BigInt(COMMIT.authorTime),
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

/** The decoded row: the contract carries millisecond times as bigint. */
const DECODED_NEXT_COMMIT = {
  ...NEXT_COMMIT,
  commitTime: BigInt(NEXT_COMMIT.commitTime),
  authorTime: BigInt(NEXT_COMMIT.authorTime),
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
      expect(result.current.commits).toEqual([DECODED_COMMIT]);
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

  it('discards the previous session rows before the next reads land, and ignores both late answers', async () => {
    // Every read below stays in flight until the test delivers it, so a switch
    // can be made while a previous session's answers are still outstanding.
    const pending: Record<string, { commits: ReturnType<typeof deferred>; pullRequests: ReturnType<typeof deferred> }> = {
      'sess-extras-3': { commits: deferred(), pullRequests: deferred() },
      'sess-extras-4': { commits: deferred(), pullRequests: deferred() },
      'sess-extras-5': { commits: deferred(), pullRequests: deferred() },
    };
    const fetcher = vi.fn((url: string) => {
      const session = Object.keys(pending).find((id) => url.includes(id));
      if (!session) throw new Error(`unexpected read: ${url}`);
      const piece = url.includes('/pull-requests') ? pending[session].pullRequests : pending[session].commits;
      return piece.promise;
    });
    vi.stubGlobal('fetch', fetcher);

    const { result, rerender } = renderHook(({ sessionId }) => useSessionExtras(sessionId), {
      initialProps: { sessionId: 'sess-extras-3' },
    });

    // Session 3's reads are the slow ones: they are abandoned below, so nothing
    // of theirs may surface under a later session.
    rerender({ sessionId: 'sess-extras-4' });
    await act(async () => {
      pending['sess-extras-4'].commits.resolve(json({ commits: [NEXT_COMMIT] }));
      pending['sess-extras-4'].pullRequests.resolve(json({ pullRequests: [NEXT_PULL_REQUEST] }));
    });
    await waitFor(() => {
      expect(result.current.commits).toEqual([DECODED_NEXT_COMMIT]);
      expect(result.current.pullRequests).toEqual([NEXT_PULL_REQUEST]);
    });

    // The switch to session 5 clears the rows session 4 delivered, before
    // session 5's reads (still pending) can deliver anything.
    rerender({ sessionId: 'sess-extras-5' });
    expect(result.current.commits).toEqual([]);
    expect(result.current.pullRequests).toEqual([]);

    // The abandoned session 3 finishes after two switches. Its commits and its
    // pull request must not appear under the session the reader is looking at,
    // and must not overwrite it either.
    await act(async () => {
      pending['sess-extras-3'].commits.resolve(json({ commits: [COMMIT] }));
      pending['sess-extras-3'].pullRequests.resolve(json({ pullRequests: [PULL_REQUEST] }));
      await settle();
    });
    expect(result.current.commits).toEqual([]);
    expect(result.current.pullRequests).toEqual([]);

    await act(async () => {
      pending['sess-extras-5'].commits.resolve(json({ commits: [NEXT_COMMIT] }));
      pending['sess-extras-5'].pullRequests.resolve(json({ pullRequests: [NEXT_PULL_REQUEST] }));
    });
    await waitFor(() => {
      expect(result.current.commits).toEqual([DECODED_NEXT_COMMIT]);
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
