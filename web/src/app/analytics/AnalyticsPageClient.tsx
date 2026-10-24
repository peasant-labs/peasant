'use client';

import { useMemo } from 'react';
import { ProjectOverview } from '@peasant-labs/fairtrade/analytics';
import { Disconnected, ConnectionStatus } from '@/components/ConnectionState';
import { IngestTeach } from '@/components/IngestTeach';
import { useChannel } from '@/contexts/WebSocketContext';
import { adaptAnalytics } from '@/lib/adapters/analytics';
import { adaptQualitySessions } from '@/lib/quality/types';
import { SkeletonList } from '@/lib/skeleton';
import { subscribe, type QualityPayload } from '@/types/messages';

export function AnalyticsPageClient() {
  const { data: quality, connected, error } = useChannel<QualityPayload>(subscribe.quality());
  const payload = useMemo(
    () => ({ sessions: adaptAnalytics(adaptQualitySessions(quality?.sessions ?? [])) }),
    [quality?.sessions],
  );

  // No quality data has arrived yet. A dropped/failed quality-channel
  // connection at this point would otherwise show an endless loading
  // skeleton — render the app-wide "waiting for the local app" state
  // instead so a real connection failure is visible and actionable.
  // Once quality data HAS arrived, a later disconnect shows a quiet stale-data
  // strip (below) rather than replacing the dashboard — matches the
  // "connection ≠ content" convention used on Home/Map/Review.
  if (quality === undefined) {
    if (!connected || error) {
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
        {(!connected || error) && <ConnectionStatus connected={connected} hasData />}
      </header>
      <ProjectOverview payload={payload} contributorLimit={10} />
    </div>
  );
}
