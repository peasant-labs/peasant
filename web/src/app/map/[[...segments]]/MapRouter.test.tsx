import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MapRouter } from './MapRouter';

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
});
