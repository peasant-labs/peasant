import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { ChangeDetailPayload, ChangeDiffPayload, ReviewListPayload } from '@peasant-labs/schema';
import { parseStrictYAML, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { ReviewSurface } from './ReviewSurface';
import type { ProjectHash } from '@/lib/navigation/projectRoutes';

vi.mock('next/navigation', () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock('@/lib/navigation/useProjectIdentity', () => ({
  useProjectIdentity: (identity: string) => ({ state: { phase: 'ready', requestedIdentity: identity, projectHash: identity, label: 'fixture project' } }),
}));
interface Fixture {
  projectHash: ProjectHash; branch: string; nextBranch: string; file: string; expectedText: string; error: string;
  review: ReviewListPayload; detail: ChangeDetailPayload; diff: ChangeDiffPayload;
  cases: Array<{ name: string; action: 'open' | 'retry' | 'scope-before' | 'scope-after' | 'unmount-before'; status: number }>;
}
const raw = requireRecord(parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/app/review/[[...segments]]/testdata/lazy-diff-mounted.yaml'), 'utf8'), 'mounted lazy diffs'), 'mounted lazy diffs');
requireUniqueNames(raw.cases as Record<string, unknown>[], 'mounted lazy diffs.cases');
const fixture = raw as unknown as Fixture;
expect(new Set(fixture.cases.map(row => row.name))).toEqual(new Set([
  'opening-child-file-fetches-and-caches', 'failed-file-read-can-retry',
  'scope-change-before-queued-read-skips-get', 'old-scope-completion-does-not-fill-new-branch',
  'unmount-before-queued-read-skips-get',
]));
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

// The real canonical child owns its file disclosure. A stand-in that asks for
// a diff during the HOST render cannot observe the child-only toggle regression.
describe('mounted Changes lazy file reads', () => {
  for (const row of fixture.cases) it(row.name, async () => {
    const reads: URL[] = [];
    let finishOld: ((response: Response) => void) | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname.endsWith('/diff')) {
        reads.push(url);
        if (row.action === 'scope-after' && url.searchParams.get('branch') === fixture.branch) return new Promise<Response>(resolve => { finishOld = resolve; });
        if (row.status !== 200 && reads.length === 1) return json({ error: fixture.error }, row.status);
        return json({ ...fixture.diff, branch: url.searchParams.get('branch') });
      }
      if (url.pathname.endsWith('/change')) return json({ ...fixture.detail, branch: url.searchParams.get('branch') });
      return json(fixture.review);
    }));
    const view = render(<ReviewSurface projectHash={fixture.projectHash} branch={fixture.branch} />);
    const path = await screen.findByText(fixture.file, { exact: true });
    expect(reads).toHaveLength(0);
    fireEvent.click(path);
    if (row.action === 'scope-before' || row.action === 'unmount-before') {
      if (row.action === 'unmount-before') view.unmount();
      else view.rerender(<ReviewSurface projectHash={fixture.projectHash} branch={fixture.nextBranch} />);
      await act(async () => { await Promise.resolve(); });
      expect(reads.filter(url => url.searchParams.get('branch') === fixture.branch)).toHaveLength(0);
      return;
    }
    await waitFor(() => expect(reads).toHaveLength(1));
    expect(reads[0].pathname).toBe(`/api/v1/review/${fixture.projectHash}/diff`);
    expect(reads[0].searchParams.get('file')).toBe(fixture.file);
    expect(reads[0].searchParams.get('branch')).toBe(fixture.branch);
    if (row.action === 'scope-after') {
      view.rerender(<ReviewSurface projectHash={fixture.projectHash} branch={fixture.nextBranch} />);
      await screen.findByText('Api timeout', { exact: true });
      await act(async () => { finishOld?.(json(fixture.diff)); });
      expect(screen.queryByText(fixture.expectedText, { exact: false })).not.toBeInTheDocument();
      fireEvent.click(screen.getByText(fixture.file, { exact: true }));
      await waitFor(() => expect(reads.some(url => url.searchParams.get('branch') === fixture.nextBranch)).toBe(true));
    } else if (row.action === 'retry') {
      await screen.findByText(new RegExp(fixture.error));
      fireEvent.click(screen.getByRole('button', { name: 'retry file diffs' }));
      await waitFor(() => expect(reads).toHaveLength(2));
    }
    await screen.findByText(fixture.expectedText, { exact: false });
    expect(screen.queryByText('loading diff…')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'retry file diffs' })).not.toBeInTheDocument();
    if (row.action === 'open') {
      fireEvent.click(screen.getByText(fixture.file, { exact: true }));
      fireEvent.click(screen.getByText(fixture.file, { exact: true }));
      await act(async () => { await Promise.resolve(); });
      expect(reads).toHaveLength(1);
    }
  });
});
