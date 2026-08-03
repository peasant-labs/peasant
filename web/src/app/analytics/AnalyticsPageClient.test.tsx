import { render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AnalyticsOverviewPayload } from '@peasant-labs/fairtrade/analytics';
import {
  QualityFixtureSetName,
  qualityFixtureSet,
} from '@/test/fixtures/quality';
import { AnalyticsPageClient } from './AnalyticsPageClient';

/** The host-facing props this page exercises on the design-system dashboard. */
interface OverviewProps {
  payload: AnalyticsOverviewPayload;
  title?: string;
  contributorLimit?: number;
  sections?: unknown;
}

let channelData: unknown = undefined;
let channelConnected = true;
let channelError: string | null = null;
const overviewProps = vi.hoisted(() => vi.fn());

vi.mock('@/contexts/WebSocketContext', () => ({
  useChannel: () => ({
    data: channelData,
    connected: channelConnected,
    error: channelError,
  }),
}));

vi.mock('@peasant-labs/fairtrade/analytics', () => ({
  ProjectOverview: (props: OverviewProps) => {
    overviewProps(props);
    return <div data-testid="project-overview" />;
  },
}));

describe('AnalyticsPageClient', () => {
  beforeEach(() => {
    overviewProps.mockReset();
    channelData = undefined;
    channelConnected = true;
    channelError = null;
  });

  it('passes adapted quality sessions to the full ProjectOverview via the payload prop', () => {
    const qualitySessions = qualityFixtureSet(QualityFixtureSetName.ProjectMix);
    channelData = { sessions: qualitySessions };

    render(<AnalyticsPageClient />);

    expect(screen.getByTestId('project-overview')).toBeInTheDocument();
    expect(overviewProps).toHaveBeenCalledTimes(1);
    const props = overviewProps.mock.calls[0][0] as OverviewProps;
    expect(props.sections).toBeUndefined();
    expect(props.payload.sessions).toHaveLength(qualitySessions.length);
    expect(props.payload.sessions).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          id: expect.any(String),
          startTime: expect.any(String),
          projectKey: expect.any(String),
          contributorId: expect.any(String),
          durationMins: expect.any(Number),
          totalTokens: expect.any(Number),
          turnCount: expect.any(Number),
          toolCallCount: expect.any(Number),
        }),
      ]),
    );
  });

  it('shows the shared loading skeleton before the quality feed arrives', () => {
    channelData = undefined;

    const { container } = render(<AnalyticsPageClient />);

    expect(container.querySelector('[aria-busy="true"]')).toBeInTheDocument();
    expect(overviewProps).not.toHaveBeenCalled();
  });

  // The route used to drop the quality-channel connection state entirely,
  // so a dropped/failed connection before any data arrived left the loading
  // skeleton spinning forever with no way to tell "still loading" from
  // "never going to load".
  it('renders an explicit disconnected state instead of an endless skeleton when the channel is not connected and no data has arrived', () => {
    channelData = undefined;
    channelConnected = false;

    render(<AnalyticsPageClient />);

    expect(screen.getByText('waiting for the peasant app')).toBeInTheDocument();
    expect(overviewProps).not.toHaveBeenCalled();
  });

  it('renders the disconnected state on a quality-channel error even while nominally connected', () => {
    channelData = undefined;
    channelConnected = true;
    channelError = 'Lost the connection to the Peasant app on this computer.';

    render(<AnalyticsPageClient />);

    expect(screen.getByText('waiting for the peasant app')).toBeInTheDocument();
    expect(overviewProps).not.toHaveBeenCalled();
  });

  it('keeps the dashboard visible with a stale-connection note when the connection drops after data has already loaded', () => {
    const qualitySessions = qualityFixtureSet(QualityFixtureSetName.ProjectMix);
    channelData = { sessions: qualitySessions };
    channelConnected = false;

    render(<AnalyticsPageClient />);

    // Connection lost after data loaded is a quiet strip, not a full
    // teardown of the dashboard — "connection ≠ content" (matches Home/Map/Review).
    expect(screen.getByText('connection lost')).toBeInTheDocument();
    expect(screen.getByTestId('project-overview')).toBeInTheDocument();
    expect(overviewProps).toHaveBeenCalledTimes(1);
  });
});
