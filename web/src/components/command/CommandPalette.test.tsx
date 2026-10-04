import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, cleanup, fireEvent, act, waitFor } from '@testing-library/react';
import { parseStrictYAML, requireExactRequiredFields, requireRecord } from '@/test/strictYaml';
import {
  CommandPalette,
  directLookupLabel,
  filterCommands,
  isDirectLookupResult,
  OPEN_COMMAND_PALETTE_EVENT,
  type Command,
} from './CommandPalette';
import { projectViewerStateFixture } from '@/components/picker/projectViewerStateFixtures';
import { loadShellHeaderManifest, paletteFailures } from '../../../scripts/visual/shell-header-manifest.mjs';
import { SHELL_HEADER_CASES } from '@/test/fixtures/shellHeaderCases';

const push = vi.fn();
vi.mock('next/navigation', () => ({ useRouter: () => ({ push }) }));

const toggle = vi.fn();
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'light', toggle }) }));

// The "go to" commands are the header's nav sections, read with the
// server-advertised capability set (ServerCapabilitiesContext). Tests drive the
// ReadonlySet<string> of advertised tokens directly — the shape the real
// provider exposes. The default advertises the code-map token, so every case
// also proves an experimental server brings no route-only section back.
const CODE_MAP_ENABLED: ReadonlySet<string> = new Set(['code_map_navigation_v1']);
let capabilities: ReadonlySet<string> = CODE_MAP_ENABLED;
const shellManifest = loadShellHeaderManifest();
vi.mock('@/contexts/ServerCapabilitiesContext', () => ({
  useServerCapabilities: () => ({
    status: 'ready',
    capabilities,
  }),
}));

const parentVisibleFixture = projectViewerStateFixture('explicit session makes parent visible');
const PROJECT_HASH = parentVisibleFixture.summary.projects[0].projectHash;
const fixturePath = resolve(process.cwd(), 'src/components/command/testdata/search_discovery.yaml');
const fixture = requireRecord(parseStrictYAML(readFileSync(fixturePath, 'utf8'), 'search discovery fixture'), 'search discovery fixture');
requireExactRequiredFields(fixture, ['valid', 'invalid'], 'search discovery fixture');
const validFixture = requireRecord(fixture.valid, 'search discovery fixture.valid');
requireExactRequiredFields(validFixture, ['search', 'discovery'], 'search discovery fixture.valid');
const invalidFixtures = fixture.invalid as Array<Record<string, unknown>>;
if (invalidFixtures.length !== 4) throw new Error(`search discovery fixture must contain exactly 4 invalid rows, got ${invalidFixtures.length}`);

const directLookupFixturePath = resolve(process.cwd(), 'src/components/command/testdata/search_direct_lookup.yaml');
const directLookupFixture = requireRecord(parseStrictYAML(readFileSync(directLookupFixturePath, 'utf8'), 'search direct lookup fixture'), 'search direct lookup fixture');
requireExactRequiredFields(directLookupFixture, ['valid'], 'search direct lookup fixture');
const directLookupValid = requireRecord(directLookupFixture.valid, 'search direct lookup fixture.valid');
requireExactRequiredFields(directLookupValid, ['search', 'discovery'], 'search direct lookup fixture.valid');

/**
 * Wrap the fixture's flat search results in the opt-in grouped envelope the
 * production palette reads (`view=grouped`). Each flat hit becomes the owner
 * transcript's own match, which is exactly how the grouped route carries an
 * ordinary result. Discovery validation is what the invalid cases exercise.
 */
function groupedEnvelope(search: Record<string, unknown>) {
  const results = Array.isArray(search.results) ? (search.results as Array<Record<string, unknown>>) : [];
  return {
    items: results.map((result) => ({
      kind: 'transcript',
      transcript: {
        session: {
          id: result.sessionId,
          harness: 'codex',
          startTime: '2026-06-01T09:00:00Z',
          durationMins: 1,
          turnCount: 1,
          totalTokens: 1,
          toolCallCount: 0,
          project: result.project,
          projectHash: result.projectHash,
        },
        matches: [result],
      },
      helperGroups: [],
    })),
    page: 1,
    limit: 20,
    totalItems: results.length,
    ordinarySessionTotal: results.length,
    helperThreadTotal: 0,
  };
}

const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

beforeEach(() => {
  push.mockClear();
  toggle.mockClear();
  capabilities = CODE_MAP_ENABLED;
  fetchMock.mockReset();
  fetchMock.mockImplementation(async (input: string | URL) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname === '/api/v1/search') return Response.json(groupedEnvelope({ results: [] }));
    if (url.pathname === '/api/v1/web/discovery') return Response.json({ items: [] });
    throw new Error(`unexpected test request ${url.pathname}`);
  });
});
afterEach(() => cleanup());

describe('filterCommands', () => {
  const cmds: Command[] = [
    { id: 'a', label: 'Go to Code Map', group: 'Go to', run: () => {} },
    { id: 'b', label: 'alpha — Changes', group: 'Project', keywords: '/work/alpha-project', run: () => {} },
    { id: 'c', label: 'Toggle theme', group: 'Action', keywords: 'dark light', run: () => {} },
  ];

  it('returns all on empty query', () => {
    expect(filterCommands(cmds, '   ')).toHaveLength(3);
  });
  it('matches label, group, and keywords case-insensitively', () => {
    expect(filterCommands(cmds, 'MAP').map((c) => c.id)).toEqual(['a']);
    expect(filterCommands(cmds, 'alpha-project').map((c) => c.id)).toEqual(['b']); // raw path keyword
    expect(filterCommands(cmds, 'dark').map((c) => c.id)).toEqual(['c']); // keyword
    expect(filterCommands(cmds, 'project').map((c) => c.id)).toEqual(['b']); // group
  });
});

describe('direct lookup rows', () => {
  it('treats an empty or whitespace-only snippet as a direct lookup', () => {
    expect(isDirectLookupResult({ snippet: '' })).toBe(true);
    expect(isDirectLookupResult({ snippet: '   ' })).toBe(true);
    expect(isDirectLookupResult({ snippet: 'fix the [pipeline] retry' })).toBe(false);
  });

  it('labels a direct hit with its session and project identity', () => {
    expect(directLookupLabel({ sessionId: 'sess-abc', project: '/work/alpha-project' })).toBe(
      'session sess-abc in alpha-project',
    );
  });
});

describe('CommandPalette', () => {
  function open() {
    render(<CommandPalette />);
    act(() => {
      window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT));
    });
  }

  it('is hidden until opened, then shows the search box', () => {
    render(<CommandPalette />);
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
    act(() => {
      window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT));
    });
    expect(screen.getByRole('combobox')).toBeInTheDocument();
  });

  it('navigates to a nav section on Enter', () => {
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'go to home' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(push).toHaveBeenCalledWith('/');
  });

  // The palette links to what the header links to and nothing more: no
  // per-project jumps into changes or the code map, and no "go to" command for
  // a route-only section — with or without the code-map capability.
  it.each(SHELL_HEADER_CASES.paletteCapabilities.map((set) => [set.name, set.tokens] as const))('holds the shell manifest palette rules with capability set %s advertised', async (_name, tokens) => {
    capabilities = new Set(tokens);
    open();
    // Let any first-open request settle before reading the commands.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(paletteFailures(shellManifest)).toEqual([]);
    expect(screen.getByText('go to home')).toBeInTheDocument();
    for (const label of ['go to analytics', 'go to changes', 'go to code map', 'alpha-project · changes', 'alpha-project · map']) {
      expect(screen.queryByText(label)).not.toBeInTheDocument();
    }
    // Project jumps are gone, so opening the palette no longer loads projects.
    expect(fetchMock).not.toHaveBeenCalledWith(expect.stringContaining('/api/v1/projects/summary'));
  });

  it('retries failed search discovery on the same surface and shows the results', async () => {
    const valid = validFixture.search as Record<string, unknown>;
    let discoveryCalls = 0;
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/search') return Response.json(groupedEnvelope(valid));
      if (url.pathname === '/api/v1/web/discovery') {
        discoveryCalls += 1;
        if (discoveryCalls === 1) throw new Error('discovery unavailable');
        return Response.json(validFixture.discovery);
      }
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'pipeline' } });
    fireEvent.click(await screen.findByRole('button', { name: 'retry search discovery' }));
    expect(await screen.findByText('fix the [pipeline] retry')).toBeInTheDocument();
    expect(discoveryCalls).toBe(2);
  });

  it('does not search for queries shorter than 2 characters', async () => {
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'a' } });
    // Give the debounce window time to (not) fire. Wrapped in act so the
    // first-open project fetch settling inside this window is flushed cleanly.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 250));
    });
    expect(fetchMock).not.toHaveBeenCalledWith(expect.stringContaining('/api/v1/search'));
    expect(screen.queryByText('Messages')).not.toBeInTheDocument();
  });

  it('searches transcripts and deep-links a Messages hit to its task turn', async () => {
    const valid = validFixture.search as Record<string, unknown>;
    const discovery = validFixture.discovery;
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/search') return Response.json(groupedEnvelope(valid));
      if (url.pathname === '/api/v1/web/discovery') return Response.json(discovery);
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'pipeline' } });

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/search?q=pipeline&view=grouped&limit=20')));
    const hit = await screen.findByText('fix the [pipeline] retry');
    expect(screen.getAllByText('Messages')).toHaveLength(2);
    expect(screen.getAllByTestId('search-annotation')).toHaveLength(2);
    expect(screen.getAllByTestId('search-annotation')[0]).toHaveTextContent('alpha-main·main·selected');
    expect(screen.getAllByTestId('search-annotation')[1]).toHaveTextContent('alpha-feature·main·unselected');
    expect(screen.getByText("<script>alert('unsafe')</script> pipeline")).toBeInTheDocument();
    expect(screen.queryByText(/opaque-location/)).not.toBeInTheDocument();
    const unselectedHit = screen.getByText("<script>alert('unsafe')</script> pipeline");
    fireEvent.mouseDown(unselectedHit);
    expect(push).toHaveBeenCalledWith(`/projects/${PROJECT_HASH}/sess-unselected?turn=7`);
  });

  it('keeps discovery metadata visible and fully described for each result', async () => {
    const valid = validFixture.search as Record<string, unknown>;
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/search') return Response.json(groupedEnvelope(valid));
      if (url.pathname === '/api/v1/web/discovery') return Response.json(validFixture.discovery);
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'pipeline' } });

    const annotation = (await screen.findAllByTestId('search-annotation'))[1];
    expect(annotation).toBeVisible();
    const resultButton = annotation.closest('button');
    expect(resultButton).not.toBeNull();
    const descriptionId = resultButton?.getAttribute('aria-describedby');
    expect(descriptionId).toBeTruthy();
    expect(document.getElementById(descriptionId ?? '')).toHaveTextContent('alpha-feature·main·unselected');
  });

  it.each(invalidFixtures.map((row) => [String(row.name), row] as const))('fails closed for %s', async (_name, row) => {
    const search = row.search as Record<string, unknown>;
    const discovery = row.discovery;
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/search') return Response.json(groupedEnvelope(search));
      if (url.pathname === '/api/v1/web/discovery') return Response.json(discovery);
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'pipeline' } });
    expect(await screen.findByRole('alert')).toHaveTextContent(/discovery/i);
    expect(screen.queryByText(/pipeline/)).not.toBeInTheDocument();
  });

  it('renders an id-match row beside a content-match row and opens its transcript', async () => {
    const search = directLookupValid.search as Record<string, unknown>;
    const query = String((search as { query: unknown }).query);
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/search') return Response.json(groupedEnvelope(search));
      if (url.pathname === '/api/v1/web/discovery') return Response.json(directLookupValid.discovery);
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: query } });

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/search?q=')));
    // The existing FTS5 row renders its snippet unchanged.
    expect(await screen.findByText('fix the [pipeline] retry')).toBeInTheDocument();
    // The direct-lookup row identifies the session in its project, not a snippet.
    const directRow = await screen.findByText(`session ${query} in alpha-project`);
    expect(directRow).toBeInTheDocument();
    // No blank or snippet-less row renders alongside the two identified rows.
    expect(screen.getAllByTestId('search-annotation')).toHaveLength(2);
    for (const item of screen.getAllByRole('option')) {
      expect(item.textContent?.trim().length).toBeGreaterThan(0);
    }
    fireEvent.mouseDown(directRow);
    expect(push).toHaveBeenCalledWith(`/projects/${PROJECT_HASH}/${query}?turn=0`);
  });

  it('runs the theme action and closes on Escape', () => {
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'Toggle' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(toggle).toHaveBeenCalled();

    // Reopen + Escape closes.
    act(() => {
      window.dispatchEvent(new Event(OPEN_COMMAND_PALETTE_EVENT));
    });
    fireEvent.keyDown(screen.getByRole('combobox'), { key: 'Escape' });
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
  });
});
