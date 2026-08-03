import { describe, expect, it } from 'vitest';
import { computeProjectAnalytics } from '@peasant-labs/fairtrade/analytics';
import {
  QualityFixtureName,
  QualityFixtureSetName,
  qualityFixture,
  qualityFixtureSet,
} from '@/test/fixtures/quality';
import {
  ANALYTICS_PLACEHOLDER_COMMIT_COUNT,
  ANALYTICS_LOCAL_USER_ID,
  adaptAnalytics,
  contributorFields,
} from './analytics';

describe('adaptAnalytics', () => {
  it('maps quality sessions into analytics session records', () => {
    const fixture = qualityFixture(QualityFixtureName.ResolvedTypical);
    const [record] = adaptAnalytics([fixture]);

    // Exact equality (not a subset match): the record contract belongs to the
    // design system — an accidentally passed-through wire field would be a
    // contract leak, so extra keys must fail here.
    expect(record).toEqual({
      id: fixture.id,
      startTime: expectedStartTime(fixture.date),
      projectKey: fixture.project,
      contributorId: ANALYTICS_LOCAL_USER_ID,
      hasCommit: false,
      commitCount: ANALYTICS_PLACEHOLDER_COMMIT_COUNT,
      durationMins: fixture.durationMinutes,
      totalTokens: fixture.totalTokens,
      turnCount: fixture.turnCount,
      toolCallCount: fixture.toolCalls,
      outcome: fixture.outcome,
    });
  });

  it('keeps contributor and commit placeholders behind one swap point', () => {
    expect(
      contributorFields(qualityFixture(QualityFixtureName.ResolvedTypical)),
    ).toEqual({
      contributorId: ANALYTICS_LOCAL_USER_ID,
      hasCommit: false,
      commitCount: ANALYTICS_PLACEHOLDER_COMMIT_COUNT,
    });
  });

  it('passes full ProjectOverview metric computation without carved data', () => {
    const sessions = adaptAnalytics(
      qualityFixtureSet(QualityFixtureSetName.ProjectMix),
    );

    const analytics = computeProjectAnalytics(sessions);

    expect(analytics.totalSessions).toBe(2);
    expect(analytics.totalProjects).toBe(2);
    expect(analytics.totalContributors).toBe(1);
    expect(analytics.sessionsPerWeek.length).toBeGreaterThan(0);
    expect(analytics.weeklyActiveContributors.length).toBeGreaterThan(0);
    expect(analytics.newContributorVelocity.length).toBeGreaterThan(0);
    expect(analytics.perContributorBreakdown).toHaveLength(1);
    expect(analytics.sessionToCommitRate).toEqual({
      total: 2,
      withCommit: 0,
      rate: 0,
    });
  });

  it('handles an empty quality payload', () => {
    expect(adaptAnalytics()).toEqual([]);
  });
});

function expectedStartTime(date: string): string {
  return date.includes('T') ? date : `${date}T00:00:00Z`;
}
