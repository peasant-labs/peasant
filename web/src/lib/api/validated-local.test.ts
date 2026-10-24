import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';
import { zSyncAuthResponse } from '@peasant-labs/schema';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { decodeLocalAnswer, LocalRequestError, localRefusal, readLocalAnswer } from './validated-local';

const fixtures = parse(readFileSync(resolve('src/lib/api/testdata/validated-local.yaml'), 'utf8')) as {
  requiredNames: string[];
  cases: { name: string; status: number; body: string; valid?: boolean; refusal?: boolean; unreadable?: boolean }[];
};
afterEach(() => vi.unstubAllGlobals());

describe('validated local responses', () => {
  it('retains every named boundary case', () => {
    expect(new Set(fixtures.cases.map(c => c.name))).toEqual(new Set(fixtures.requiredNames));
  });
  for (const fixture of fixtures.cases) it(fixture.name, async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(fixture.body, { status: fixture.status }));
    vi.stubGlobal('fetch', fetcher);
    const pending = readLocalAnswer('/api/v1/sync/auth');
    if (fixture.unreadable) {
      await expect(pending).rejects.toMatchObject({ status: fixture.status, message: expect.stringContaining('unreadable JSON') });
      return;
    }
    const answer = await pending;
    expect(answer.status).toBe(fixture.status);
    expect(answer.body).toEqual(JSON.parse(fixture.body));
    if (fixture.refusal) {
      expect(localRefusal('/api/v1/sync/auth', answer)).toMatchObject({ status: 401, message: expect.stringContaining('sign in') });
    } else if (fixture.valid) {
      expect(decodeLocalAnswer('/api/v1/sync/auth', answer, zSyncAuthResponse)).toEqual({ authenticated: false });
    } else {
      expect(() => decodeLocalAnswer('/api/v1/sync/auth', answer, zSyncAuthResponse)).toThrow(LocalRequestError);
    }
    expect(fetcher).toHaveBeenCalledWith(expect.stringContaining('/api/v1/sync/auth'), { cache: 'no-store' });
  });
  it('distinguishes a local outage from a remote HTTP failure', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('connection refused')));
    await expect(readLocalAnswer('/api/v1/health')).rejects.toMatchObject({ status: 0 });
  });
  it('does not relabel user cancellation as a local outage', async () => {
    const controller = new AbortController();
    controller.abort();
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(controller.signal.reason));
    await expect(readLocalAnswer('/api/v1/health', { signal: controller.signal })).rejects.toBe(controller.signal.reason);
  });
});
