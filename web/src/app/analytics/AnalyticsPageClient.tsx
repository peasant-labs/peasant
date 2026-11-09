'use client';

import { useMemo } from 'react';
import { ProjectOverview } from '@peasant-labs/fairtrade/analytics';
import { Disconnected, ConnectionStatus } from '@/components/ConnectionState';
import { IngestTeach } from '@/components/IngestTeach';
import { useChannel } from '@/contexts/WebSocketContext';
import { FeedbackPanel } from '@/lib/ft-ui';
import { adaptAnalytics } from '@/lib/adapters/analytics';
import { adaptQualitySessions } from '@/lib/quality/types';
import { SkeletonList } from '@/lib/skeleton';
import { subscribe, type QualityPayload } from '@/types/messages';

export function AnalyticsPageClient() {
  const { data: quality, connected, channelFault } = useChannel<QualityPayload>(subscribe.quality());
  const payload = useMemo(
    () => ({ sessions: adaptAnalytics(adaptQualitySessions(quality?.sessions ?? [])) }),
    [quality?.sessions],
  );

  // No quality data has arrived yet. This channel's own topic error is a data
  // failure the page must state; a provider-wide socket or frame failure is not,
  // and the app-level banner carries it. Either way the page never renders an
  // endless skeleton.
  // Once quality data HAS arrived, a later disconnect shows a quiet stale-data
  // strip (below) rather than replacing the dashboard — matches the
  // "connection ≠ content" convention used on Home/Map/Review.
  if (quality === undefined) {
    if (!connected || channelFault) {
      return (
        <div className="asb-page">
          <Disconnected />
        </div>
      );
    }
    return (
      <div className="asb-page">
        <SkeletonList rows={5} label="Loading analytics" />
      </div>
    );
  }

  // Nothing recorded yet: charts over zero sessions are empty frames, so the
  // page says what is missing instead, like every other empty local page.
  if (payload.sessions.length === 0) {
    return (
      <div className="asb-page animate-fade-up">
        <h1 className="iu-page-title page-title">analytics</h1>
        <IngestTeach />
      </div>
    );
  }

  // The page title is the app's, in the page frame every local page shares;
  // the overview below it draws only its summary line, section toggles and
  // charts, inset to the same left edge.
  return (
    <div className="animate-fade-up">
      <header className="asb-page analytics-head">
        <h1 className="iu-page-title page-title">analytics</h1>
        {channelFault && <FeedbackPanel variant="error">{channelFault.message}</FeedbackPanel>}
        {!channelFault && !connected && <ConnectionStatus connected={connected} hasData />}
      </header>
      <ProjectOverview payload={payload} contributorLimit={10} />
    </div>
  );
}
