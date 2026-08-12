import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, cleanup, fireEvent, act, waitFor } from '@testing-library/react';
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

const fetchProjectSummaries = vi.fn();
const fetchSearch = vi.fn();
const fetchDiscovery = vi.fn();
const parentVisibleFixture = projectViewerStateFixture('explicit session makes parent visible');
const PROJECT_HASH = parentVisibleFixture.summary.projects[0].projectHash;
vi.mock('@/lib/api/map', () => ({
  fetchProjectSummaries: () => fetchProjectSummaries(),
  fetchSearch: (q: string, limit?: number) => fetchSearch(q, limit),
}));
vi.mock('@/lib/api/discovery', () => ({
  fetchDiscovery: () => fetchDiscovery(),
  requireDiscoveryItem: (discovery: Map<string, unknown>, sessionId: string) => {
    const item = discovery.get(sessionId);
    if (!item) throw new Error('requires exactly one discovery row');
    return item;
  },
}));

beforeEach(() => {
  push.mockClear();
  toggle.mockClear();
  fetchSearch.mockReset();
  fetchDiscovery.mockReset();
  fetchSearch.mockResolvedValue({ query: '', results: [] });
  fetchDiscovery.mockResolvedValue(new Map());
  fetchProjectSummaries.mockResolvedValue(parentVisibleFixture.summary);
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
    const callsBefore = fetchProjectSummaries.mock.calls.length;
    fetchProjectSummaries
      .mockRejectedValueOnce(new Error('database unavailable'))
      .mockResolvedValueOnce({
        projects: [{
          projectHash: PROJECT_HASH,
          project: '/work/alpha-project',
          sessions: 3,
          recordedFiles: 1,
          totalFiles: 2,
          openChanges: 1,
        }],
      });
    open();

    fireEvent.click(await screen.findByRole('button', { name: 'retry project discovery' }));
    expect(await screen.findByText('alpha-project · map')).toBeInTheDocument();
    expect(fetchProjectSummaries.mock.calls.length - callsBefore).toBe(2);
  });

  it('does not search for queries shorter than 2 characters', async () => {
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'a' } });
    // Give the debounce window time to (not) fire.
    await new Promise((r) => setTimeout(r, 250));
    expect(fetchSearch).not.toHaveBeenCalled();
    expect(screen.queryByText('Messages')).not.toBeInTheDocument();
  });

  it('searches transcripts and deep-links a Messages hit to its task turn', async () => {
    fetchSearch.mockResolvedValue({
      query: 'pipeline',
      results: [
        {
          sessionId: 'sess-9',
          project: '/work/alpha-project',
          projectHash: PROJECT_HASH,
          entryIndex: 6,
          role: 'user',
          snippet: 'fix the [pipeline] retry',
          score: 2,
        },
      ],
    });
    fetchDiscovery.mockResolvedValue(new Map([
      ['sess-9', { sessionId: 'sess-9', locationLabel: 'alpha', branch: 'main', selectionStatus: 'unselected' }],
    ]));
    open();
    const input = screen.getByRole('combobox');
    fireEvent.change(input, { target: { value: 'pipeline' } });

    await waitFor(() => expect(fetchSearch).toHaveBeenCalledWith('pipeline', 20));
    const hit = await screen.findByText('fix the [pipeline] retry');
    expect(screen.getByText('Messages')).toBeInTheDocument();
    expect(screen.getByText(/alpha · main · unselected/)).toBeInTheDocument();

    fireEvent.mouseDown(hit);
    expect(push).toHaveBeenCalledWith(
      `/projects/${PROJECT_HASH}/sess-9?turn=6`,
    );
  });

  it('fails closed when a search result has no discovery row', async () => {
    fetchSearch.mockResolvedValue({ query: 'pipeline', results: [{
      sessionId: 'missing', project: '/work/alpha-project', projectHash: PROJECT_HASH,
      entryIndex: 1, role: 'user', snippet: 'pipeline', score: 1,
    }] });
    fetchDiscovery.mockResolvedValue(new Map());
    open();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'pipeline' } });
    expect(await screen.findByRole('alert')).toHaveTextContent(/discovery/i);
    expect(screen.queryByText('pipeline')).not.toBeInTheDocument();
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
