import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, cleanup, fireEvent, act, waitFor } from '@testing-library/react';
import { parseStrictYAML, requireExactRequiredFields, requireRecord } from '@/test/strictYaml';
import {
  CommandPalette,
  filterCommands,
  OPEN_COMMAND_PALETTE_EVENT,
  type Command,
} from './CommandPalette';
import { projectViewerStateFixture } from '@/components/picker/projectViewerStateFixtures';

const push = vi.fn();
vi.mock('next/navigation', () => ({ useRouter: () => ({ push }) }));

const toggle = vi.fn();
vi.mock('@/hooks/useTheme', () => ({ useTheme: () => ({ theme: 'light', toggle }) }));

const parentVisibleFixture = projectViewerStateFixture('explicit session makes parent visible');
const PROJECT_HASH = parentVisibleFixture.summary.projects[0].projectHash;
const fixturePath = resolve(process.cwd(), 'src/components/command/testdata/search_discovery.yaml');
const fixture = requireRecord(parseStrictYAML(readFileSync(fixturePath, 'utf8'), 'search discovery fixture'), 'search discovery fixture');
requireExactRequiredFields(fixture, ['valid', 'invalid'], 'search discovery fixture');
const validFixture = requireRecord(fixture.valid, 'search discovery fixture.valid');
requireExactRequiredFields(validFixture, ['search', 'discovery'], 'search discovery fixture.valid');
const invalidFixtures = fixture.invalid as Array<Record<string, unknown>>;
if (invalidFixtures.length !== 4) throw new Error(`search discovery fixture must contain exactly 4 invalid rows, got ${invalidFixtures.length}`);

const fetchMock = vi.fn();
vi.stubGlobal('fetch', fetchMock);

beforeEach(() => {
  push.mockClear();
  toggle.mockClear();
  fetchMock.mockReset();
  fetchMock.mockImplementation(async (input: string | URL) => {
    const url = new URL(String(input), 'http://localhost');
    if (url.pathname === '/api/v1/projects/summary') return Response.json(parentVisibleFixture.summary);
    if (url.pathname === '/api/v1/search') return Response.json({ query: url.searchParams.get('q'), results: [] });
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
    fireEvent.change(input, { target: { value: 'Go to Code Map' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(push).toHaveBeenCalledWith('/map');
  });

  it('lists the explicit session parent from the shared summary and jumps to its map', async () => {
    open();
    const input = screen.getByRole('combobox');
    // Project commands appear once the summary fetch resolves.
    await waitFor(() => expect(screen.getByText('alpha-project · map')).toBeInTheDocument());
    fireEvent.change(input, { target: { value: 'alpha-project · map' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(push).toHaveBeenCalledWith(`/map/${PROJECT_HASH}`);
    expect(screen.queryByRole('status', { name: 'project selection recovery' })).not.toBeInTheDocument();
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
  });

  it('retries failed project discovery on the same surface and repopulates projects', async () => {
    let projectCalls = 0;
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/projects/summary') {
        projectCalls += 1;
        if (projectCalls === 1) throw new Error('database unavailable');
        return Response.json({ projects: [{ projectHash: PROJECT_HASH, project: '/work/alpha-project', sessions: 3, recordedFiles: 1, totalFiles: 2, openChanges: 1 }] });
      }
      if (url.pathname === '/api/v1/search') return Response.json({ query: url.searchParams.get('q'), results: [] });
      if (url.pathname === '/api/v1/web/discovery') return Response.json({ items: [] });
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();

    fireEvent.click(await screen.findByRole('button', { name: 'retry project discovery' }));
    expect(await screen.findByText('alpha-project · map')).toBeInTheDocument();
    expect(projectCalls).toBe(2);
  });

  it('does not search for queries shorter than 2 characters', async () => {
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'a' } });
    // Give the debounce window time to (not) fire.
    await new Promise((r) => setTimeout(r, 250));
    expect(fetchMock).not.toHaveBeenCalledWith(expect.stringContaining('/api/v1/search'));
    expect(screen.queryByText('Messages')).not.toBeInTheDocument();
  });

  it('searches transcripts and deep-links a Messages hit to its task turn', async () => {
    const valid = validFixture.search as Record<string, unknown>;
    const discovery = validFixture.discovery;
    fetchMock.mockImplementation(async (input: string | URL) => {
      const url = new URL(String(input), 'http://localhost');
      if (url.pathname === '/api/v1/search') return Response.json(valid);
      if (url.pathname === '/api/v1/web/discovery') return Response.json(discovery);
      if (url.pathname === '/api/v1/projects/summary') return Response.json(parentVisibleFixture.summary);
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'pipeline' } });

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/search?q=pipeline&limit=20')));
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
      if (url.pathname === '/api/v1/search') return Response.json(valid);
      if (url.pathname === '/api/v1/web/discovery') return Response.json(validFixture.discovery);
      if (url.pathname === '/api/v1/projects/summary') return Response.json(parentVisibleFixture.summary);
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
      if (url.pathname === '/api/v1/search') return Response.json(search);
      if (url.pathname === '/api/v1/web/discovery') return Response.json(discovery);
      if (url.pathname === '/api/v1/projects/summary') return Response.json(parentVisibleFixture.summary);
      throw new Error(`unexpected test request ${url.pathname}`);
    });
    open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'pipeline' } });
    expect(await screen.findByRole('alert')).toHaveTextContent(/discovery/i);
    expect(screen.queryByText(/pipeline/)).not.toBeInTheDocument();
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
