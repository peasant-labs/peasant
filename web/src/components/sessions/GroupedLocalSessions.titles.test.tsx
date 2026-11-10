import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseStrictYAML, requireExactFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import type { LocalSessionListPayload } from '@/lib/api/grouped';
import { GroupedSessionsSection } from './GroupedSessionsSection';

// The grouped row title comes from the bounded page's stored `title`, not from
// the retired full sessions channel. Each case mounts one row through the
// shared-table adapter and asserts the rendered title, including the preview
// and short-id fallbacks and the search variant that omits the stored title.

const caseFields = ['name', 'sessionId', 'title', 'preview', 'expectedTitle'] as const;

type TitleCase = {
  name: string;
  sessionId: string;
  title?: string;
  preview?: string;
  expectedTitle: string;
};

const source = readFileSync(resolve(process.cwd(), 'src/components/sessions/testdata/grouped_row_titles.yaml'), 'utf8');

function loadCases(): TitleCase[] {
  const root = requireRecord(parseStrictYAML(source, 'grouped row titles'), 'grouped row titles');
  if (!Array.isArray(root.requiredNames) || !Array.isArray(root.cases)) {
    throw new Error('grouped row titles requires requiredNames and cases');
  }
  const rows = root.cases.map((row, index) => requireRecord(row, `grouped row titles.cases[${index}]`));
  requireUniqueNames(rows, 'grouped row titles.cases');
  rows.forEach((row, index) => {
    requireExactFields(row, [...caseFields], `grouped row titles.cases[${index}]`);
    for (const field of ['name', 'sessionId', 'expectedTitle']) {
      if (typeof row[field] !== 'string' || (row[field] as string).length === 0) {
        throw new Error(`grouped row titles.cases[${index}].${field} must be a nonempty string`);
      }
    }
    for (const field of ['title', 'preview']) {
      if (field in row && typeof row[field] !== 'string') {
        throw new Error(`grouped row titles.cases[${index}].${field} must be a string`);
      }
    }
  });
  const required = root.requiredNames as string[];
  const names = rows.map((row) => row.name as string);
  if (required.length !== names.length || required.some((name) => !names.includes(name))) {
    throw new Error('grouped row titles cases do not match their required names');
  }
  return rows as TitleCase[];
}

const cases = loadCases();

function payloadFor(row: TitleCase): LocalSessionListPayload {
  const transcript: Record<string, unknown> = {
    session: {
      id: row.sessionId,
      harness: 'codex',
      startTime: '2026-01-01T00:00:00Z',
      durationMins: 1,
      turnCount: 2,
      totalTokens: 10,
      toolCallCount: 0,
      projectHash: 'a'.repeat(64),
      ...(row.preview === undefined ? {} : { preview: row.preview }),
    },
  };
  if (row.title !== undefined) transcript.title = row.title;
  return {
    page: 1,
    perPage: 25,
    total: 1,
    nextItems: [],
    revision: '0',
    ordinarySessionTotal: 1,
    helperThreadTotal: 0,
    items: [{ kind: 'transcript', transcript, helperGroups: [] }],
  } as unknown as LocalSessionListPayload;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('grouped row titles', () => {
  for (const row of cases) {
    it(row.name, async () => {
      vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true, json: async () => payloadFor(row) } as Response)));
      render(<GroupedSessionsSection variant="sessions" />);
      expect(await screen.findByText(row.expectedTitle)).toBeInTheDocument();
    });
  }
});
