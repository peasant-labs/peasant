import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, within } from '@testing-library/react';
import { Harness } from '@peasant-labs/schema';
import type { SessionSummary } from '@/types/messages';
import type { QualitySession } from '@/lib/quality/types';
import type { LocalSessionListPayload } from '@/lib/api/grouped';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { QualityFixtureName, qualityFixture } from '@/test/fixtures/quality';
import { adaptAnalytics } from '@/lib/adapters/analytics';
import { AllSessions } from '@/components/sessions/AllSessions';
import { GroupedSessionsSection } from '@/components/sessions/GroupedSessionsSection';
import { displayedTurnCount } from './displayedTurnCount';

const DIR = 'src/lib/sessions/testdata';
const read = (name: string) => requireRecord(parseStrictYAML(readFileSync(resolve(process.cwd(), DIR, name), 'utf8'), name), name);
const fixture = read('displayed_turn_count.yaml');
const manifest = read('displayed_turn_count.manifest.yaml');
requireExactRequiredFields(fixture, ['cases'], 'displayed_turn_count.yaml');
requireExactRequiredFields(manifest, ['requiredNames'], 'displayed_turn_count.manifest.yaml');

type CountCase = {
  name: string;
  turnCount: number;
  conversationTurnCount: number | 'absent';
  want: number;
  printed: string;
  notPrinted: string | null;
};
const cases = (fixture.cases as unknown[]).map((row, index) => {
  const record = requireRecord(row, `cases[${index}]`);
  requireExactRequiredFields(record, ['name', 'turnCount', 'conversationTurnCount', 'want', 'printed', 'notPrinted'], `cases[${index}]`);
  return record as unknown as CountCase;
});
requireUniqueNames(cases as unknown as Record<string, unknown>[], 'cases');

/** The two wire counts of a row, with an unmeasured conversation count left off. */
function counts(row: CountCase): { turnCount: number; conversationTurnCount?: number } {
  return row.conversationTurnCount === 'absent'
    ? { turnCount: row.turnCount }
    : { turnCount: row.turnCount, conversationTurnCount: row.conversationTurnCount };
}

const PROJECT_HASH = 'a'.repeat(64);
// Tokens and duration are chosen so no other cell can print a fixture count.
function summary(row: CountCase): SessionSummary {
  return {
    id: 'count-r1',
    harness: Harness.ClaudeCode,
    startTime: '2026-08-12T14:20:00.000Z',
    durationMins: 1,
    toolCallCount: 0,
    totalTokens: 28_600,
    project: 'tmp',
    projectHash: PROJECT_HASH,
    ...counts(row),
  } as SessionSummary;
}

function cellTexts(): string[] {
  const row = screen.getAllByRole('row').find((candidate) => within(candidate).queryAllByRole('cell').length > 0);
  if (!row) throw new Error('the session list rendered no body row');
  return within(row).getAllByRole('cell').map((cell) => cell.textContent?.trim() ?? '');
}

describe('the turn count a session list prints', () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('holds every required fixture row', () => {
    const names = cases.map((row) => row.name);
    expect((manifest.requiredNames as string[]).filter((name) => !names.includes(name))).toEqual([]);
  });

  describe.each(cases.map((row) => [row.name, row] as const))('%s', (_name, row) => {
    it('resolves the displayed count', () => {
      expect(displayedTurnCount(counts(row))).toBe(row.want);
    });

    it('prints it in the flat session list and in the row label', () => {
      render(<AllSessions sessions={[summary(row)]} />);
      const cells = cellTexts();
      expect(cells).toContain(row.printed);
      if (row.notPrinted != null) expect(cells).not.toContain(row.notPrinted);
      expect(screen.getByRole('link', { name: new RegExp(`, ${row.want} turns, `) })).toBeInTheDocument();
    });

    it('prints it in the grouped session list', async () => {
      const payload = {
        page: 1, perPage: 25, total: 1, nextItems: [], revision: '0', ordinarySessionTotal: 1, helperThreadTotal: 0,
        items: [{ kind: 'transcript', transcript: { session: summary(row) }, helperGroups: [] }],
      } as unknown as LocalSessionListPayload;
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true, json: async () => payload } as Response)));
      render(<GroupedSessionsSection variant="sessions" heading="all sessions" />);
      await screen.findByText('count-r1');
      const cells = cellTexts();
      expect(cells).toContain(row.printed);
      if (row.notPrinted != null) expect(cells).not.toContain(row.notPrinted);
    });

    it('feeds it to the analytics record', () => {
      const session = { ...qualityFixture(QualityFixtureName.ResolvedTypical), conversationTurnCount: undefined, ...counts(row) } as QualitySession;
      expect(adaptAnalytics([session])[0]?.turnCount).toBe(row.want);
    });
  });
});
