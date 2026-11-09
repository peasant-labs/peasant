'use client';

import { useMemo } from 'react';
import { ProjectOverview } from '@peasant-labs/fairtrade/analytics';
import { Disconnected, ConnectionStatus } from '@/components/ConnectionState';
import { IngestTeach } from '@/components/IngestTeach';
import { useChannel } from '@/contexts/WebSocketContext';
import { isRetryableConnectionFault } from '@/lib/faults';
import { Button, FeedbackPanel } from '@/lib/ft-ui';
import { adaptAnalytics } from '@/lib/adapters/analytics';
import { adaptQualitySessions } from '@/lib/quality/types';
import { SkeletonList } from '@/lib/skeleton';
import { subscribe, type QualityPayload } from '@/types/messages';

export function AnalyticsPageClient() {
  const { data: quality, connected, channelFault, connectionFault, refresh } = useChannel<QualityPayload>(subscribe.quality());
  const payload = useMemo(
    () => ({ sessions: adaptAnalytics(adaptQualitySessions(quality?.sessions ?? [])) }),
    [quality?.sessions],
  );

  // No quality data has arrived yet. A stopped app is the offline notice's
  // story. This channel's own topic error is a data failure the page states,
  // with a retry. A frame failure on a live socket is the app-level banner's
  // story, and the page can still ask this channel to read again.
  // Once quality data HAS arrived, a later disconnect shows a quiet stale-data
  // strip (below) rather than replacing the dashboard — matches the
  // "connection ≠ content" convention used on Home/Map/Review.
  if (quality === undefined) {
    if (!connected) {
      return (
        <div className="asb-page">
          <Disconnected />
        </div>
      );
    }
    if (channelFault) {
      return (
        <div className="asb-page">
          <FeedbackPanel variant="error">{channelFault.message}</FeedbackPanel>
          <div className="mt-3">
            <Button variant="secondary" size="sm" onClick={refresh}>try again</Button>
          </div>
        </div>
      );
    }
    return (
      <div className="asb-page">
        <SkeletonList rows={5} label="Loading analytics" />
        {isRetryableConnectionFault(connectionFault) && (
          <div className="mt-3">
            <Button variant="secondary" size="sm" onClick={refresh}>try again</Button>
          </div>
        )}
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
        {channelFault && <div className="flex flex-col items-start gap-3">
          <FeedbackPanel variant="error">{channelFault.message}</FeedbackPanel>
          <Button variant="secondary" size="sm" onClick={refresh}>try again</Button>
        </div>}
        {!connected && <ConnectionStatus connected={connected} hasData />}
      </header>
      <ProjectOverview payload={payload} contributorLimit={10} />
    </div>
  );
}
