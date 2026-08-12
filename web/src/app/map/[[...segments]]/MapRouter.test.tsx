import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MapRouter } from './MapRouter';
import { projectViewerStateFixture } from '@/components/picker/projectViewerStateFixtures';

vi.mock('next/navigation', () => ({
  usePathname: () => '/map',
  useRouter: () => ({ replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock('@/components/Explainer', () => ({
  Explainer: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  ExplainerToggle: () => null,
  useExplainer: () => ({ open: false }),
}));

const fetchProjectSummaries = vi.fn();
vi.mock('@/lib/api/map', () => ({
  cachedProjectSummaries: () => null,
  fetchProjectSummaries: () => fetchProjectSummaries(),
}));

describe('MapRouter project discovery', () => {
  beforeEach(() => fetchProjectSummaries.mockReset());
  afterEach(() => cleanup());

  it('retries on the same map surface and repopulates the project picker', async () => {
    fetchProjectSummaries
      .mockRejectedValueOnce(new Error('database unavailable'))
      .mockResolvedValueOnce({
        projects: [{
          projectHash: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
          project: '/work/alpha-project',
          sessions: 3,
          recordedFiles: 1,
          totalFiles: 2,
          openChanges: 1,
        }],
      });
    render(<MapRouter />);

    fireEvent.click(await screen.findByRole('button', { name: 'retry project discovery' }));
    expect(await screen.findByRole('link', { name: 'Open the map of alpha-project' })).toBeInTheDocument();
    expect(fetchProjectSummaries).toHaveBeenCalledTimes(2);
  });

  it('shows the shared recovery panel instead of first-use teaching when selection hides all data', async () => {
    const fixture = projectViewerStateFixture('all hidden by saved selection');
    fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<MapRouter />);

    const panel = await screen.findByRole('status', { name: 'project selection recovery' });
    expect(panel).toHaveTextContent('Peasant hides 2 projects and 5 sessions.');
    expect(panel).toHaveTextContent('The web viewer does not list it.');
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
    for (const identity of fixture.forbiddenIdentities) {
      expect(document.body.textContent).not.toContain(identity);
    }
  });

  it('keeps genuine no-data teaching when there is no hidden selection', async () => {
    const fixture = projectViewerStateFixture('genuine no data');
    fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<MapRouter />);

    expect(await screen.findByText('peasant ingest')).toBeInTheDocument();
    expect(screen.queryByRole('status', { name: 'project selection recovery' })).not.toBeInTheDocument();
  });

  it('renders an explicit session parent from the shared project summary without recovery guidance', async () => {
    const fixture = projectViewerStateFixture('explicit session makes parent visible');
    fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<MapRouter />);

    expect(
      await screen.findByRole('link', {
        name: `Open the map of ${fixture.expectedParentLabel}`,
      }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('status', { name: 'project selection recovery' })).not.toBeInTheDocument();
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
  });
});
