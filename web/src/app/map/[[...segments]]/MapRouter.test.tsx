import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DOCS } from '@/lib/docs';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MapRouter } from './MapRouter';
import { projectViewerStateFixture } from '@/components/picker/projectViewerStateFixtures';
import { localReviewClarityFixture, makeClarityProjectSummaries } from '@/test/fixtures/localReviewClarity';

vi.mock('next/navigation', () => ({
  usePathname: () => '/map',
  useRouter: () => ({ replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));

const fetchProjectSummaries = vi.fn();
vi.mock('@/lib/api/map', () => ({
  cachedProjectSummaries: () => null,
  fetchProjectSummaries: () => fetchProjectSummaries(),
}));

describe('MapRouter project discovery', () => {
  beforeEach(() => fetchProjectSummaries.mockReset());
  afterEach(() => cleanup());

  it('mounts the fixture-backed map picker with review clarity and filtering', async () => {
    const testCase = localReviewClarityFixture.pickerCases.find((row) => row.surface === 'map')!;
    fetchProjectSummaries.mockResolvedValue({ projects: makeClarityProjectSummaries(testCase) });
    render(<MapRouter />);

    const search = await screen.findByRole('searchbox', { name: localReviewClarityFixture.copy.searchAccessibleName });
    expect(search).toHaveClass('input', 'is-input');
    expect(search).toHaveAttribute('placeholder', localReviewClarityFixture.copy.searchPlaceholder);
    expect(search.closest('.input-ico')?.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('button', { name: localReviewClarityFixture.copy.coverageHelpName })).toHaveTextContent(localReviewClarityFixture.copy.coverageVisibleLabel);
    expect(screen.getByRole('link', { name: testCase.expectedLinkName })).toHaveAttribute('href', testCase.expectedHref);

    fireEvent.change(search, { target: { value: 'no matching project' } });
    expect(screen.getByText('no projects match “', { exact: false }).closest('p')).toHaveTextContent('no projects match “no matching project”.');
    fireEvent.change(search, { target: { value: testCase.searchQuery } });
    expect(screen.getByRole('link', { name: testCase.expectedLinkName })).toHaveAttribute('href', testCase.expectedHref);
  });

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

  for (const name of [
    'restrictive-saved-selection-keeps-recorded-projects',
    'empty-saved-selection-keeps-recorded-projects',
    'excluded-saved-selection-keeps-recorded-projects',
    'stale-saved-selection-keeps-recorded-projects',
  ] as const) {
    it(`lists every recorded project: ${name}`, async () => {
      const fixture = projectViewerStateFixture(name);
      fetchProjectSummaries.mockResolvedValue(fixture.summary);
      render(<MapRouter />);

      expect(
        await screen.findByRole('link', {
          name: `Open the map of ${fixture.expectedParentLabel}`,
        }),
      ).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Open the map of control-project' })).toBeInTheDocument();
      expect(screen.queryByText(/hidden by a saved selection/)).not.toBeInTheDocument();
      expect(screen.queryByText(/your saved selection hides all projects/)).not.toBeInTheDocument();
      expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
    });
  }

  it('keeps genuine no-data teaching when there are no recorded projects', async () => {
    const fixture = projectViewerStateFixture('genuine no data');
    fetchProjectSummaries.mockResolvedValue(fixture.summary);
    render(<MapRouter />);

    expect(await screen.findByRole('link', { name: /^how to record sessions/ })).toHaveAttribute('href', DOCS.record);
    expect(screen.queryByText('peasant ingest')).not.toBeInTheDocument();
    expect(screen.queryByText(/hidden by a saved selection/)).not.toBeInTheDocument();
  });
});
