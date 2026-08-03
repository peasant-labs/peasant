import { describe, it, expect } from 'vitest';
import { buildChangeRecap } from './changeRecap';
import { CHANGE_DETAIL_PAYLOAD } from './test-fixtures';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';

describe('buildChangeRecap', () => {
  it('leads with the branch heading and a files+churn line with where it landed', () => {
    const md = buildChangeRecap(CHANGE_DETAIL_PAYLOAD);
    expect(md.startsWith(`## ${CHANGE_DETAIL_PAYLOAD.branch}\n`)).toBe(true);
    expect(md).toContain(
      `${CHANGE_DETAIL_PAYLOAD.files.length} files changed (+${CHANGE_DETAIL_PAYLOAD.linesAdded} / -${CHANGE_DETAIL_PAYLOAD.linesRemoved})`,
    );
    // "where" names the top directory groups with file counts.
    expect(md).toMatch(/across .+\(\d+\)/);
  });

  it('summarizes the recorded work (conversations + requests)', () => {
    const md = buildChangeRecap(CHANGE_DETAIL_PAYLOAD);
    expect(md).toMatch(/Recorded work: \d+ conversations?, \d+ requests?\./);
  });

  it('lists recurring friction clusters when present, neutrally', () => {
    const payload: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      frictions: [
        { kind: 'retryLoop', label: 'retry loops', file: 'internal/api/server.go', count: 3, sessions: 2 },
        { kind: 'retryLoop', label: 'retry loops', file: 'internal/x.go', count: 1, sessions: 1 },
      ],
    };
    const md = buildChangeRecap(payload);
    expect(md).toContain('Recurring friction:');
    expect(md).toContain('- internal/api/server.go · retry loops 3 times across 2 conversations');
    expect(md).toContain('- internal/x.go · retry loops 1 time across 1 conversation');
  });

  it('omits the friction section when there is none', () => {
    const md = buildChangeRecap({ ...CHANGE_DETAIL_PAYLOAD, frictions: [] });
    expect(md).not.toContain('Recurring friction');
  });

  it('states "No file changes." for an empty change', () => {
    const md = buildChangeRecap({
      ...CHANGE_DETAIL_PAYLOAD,
      files: [],
      linesAdded: 0,
      linesRemoved: 0,
    });
    expect(md).toContain('No file changes.');
  });

  it('is ASCII-only and deterministic (portable in any PR box)', () => {
    const md = buildChangeRecap(CHANGE_DETAIL_PAYLOAD);
    // eslint-disable-next-line no-control-regex
    expect(/^[\x00-\x7F]*$/.test(md)).toBe(true);
    expect(buildChangeRecap(CHANGE_DETAIL_PAYLOAD)).toBe(md);
  });

  it('omits churn parens when there are no line stats', () => {
    const md = buildChangeRecap({
      ...CHANGE_DETAIL_PAYLOAD,
      linesAdded: 0,
      linesRemoved: 0,
    });
    expect(md).not.toContain('(+0');
  });
});
