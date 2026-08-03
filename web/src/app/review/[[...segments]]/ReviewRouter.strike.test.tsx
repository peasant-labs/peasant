import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  reviewHref,
  RouteOrigin,
  returnLocation,
  transcriptHref,
} from '@/lib/navigation/projectRoutes';
import { strikeMountedWebFixture } from '@/test/fixtures/strikeMountedWeb';
import { ReviewRouter } from './ReviewRouter';

const routerPush = vi.hoisted(() => vi.fn());
const routerReplace = vi.hoisted(() => vi.fn());
const fetchProjectResolution = vi.hoisted(() => vi.fn());
const fetchReviewChanges = vi.hoisted(() => vi.fn());
const fetchChangeDetail = vi.hoisted(() => vi.fn());
const fetchChangeDiff = vi.hoisted(() => vi.fn());

vi.mock('next/navigation', () => ({
  usePathname: () => `/review/${strikeMountedWebFixture.projectHash}`,
  useSearchParams: () => new URLSearchParams(),
  useRouter: () => ({ push: routerPush, replace: routerReplace }),
}));

vi.mock('@/lib/api/map', () => ({
  fetchProjectResolution: (...args: unknown[]) => fetchProjectResolution(...args),
  fetchReviewChanges: (...args: unknown[]) => fetchReviewChanges(...args),
  fetchChangeDetail: (...args: unknown[]) => fetchChangeDetail(...args),
  fetchChangeDiff: (...args: unknown[]) => fetchChangeDiff(...args),
}));

describe('mounted Review route with a Strike session', () => {
  beforeEach(() => {
    routerPush.mockReset();
    routerReplace.mockReset();
    fetchProjectResolution.mockReset().mockResolvedValue({
      project: strikeMountedWebFixture.projectName,
      projectHash: strikeMountedWebFixture.projectHash,
    });
    fetchReviewChanges.mockReset().mockResolvedValue(strikeMountedWebFixture.reviewList);
    fetchChangeDetail.mockReset();
    fetchChangeDiff.mockReset();
  });

  afterEach(() => cleanup());

  it('renders canonical Strike identity in Changes and navigates to its transcript', async () => {
    const user = userEvent.setup();
    const view = render(<ReviewRouter />);
    const sessionAction = await screen.findByRole('button', {
      name: new RegExp(strikeMountedWebFixture.expected.reviewSessionTitle, 'i'),
    });

    const providerName = sessionAction.querySelector('.pv-name');
    expect(providerName).toHaveTextContent(strikeMountedWebFixture.sessionDetail.harness);
    expect(providerName?.querySelector('.brand')).toBeInTheDocument();
    expect(view.container.querySelector('.gmp-changes-root')).toBeInTheDocument();
    expect(screen.getAllByText(strikeMountedWebFixture.reviewList.changes[0].branch).length).toBeGreaterThan(0);

    await user.click(sessionAction);
    const reviewReturn = returnLocation(reviewHref(strikeMountedWebFixture.projectHash));
    if (!reviewReturn) throw new Error('mounted Strike Review route must produce a valid return location');
    await waitFor(() => expect(routerPush).toHaveBeenCalledWith(
      transcriptHref(
        strikeMountedWebFixture.projectHash,
        strikeMountedWebFixture.sessionDetail.id,
        { origin: RouteOrigin.Review, returnLocation: reviewReturn },
      ),
    ));
    expect(within(sessionAction).getByText(strikeMountedWebFixture.expected.reviewSessionTitle)).toBeInTheDocument();
  });
});
