import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parse } from 'yaml';
import type { SyncAuthResponse } from '@peasant-labs/schema';
import { createVillageAuthFocusReader, VILLAGE_AUTH_FOCUS_GAP_MS, type VillageAuthReading } from './villageAuthFocus';

interface TimelineEvent {
  at: number;
  subscribe?: string;
  unsubscribe?: string;
  focus?: true;
  pendingTimers?: number;
  listeners?: number;
}
interface Case {
  name: string;
  reads: { ms: number; answer: string }[];
  timeline: TimelineEvent[];
  until: number;
  expectReads: number[];
  expectAborts?: number[];
  expectHeard: Record<string, string[]>;
  expectListening: boolean;
}

const FIXTURE = 'src/lib/api/testdata/village_auth_focus.yaml';
const fixture = parse(readFileSync(resolve(process.cwd(), FIXTURE), 'utf8')) as { requiredNames: string[]; cases: Case[] };
const EVENT_KEYS = ['at', 'subscribe', 'unsubscribe', 'focus', 'pendingTimers', 'listeners'];

/** What a subscriber heard, in the fixture's words. */
function heard(reading: VillageAuthReading): string {
  if (reading === null) return 'no account';
  return reading.authenticated ? reading.username ?? 'signed in' : 'signed out';
}

/** A window stand-in that says how many focus listeners it holds. */
class CountingTarget extends EventTarget {
  listening = 0;
  override addEventListener(...args: Parameters<EventTarget['addEventListener']>): void {
    if (args[0] === 'focus') this.listening += 1;
    super.addEventListener(...args);
  }
  override removeEventListener(...args: Parameters<EventTarget['removeEventListener']>): void {
    if (args[0] === 'focus') this.listening -= 1;
    super.removeEventListener(...args);
  }
}

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

/**
 * The shared sign-in reader played on a fake clock against scripted answers:
 * the real reader, with its clock, timers and window injected.
 */
describe('the sign-in read on window focus', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });

  it('waits two seconds between reads', () => {
    expect(VILLAGE_AUTH_FOCUS_GAP_MS).toBe(2000);
  });

  for (const row of fixture.cases) {
    it(row.name, async () => {
      for (const event of row.timeline) {
        const unknown = Object.keys(event).filter((key) => !EVENT_KEYS.includes(key));
        expect(unknown, `${row.name}: event keys the test reads (${EVENT_KEYS.join(', ')})`).toEqual([]);
      }
      const t0 = Date.now();
      const started: number[] = [];
      const aborted: number[] = [];
      let inFlight = 0;
      let mostInFlight = 0;
      const read = (signal: AbortSignal) => {
        const answer = row.reads[Math.min(started.length, row.reads.length - 1)];
        started.push(Date.now() - t0);
        inFlight += 1;
        mostInFlight = Math.max(mostInFlight, inFlight);
        if (answer.answer === 'throw') {
          inFlight -= 1;
          throw new Error('the sign-in could not be read');
        }
        return new Promise<SyncAuthResponse>((done, fail) => {
          const waitsForAbort = answer.answer === 'stall' || answer.answer === 'abort-success' || answer.answer === 'abort-delay-success';
          const timer = waitsForAbort ? null : setTimeout(() => {
            inFlight -= 1;
            if (answer.answer === 'fail') fail(new Error('the sign-in could not be read'));
            else if (answer.answer === 'signed out') done({ authenticated: false } as SyncAuthResponse);
            else done({ authenticated: true, username: answer.answer } as SyncAuthResponse);
          }, answer.ms);
          signal.addEventListener('abort', () => {
            if (timer !== null) clearTimeout(timer);
            aborted.push(Date.now() - t0);
            if (answer.answer === 'abort-delay-success') {
              setTimeout(() => {
                inFlight -= 1;
                done({ authenticated: true, username: 'stale' } as SyncAuthResponse);
              }, answer.ms);
              return;
            }
            inFlight -= 1;
            if (answer.answer === 'abort-success') done({ authenticated: true, username: 'stale' } as SyncAuthResponse);
            else fail(new DOMException('request aborted', 'AbortError'));
          }, { once: true });
        });
      };
      const target = new CountingTarget();
      const reader = createVillageAuthFocusReader({
        read,
        now: () => Date.now(),
        setTimer: (run, ms) => setTimeout(run, ms),
        clearTimer: (timer) => clearTimeout(timer as ReturnType<typeof setTimeout>),
        target,
      });
      const said: Record<string, string[]> = {};
      const leave: Record<string, () => void> = {};
      let clock = 0;
      for (const event of [...row.timeline].sort((a, b) => a.at - b.at)) {
        await vi.advanceTimersByTimeAsync(event.at - clock);
        clock = event.at;
        if (event.subscribe !== undefined) {
          const name = event.subscribe;
          said[name] = [];
          leave[name] = reader.subscribe((reading) => said[name].push(heard(reading)));
        }
        if (event.unsubscribe !== undefined) leave[event.unsubscribe]();
        if (event.focus) target.dispatchEvent(new Event('focus'));
        if (event.listeners !== undefined) expect(target.listening, `${row.name}: focus listeners at ${event.at} ms`).toBe(event.listeners);
        if (event.pendingTimers !== undefined) expect(vi.getTimerCount(), `${row.name}: timers waiting at ${event.at} ms`).toBe(event.pendingTimers);
      }
      await vi.advanceTimersByTimeAsync(row.until - clock);
      expect(mostInFlight, `${row.name}: reads in flight at once`).toBe(1);
      expect(started, `${row.name}: when each read started`).toEqual(row.expectReads);
      expect(aborted, `${row.name}: when requests were aborted`).toEqual(row.expectAborts ?? []);
      expect(said, `${row.name}: what each subscriber heard`).toEqual(row.expectHeard);
      expect(target.listening > 0, `${row.name}: still listening for focus`).toBe(row.expectListening);
      expect(target.listening, `${row.name}: focus listeners held`).toBeLessThanOrEqual(1);
    });
  }
});
