import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { PageSize } from '@peasant-labs/schema';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import { HomeGroupBy, ALL_HOME_COLUMNS } from '@/components/home/homeColumns';
import { RootSessionList } from '@/components/home/RootSessionList';
import { AllSessions } from '@/components/sessions/AllSessions';
import type { RootSessionRow } from '@/lib/home/publishState';
import type { useHomeSessions } from '@/hooks/useHomeSessions';

// The Home adapters mount Fairtrade's shared table. This fixture names the
// surfaces and the retired local markers, and the test proves both the rendered
// body and the adapter sources.

vi.mock('@/hooks/useRootSessions', () => ({ usePublicationAudience: () => new Map() }));

type Surface = 'sessions' | 'flat';
interface AdapterCase {
  name: string;
  surface: Surface;
  sharedTable: boolean;
  localTable: boolean;
  marks: string[];
}

const casesSource = readFileSync(resolve(process.cwd(), 'src/app/testdata/home-adapter.yaml'), 'utf8');
const manifestSource = readFileSync(resolve(process.cwd(), 'src/app/testdata/home-adapter.manifest.yaml'), 'utf8');

const manifest = requireRecord(parseStrictYAML(manifestSource, 'home adapter manifest'), 'home adapter manifest');
requireExactRequiredFields(manifest, ['expectedCount', 'requiredNames', 'sources'], 'home adapter manifest');
const root = requireRecord(parseStrictYAML(casesSource, 'home adapter cases'), 'home adapter cases');
requireExactRequiredFields(root, ['requiredNames', 'cases'], 'home adapter cases');
if (!Array.isArray(root.cases)) throw new Error('home adapter cases must be a list');
const requiredNames = manifest.requiredNames as string[];
if (!Array.isArray(manifest.sources) || manifest.sources.length === 0) throw new Error('home adapter manifest must name its source files');

const cases = root.cases.map((value, index) => {
  const where = `home adapter cases[${index}]`;
  const row = requireRecord(value, where);
  requireExactFields(row, ['name', 'surface', 'sharedTable', 'localTable', 'marks'], where);
  if (!['sessions', 'flat'].includes(String(row.surface))) throw new Error(`${where}.surface is invalid`);
  if (typeof row.sharedTable !== 'boolean' || typeof row.localTable !== 'boolean') throw new Error(`${where} must state its table booleans`);
  if (!Array.isArray(row.marks) || row.marks.some((mark) => typeof mark !== 'string' || mark.length === 0)) throw new Error(`${where}.marks is invalid`);
  return row as unknown as AdapterCase;
});
requireUniqueNames(cases as unknown as Record<string, unknown>[], 'home adapter cases');
if (cases.length !== manifest.expectedCount) throw new Error('home adapter manifest expectedCount is wrong');
const names = cases.map((row) => row.name);
if (requiredNames.length !== names.length || requiredNames.some((name) => !names.includes(name))) {
  throw new Error('home adapter case names do not match their required names');
}

function sessionRow(id: string): RootSessionRow {
  return {
    sync: {
      id,
      harness: 'claude-code',
      projectName: 'fixture-project',
      projectHash: 'a'.repeat(64),
      hostSlug: 'fixture-host',
      startTime: '2026-10-15T10:00:00Z',
      durationMs: BigInt(60000),
      totalTokens: 10,
      turnCount: 2,
      model: 'fixture',
      syncStatus: 'new',
      previouslyPushed: false,
    },
    publication: { sessionId: id, state: 'unpublished', autoPublish: false, outsideSelection: false },
  } as unknown as RootSessionRow;
}

function HomeAdapter() {
  const rows = [sessionRow('root-1'), sessionRow('root-2')];
  const home = {
    expanded: new Set<string>(),
    loading: false,
    children: new Map(),
    rootPage: { items: rows, nextItems: [], page: 1, perPage: PageSize.P25, total: rows.length, revision: '0' },
    previousPage: null,
    toggle: () => {},
    retryChild: () => {},
    more: () => {},
  } as unknown as ReturnType<typeof useHomeSessions>;
  return (
    <RootSessionList
      rows={rows}
      titles={new Map()}
      previews={new Map()}
      home={home}
      filter="all"
      page={1}
      pageSize={PageSize.P25}
      total={rows.length}
      viewState={{ groupBy: HomeGroupBy.Project, visibleColumns: ALL_HOME_COLUMNS, collapsed: [] }}
      onViewStateChange={() => {}}
      onPageChange={() => {}}
      onPageSizeChange={() => {}}
    />
  );
}

afterEach(cleanup);

describe('home adapters mount the shared table', () => {
  for (const testCase of cases) {
    it(testCase.name, () => {
      const { container } = render(testCase.surface === 'flat'
        ? <AllSessions sessions={[{ id: 'flat-1', harness: 'claude-code', startTime: '2026-10-15T10:00:00Z', durationMins: 1, toolCallCount: 0, totalTokens: 10, turnCount: 2, project: 'fixture-project', projectHash: 'a'.repeat(64) } as never]} />
        : <HomeAdapter />);
      if (testCase.sharedTable) expect(container.querySelector('.libt-table')).not.toBeNull();
      if (!testCase.localTable) expect(container.querySelector('.home-table')).toBeNull();
      for (const mark of testCase.marks) {
        expect(container.querySelector(`.${mark}`), mark).toBeNull();
      }
    });
  }

  it('the adapter sources carry no retired local table marker', () => {
    const marks = new Set(cases.flatMap((testCase) => testCase.marks));
    for (const source of manifest.sources as string[]) {
      const text = readFileSync(resolve(process.cwd(), source), 'utf8');
      for (const mark of marks) {
        expect(text.includes(mark), `${source} still names ${mark}`).toBe(false);
      }
    }
  });
});
