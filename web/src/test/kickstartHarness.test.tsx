import { useEffect, useState, type ReactElement } from 'react';
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

/** A page that asks the status route once it is drawn and shows what it was told. */
function StatusProbe() {
  const [text, setText] = useState('waiting');
  useEffect(() => {
    void fetch('/api/v1/kickstart/status')
      .then((response) => response.json())
      .then((body: { configured: boolean }) => setText(`configured: ${body.configured ? 'yes' : 'no'}`));
  }, []);
  return <p>{text}</p>;
}

/** The pages a fixture case may name with `page`. */
const PAGES: Record<string, ReactElement> = {
  text: <p>hello</p>,
  'status-probe': <StatusProbe />,
};

describe('the kickstart fixture harness', () => {
  for (const entry of loadCases(PATH)) {
    it(entry.name, async () => {
      const played = { name: entry.name, ...(entry.play as Record<string, unknown>) } as FixtureCase;
      const own = (entry.own as string[] | undefined) ?? [];
      const page = PAGES[(entry.page as string | undefined) ?? 'text'];
      if (page === undefined) throw new Error(`${PATH}: case ${entry.name} names the page ${JSON.stringify(entry.page)}; the pages are ${Object.keys(PAGES).join(', ')}`);
      const failure = await playCase(played, page, own).then(
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
