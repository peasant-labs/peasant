import { afterEach, describe, expect, it } from 'vitest';
import { loadCases, type FixtureCase } from '@/test/fixtures/kickstart';
import { playCase, teardown } from '@/test/kickstartHarness';

/**
 * The harness the kickstart pages are tested through must fail loudly when a
 * fixture is wrong, or every page test above it is only as good as its
 * spelling. Each case plays a small fixture case through the real `playCase`
 * and expects either that it plays or that it is refused with the words that
 * say why.
 */

afterEach(teardown);

const PATH = 'src/test/testdata/kickstart_harness.yaml';

describe('the kickstart fixture harness', () => {
  for (const entry of loadCases(PATH)) {
    it(entry.name, async () => {
      const played = { name: entry.name, ...(entry.play as Record<string, unknown>) } as FixtureCase;
      const own = (entry.own as string[] | undefined) ?? [];
      const failure = await playCase(played, <p>hello</p>, own).then(
        () => null,
        (error: unknown) => (error instanceof Error ? error : new Error(String(error))),
      );
      if (!Array.isArray(entry.refuses)) {
        expect(failure?.message, 'the harness should play this case').toBeUndefined();
        return;
      }
      expect(failure, 'the harness should refuse this case').not.toBeNull();
      for (const text of entry.refuses as string[]) expect(failure!.message).toContain(text);
    });
  }
});
