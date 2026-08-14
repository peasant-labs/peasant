import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { groupShareHierarchy } from './group';
import type { ShareHierarchySession } from './types';

interface FixtureRow { id: string; projectName: string; projectHash: string; locationLabel: string; repositoryLocationId: string; branch: string }
interface Fixture { sessions: FixtureRow[] }

const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/hierarchy.yaml'), 'utf8')) as Fixture;

describe('Share hierarchy fixture', () => {
  it('keeps repository locations and branches as separate hierarchy levels', () => {
    expect(fixture.sessions).toHaveLength(7);
    const sessions = fixture.sessions.map((row): ShareHierarchySession => ({
      ...row, provider: 'claude-code', hostSlug: '', startTime: '2026-08-12T00:00:00Z',
      durationMins: 1, totalTokens: 1, turnCount: 1, model: '', shareStatus: 'new', preview: row.id,
    }));
    const hierarchy = groupShareHierarchy(sessions);
    expect(hierarchy).toHaveLength(2);
    expect(hierarchy[0].locations[0].branches.map((branch) => branch.branch)).toEqual(['main', 'feature']);
    expect(hierarchy[0].locations[0].branches[0].sessions.map((session) => session.id)).toEqual(['session-a']);
    expect(hierarchy[0].locations.map((location) => location.locationLabel)).toEqual(['workspace a', 'workspace b', 'repository unavailable']);
    expect(hierarchy[0].locations[2].branches).toHaveLength(1);
    expect(hierarchy[0].locations[2].branches[0].branch).toBe('');
    expect(hierarchy[0].locations[2].branches[0].sessions.map((session) => session.id)).toEqual([
      'unresolved-alpha-unknown-a', 'unresolved-alpha-unknown-b',
    ]);
    expect(hierarchy[1].locations).toHaveLength(1);
    expect(hierarchy[1].locations[0].locationLabel).toBe('repository unavailable');
    expect(hierarchy[1].locations[0].branches.map((branch) => branch.branch)).toEqual(['main', 'release']);
    expect(hierarchy.flatMap((project) => project.locations.flatMap((location) => location.branches.flatMap((branch) => branch.sessions)))).toHaveLength(fixture.sessions.length);
  });
});
