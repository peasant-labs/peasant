import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { changesSummary, compareLoadedTurns, pushRequest, scanVersion } from './publishModel';

type ConsentCase = {
  name: string;
  kind: 'scan' | 'comparison' | 'push';
  revision?: string;
  previousRevision?: string;
  loadedSegments?: number;
  totalSegments?: number;
  publishedAt?: string;
  turns?: { timestamp: string }[];
  expectVersion?: string;
  expectPreviousVersion?: string;
  forbid?: string;
  expectedReason?: string;
  expectedNewTurns?: number;
  expectedSummary?: string;
  sessionId?: string;
  level?: 'standard';
  expectedRequest?: unknown;
};

const fixture = requireRecord(
  parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/publish/testdata/segment-consent.yaml'), 'utf8'), 'segment consent fixture'),
  'segment consent fixture',
) as unknown as { requiredNames: string[]; cases: ConsentCase[] };
requireExactRequiredFields(fixture as unknown as Record<string, unknown>, ['requiredNames', 'cases'], 'segment consent fixture');
requireUniqueNames(fixture.cases as unknown as Record<string, unknown>[], 'segment consent fixture.cases');

describe('segment publish consent', () => {
  it('keeps the required consent cases', () => {
    expect(fixture.cases.map((entry) => entry.name)).toEqual(fixture.requiredNames);
  });

  it.each(fixture.cases)('$name', (entry) => {
    if (entry.kind === 'scan') {
      expect(scanVersion(entry.revision ?? '')).toBe(entry.expectVersion);
      expect(scanVersion(entry.previousRevision ?? '')).toBe(entry.expectPreviousVersion);
      expect(scanVersion(entry.revision ?? '')).not.toBe(String(entry.turns?.length ?? 0));
      return;
    }
    if (entry.kind === 'comparison') {
      const comparison = compareLoadedTurns({
        loadedSegments: entry.loadedSegments ?? 0,
        totalSegments: entry.totalSegments ?? 0,
        turns: entry.turns ?? [],
        publishedAt: entry.publishedAt,
      });
      if (entry.expectedReason) {
        expect(comparison).toEqual({ comparable: false, reason: entry.expectedReason });
        expect(changesSummary(0)).toBe('no new turns since you published');
      } else {
        expect(comparison).toEqual({ comparable: true, newTurns: entry.expectedNewTurns });
        if (comparison.comparable) expect(changesSummary(comparison.newTurns)).toBe(entry.expectedSummary);
      }
      return;
    }
    expect(pushRequest(entry.sessionId ?? '', entry.level ?? 'standard', { add: [], remove: [] }, { listed: [], readers: [] })).toEqual(entry.expectedRequest);
  });
});
