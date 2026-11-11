import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { parse } from 'yaml';
import { fetchVillageAuth } from './publishing';
import { watchVillageSignIn, type SignInWatch } from './villageSignIn';

vi.mock('./publishing', () => ({ fetchVillageAuth: vi.fn() }));

interface Step { advance?: number; stop?: true; checks: number; outcome?: SignInWatch }
interface Case {
  name: string;
  options: { pollMs?: number; timeoutMs?: number; checkNow?: boolean };
  answers: ({ authenticated: boolean } | { fail: true })[];
  steps: Step[];
}

const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/village-sign-in-watch.yaml'), 'utf8')) as { requiredNames: string[]; cases: Case[] };

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.mocked(fetchVillageAuth).mockReset();
});

/**
 * The wait for the browser sign-in, played on a fake clock against scripted
 * answers of the real sign-in check. The popup and the setup flow wait through
 * this one function, so what they share is what is held here.
 */
describe('waiting for the browser sign-in', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });

  for (const row of fixture.cases) {
    it(row.name, async () => {
      let asked = 0;
      vi.mocked(fetchVillageAuth).mockImplementation(async () => {
        const answer = row.answers[Math.min(asked, row.answers.length - 1)];
        asked += 1;
        if ('fail' in answer) throw new Error('the check could not be made');
        return { authenticated: answer.authenticated } as Awaited<ReturnType<typeof fetchVillageAuth>>;
      });
      const stop = new AbortController();
      let outcome: SignInWatch | undefined;
      void watchVillageSignIn({ ...row.options, signal: stop.signal }).then((result) => {
        outcome = result;
      });
      for (const [position, step] of row.steps.entries()) {
        if (step.stop) stop.abort();
        await vi.advanceTimersByTimeAsync(step.advance ?? 0);
        expect(asked, `step ${position + 1}: checks made`).toBe(step.checks);
        expect(outcome, `step ${position + 1}: ${step.outcome === undefined ? 'the wait is still going' : `the wait ended ${step.outcome}`}`).toBe(step.outcome);
      }
    });
  }
});
