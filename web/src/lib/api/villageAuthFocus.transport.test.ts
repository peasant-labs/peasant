import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parse } from 'yaml';
import type { VillageAuthReading } from './villageAuthFocus';

const TransportStall = Object.freeze({ Response: 'response', Body: 'body' } as const);
interface TransportCase {
  name: string;
  stall: (typeof TransportStall)[keyof typeof TransportStall];
}

const FIXTURE = 'src/lib/api/testdata/village_auth_focus_transport.yaml';
const fixture = parse(readFileSync(resolve(process.cwd(), FIXTURE), 'utf8')) as {
  requiredNames: string[];
  cases: TransportCase[];
};

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetModules();
});

afterEach(() => {
  vi.clearAllTimers();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the shared sign-in reader through the HTTP transport', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });

  for (const row of fixture.cases) {
    it(row.name, async () => {
      const t0 = Date.now();
      const calls: { path: string; at: number; signal: AbortSignal | null | undefined }[] = [];
      const aborts: number[] = [];
      let active = 0;
      let mostActive = 0;
      let bodyReads = 0;
      // This is the network boundary, not the reader or settings API. Its
      // abort behavior models fetch before headers and while reading a body.
      const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        calls.push({ path: new URL(String(input)).pathname, at: Date.now() - t0, signal: init?.signal });
        active += 1;
        mostActive = Math.max(mostActive, active);
        if (calls.length !== 2) {
          active -= 1;
          return Promise.resolve(Response.json(calls.length === 1
            ? { authenticated: true, username: 'ada' }
            : { authenticated: false }));
        }
        return new Promise<Response>((resolveResponse, rejectResponse) => {
          if (row.stall === TransportStall.Response) {
            init?.signal?.addEventListener('abort', () => {
              active -= 1;
              aborts.push(Date.now() - t0);
              rejectResponse(new DOMException('aborted', 'AbortError'));
            }, { once: true });
            return;
          }
          const body = new ReadableStream<Uint8Array>({
            start(controller) {
              init?.signal?.addEventListener('abort', () => {
                active -= 1;
                aborts.push(Date.now() - t0);
                controller.error(new DOMException('aborted', 'AbortError'));
              }, { once: true });
            },
          });
          const response = new Response(body, { status: 200 });
          const text = response.text.bind(response);
          response.text = () => {
            bodyReads += 1;
            return text();
          };
          resolveResponse(response);
        });
      });
      vi.stubGlobal('fetch', fetch);
      const { villageAuthFocusReader } = await import('./villageAuthFocus');
      const heard: VillageAuthReading[] = [];
      const leave = villageAuthFocusReader().subscribe((reading) => heard.push(reading));
      try {
        await vi.advanceTimersByTimeAsync(0);
        expect(heard).toEqual([{ authenticated: true, username: 'ada' }]);
        await vi.advanceTimersByTimeAsync(3000);
        window.dispatchEvent(new Event('focus'));
        await vi.advanceTimersByTimeAsync(500);
        window.dispatchEvent(new Event('focus'));
        expect(calls.map((call) => call.at)).toEqual([0, 3000]);
        // unstring-allow: endpoint-literal — pinned schema has no route members
        expect(calls.every((call) => call.path === '/api/v1/sync/auth')).toBe(true);
        expect(calls[1].signal).toBeDefined();
        expect(calls[1].signal?.aborted).toBe(false);
        expect(bodyReads).toBe(row.stall === TransportStall.Body ? 1 : 0);

        await vi.advanceTimersByTimeAsync(9499);
        expect(calls[1].signal?.aborted).toBe(false);
        expect(heard).toHaveLength(1);
        await vi.advanceTimersByTimeAsync(1);
        expect(calls[1].signal?.aborted).toBe(true);
        expect(aborts).toEqual([13000]);
        expect(heard).toEqual([{ authenticated: true, username: 'ada' }, null]);
        expect(active).toBe(0);
        await vi.advanceTimersByTimeAsync(1999);
        expect(calls).toHaveLength(2);
        await vi.advanceTimersByTimeAsync(1);
        expect(calls.map((call) => call.at)).toEqual([0, 3000, 15000]);
        expect(mostActive).toBe(1);
        expect(heard).toEqual([{ authenticated: true, username: 'ada' }, null, { authenticated: false }]);
      } finally {
        leave();
      }
      expect(vi.getTimerCount()).toBe(0);
    });
  }

  it('leaves independent settings reads without a focus-reader deadline', async () => {
    let reply: ((response: Response) => void) | undefined;
    const fetch = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => new Promise<Response>((resolveResponse) => {
      reply = resolveResponse;
    }));
    vi.stubGlobal('fetch', fetch);
    const { fetchVillageAuth } = await import('./settings');
    let settled = false;
    const read = fetchVillageAuth().then((result) => {
      settled = true;
      return result;
    });
    expect(fetch.mock.calls[0][1]?.signal).toBeUndefined();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(settled).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
    expect(reply).toBeDefined();
    reply?.(Response.json({ authenticated: false }));
    await expect(read).resolves.toEqual({ authenticated: false });
  });
});
