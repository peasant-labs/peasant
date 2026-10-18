import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { useState } from 'react';
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { createPublishWorld, loadPublishStates } from '@/app/share/testdata/publish-popup-states.mjs';
import { useRedactionPipeline, type RedactionCache, type RedactionCacheEntry } from './redactionScan';
import { boundaryDomain, boundaryResponse, REDACTION_REVIEW_BOUNDARIES } from '@/test/fixtures/redaction-review-boundaries';
import * as mockData from '@/lib/share/mock-data';

interface Case { name: string; initialStatus: 'success' | 'failure'; cachedVersion?: string; targetVersion?: string; scans: number; rescan?: boolean }
const fixture = requireRecord(parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/redaction-cache-versions.yaml'), 'utf8'), 'scan versions fixture'), 'scan versions fixture') as unknown as { requiredNames: string[]; cases: Case[] };
requireExactRequiredFields(fixture as unknown as Record<string, unknown>, ['requiredNames', 'cases'], 'scan versions fixture');
requireUniqueNames(fixture.cases as unknown as Record<string, unknown>[], 'scan versions fixture.cases');
for (const entry of fixture.cases) requireExactFields(entry as unknown as Record<string, unknown>, ['name', 'initialStatus', 'cachedVersion', 'targetVersion', 'scans', 'rescan'], `scan versions fixture.${entry.name}`);
const states = loadPublishStates(readFileSync(resolve(process.cwd(), 'src/app/share/testdata/publish-popup-states.yaml'), 'utf8'));
const ready = states.cases.find((entry) => entry.name === 'ready-to-publish')!;
const SESSION = 'session';
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('untrusted scan success', () => {
  it('invalidates the action guard before a deferred failure cache write renders', async () => {
    const entry = REDACTION_REVIEW_BOUNDARIES.find((row) => row.name === 'unknown-item')!;
    vi.stubGlobal('fetch', vi.fn(async () => Response.json(boundaryResponse(entry.mutation))));
    const cache: RedactionCache = new Map([['standard:session', { status: 'success', redactions: boundaryDomain('none') }]]);
    // Model an owner whose state update has been queued, but not rendered yet.
    const update = vi.fn();
    const hook = renderHook(() => useRedactionPipeline([{ id: SESSION }], 'standard', false, cache, update));
    expect(hook.result.current.isReviewReady()).toBe(true);
    act(() => hook.result.current.runScan(true));
    expect(hook.result.current.isReviewReady()).toBe(false);
    await waitFor(() => expect(update).toHaveBeenCalledTimes(2));
    expect(update.mock.calls[1][0](cache).get('standard:session').status).toBe('failure');
    expect(hook.result.current.isReviewReady()).toBe(false);
  });

  it.each(REDACTION_REVIEW_BOUNDARIES.filter((entry) => entry.source === 'cache' || entry.source === 'mock'))('$name fails before readiness and recovers only after explicit scan', async (entry) => {
    const fetcher = vi.fn(async () => Response.json(boundaryResponse()));
    vi.stubGlobal('fetch', fetcher);
    const mock = vi.spyOn(mockData, 'fetchMockRedactionPreview').mockReturnValue(boundaryDomain(entry.mutation).map((item) => ({ ...item, minimumLevel: entry.minimumLevel ?? 'standard' })));
    let first = true;
    const hook = renderHook(() => {
      const [cache, setCache] = useState<RedactionCache>(() => entry.source === 'cache'
        ? new Map([['standard:session', { status: 'success', redactions: boundaryDomain(entry.mutation) }]]) : new Map());
      const result = useRedactionPipeline([{ id: SESSION }], 'standard', entry.source === 'mock', cache, setCache);
      if (first && entry.source === 'cache') {
        expect(result.phase).toBe('ready');
        expect(result.scanError).toContain(entry.error);
        expect(result.sessionRedactions.size).toBe(0);
        expect(result.isReviewReady()).toBe(false);
      }
      first = false;
      return { ...result, cache };
    });
    await waitFor(() => expect(hook.result.current.scanError).toContain(entry.error));
    expect(hook.result.current.cache.get('standard:session')?.status).toBe('failure');
    expect(fetcher).not.toHaveBeenCalled();
    expect(hook.result.current.isReviewReady()).toBe(false);
    mock.mockReturnValue(boundaryDomain('none').map((item) => ({ ...item, minimumLevel: 'standard' })));
    act(() => hook.result.current.runScan(true));
    await waitFor(() => expect(hook.result.current.scanError).toBeNull());
    expect(hook.result.current.isReviewReady()).toBe(true);
  });
});

describe('scan cache content versions', () => {
  it('keeps the required scenarios', () => {
    expect(new Set(fixture.cases.map((entry) => entry.name))).toEqual(new Set(fixture.requiredNames));
  });
  it.each(fixture.cases)('$name', async (entry) => {
    const world = createPublishWorld(states, ready, { sessionId: SESSION, turns: [] });
    let release!: () => void;
    const held = new Promise<void>((done) => { release = done; });
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      const answer = world.respond({ method: 'GET', url });
      await held;
      if (!answer || !('json' in answer)) throw new Error('unexpected scan answer');
      return Response.json(answer.json, { status: answer.status });
    }));
    const cached: RedactionCacheEntry = entry.initialStatus === 'success'
      ? { status: 'success', version: entry.cachedVersion, redactions: [] }
      : { status: 'failure', version: entry.cachedVersion, error: 'scan failed' };
    const hook = renderHook(() => {
      const [cache, setCache] = useState<RedactionCache>(() => new Map([['standard:session', cached]]));
      return useRedactionPipeline([{ id: SESSION, version: entry.targetVersion }], 'standard', false, cache, setCache);
    });
    expect(world.scanRequests).toBe(entry.scans);
    if (entry.scans) {
      expect(hook.result.current.phase).toBe('scanning');
      expect(hook.result.current.sessionRedactions.has(SESSION)).toBe(false);
      await act(async () => release());
      await waitFor(() => expect(hook.result.current.phase).toBe('ready'));
      expect(hook.result.current.sessionRedactions.get(SESSION)).toHaveLength(states.matches.length);
      expect(hook.result.current.sessionMatchCounts.get(SESSION)).toBe(states.matches.reduce((total, match) => total + (match.count ?? 1), 0));
    } else if (entry.rescan) {
      expect(hook.result.current.scanError).toBe('scan failed');
      act(() => hook.result.current.runScan(true));
      expect(world.scanRequests).toBe(1);
      await act(async () => release());
      await waitFor(() => expect(hook.result.current.scanError).toBeNull());
    } else expect(hook.result.current.phase).toBe('ready');
  });
});
