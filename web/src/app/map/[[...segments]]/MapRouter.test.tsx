import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MapRouter } from './MapRouter';
import {
  localReviewClarityFixture,
  makeClarityProjectSummaries,
} from '@/test/fixtures/localReviewClarity';

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

  const mapClarityCase = localReviewClarityFixture.pickerCases.find(({ surface }) => surface === 'map');
  if (!mapClarityCase) throw new Error('local review clarity fixture is missing the mounted Map case');

  it(mapClarityCase.name, async () => {
    fetchProjectSummaries.mockResolvedValue({
      projects: makeClarityProjectSummaries(mapClarityCase),
    });
    render(<MapRouter />);

    const search = await screen.findByRole('searchbox', {
      name: localReviewClarityFixture.copy.searchAccessibleName,
    });
    expect(search).toHaveAttribute('placeholder', localReviewClarityFixture.copy.searchPlaceholder);
    const searchIcon = search.closest('.input-ico')?.querySelector('svg');
    expect(searchIcon).toBeInTheDocument();
    expect(searchIcon).toHaveAttribute('aria-hidden', 'true');

    const coverageHelp = screen.getByRole('button', {
      name: localReviewClarityFixture.copy.coverageHelpName,
    });
    expect(coverageHelp).toHaveTextContent(localReviewClarityFixture.copy.coverageVisibleLabel);
    expect(screen.queryByText('Files with AI')).not.toBeInTheDocument();
    fireEvent.focus(coverageHelp);
    const coverageTooltip = screen.getByRole('tooltip');
    expect(coverageTooltip).toHaveTextContent(localReviewClarityFixture.copy.coverageHelpText);
    expect(coverageHelp).toHaveAttribute('aria-describedby', coverageTooltip.id);

    const target = screen.getByRole('link', { name: mapClarityCase.expectedLinkName });
    expect(target).toHaveAttribute('href', mapClarityCase.expectedHref);
    expect(within(target).getByLabelText(mapClarityCase.expectedCoverageLabel)).toBeInTheDocument();

    fireEvent.change(search, { target: { value: mapClarityCase.searchQuery } });
    expect(screen.getAllByRole('link', { name: /Open the map of/ })).toEqual([target]);
    expect(target).toHaveAttribute('href', mapClarityCase.expectedHref);
  });
});
