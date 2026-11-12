import { useEffect, useState, type ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { loadCases, type FixtureCase } from '@/test/fixtures/kickstart';
import { playCase, teardown } from '@/test/kickstartHarness';

/**
 * The harness the kickstart pages are tested through must fail loudly when a
 * fixture is wrong, or every page test above it is only as good as its
 * spelling. Each case plays a small fixture case through the real `playCase`
 * and expects either that it plays or that it is refused with the words that
 * say why.
 */

// The router's redirect, as the playing test hands it to `playCase`.
const redirect = vi.fn();

beforeEach(() => {
  redirect.mockReset();
});
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

/** A page that reads one session's forest once it is drawn and shows how many rows it was given. */
function TreeProbe() {
  const [text, setText] = useState('waiting');
  useEffect(() => {
    void fetch('/api/v1/kickstart/sessions/ses-x/tree')
      .then((response) => response.json())
      .then((body: { nodes: unknown[] }) => setText(`rows: ${body.nodes.length}`));
  }, []);
  return <p>{text}</p>;
}

/**
 * A page that reads the status once it is drawn, redirects to setup when it is
 * told setup is not saved, and shows what it was told only after it has decided,
 * so a case can wait for the decision before it says the page did not redirect.
 */
function RedirectProbe() {
  const [text, setText] = useState('waiting');
  useEffect(() => {
    void fetch('/api/v1/kickstart/status')
      .then((response) => response.json())
      .then((body: { configured: boolean }) => {
        if (!body.configured) redirect('/kickstart');
        setText(`configured: ${body.configured ? 'yes' : 'no'}`);
      });
  }, []);
  return <p>{text}</p>;
}

/** The pages a fixture case may name with `page`. */
const PAGES: Record<string, ReactElement> = {
  text: <p>hello</p>,
  'status-probe': <StatusProbe />,
  'tree-probe': <TreeProbe />,
  'redirect-probe': <RedirectProbe />,
};

describe('the kickstart fixture harness', () => {
  for (const entry of loadCases(PATH)) {
    it(entry.name, async () => {
      // The file's `defaultServer` is already in `entry.server`; the played case's own routes win.
      const playedCase = entry.play as Record<string, unknown>;
      const played = { name: entry.name, ...playedCase, server: { ...((entry.server ?? {}) as object), ...((playedCase.server ?? {}) as object) } } as FixtureCase;
      const own = (entry.own as string[] | undefined) ?? [];
      const page = PAGES[(entry.page as string | undefined) ?? 'text'];
      if (page === undefined) throw new Error(`${PATH}: case ${entry.name} names the page ${JSON.stringify(entry.page)}; the pages are ${Object.keys(PAGES).join(', ')}`);
      const failure = await playCase(played, page, own, { redirect }).then(
        () => null,
        (error: unknown) => (error instanceof Error ? error : new Error(String(error))),
      );
      if (Array.isArray(entry.teardownRefuses)) {
        // The case plays; what fails is the teardown that follows it.
        expect(failure?.message, 'the harness should play this case').toBeUndefined();
        let teardownFailure: unknown = null;
        try {
          teardown();
        } catch (error) {
          teardownFailure = error;
        }
        expect(teardownFailure, 'the teardown should refuse this case').toBeInstanceOf(Error);
        for (const text of entry.teardownRefuses as string[]) expect((teardownFailure as Error).message).toContain(text);
        return;
      }
      if (!Array.isArray(entry.refuses)) {
        expect(failure?.message, 'the harness should play this case').toBeUndefined();
        return;
      }
      expect(failure, 'the harness should refuse this case').not.toBeNull();
      for (const text of entry.refuses as string[]) expect(failure!.message).toContain(text);
    });
  }
});
