import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import type { SelectableRedactionLevel } from './redactions';
import { pushRequestBody, runPush } from './push';

const source = readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/push_request.yaml'), 'utf8');
const caseFields = ['name', 'sessionIds', 'redactionLevel', 'expectedBody'] as const;

interface PushRequestCase {
  name: string;
  sessionIds: string[];
  redactionLevel: SelectableRedactionLevel;
  expectedBody: Record<string, unknown>;
}

function loadFixture(yaml: string): { forbiddenKeys: string[]; cases: PushRequestCase[] } {
  const root = requireRecord(parseStrictYAML(yaml, 'push request fixture'), 'push request fixture');
  requireExactRequiredFields(root, ['requiredNames', 'forbiddenKeys', 'cases'], 'push request fixture');
  const requiredNames = root.requiredNames as string[];
  const forbiddenKeys = root.forbiddenKeys as string[];
  if (!Array.isArray(requiredNames) || requiredNames.length === 0) throw new Error('push request fixture requiredNames must list the cases');
  if (!Array.isArray(forbiddenKeys) || forbiddenKeys.length === 0) throw new Error('push request fixture forbiddenKeys must not be empty');
  if (!Array.isArray(root.cases)) throw new Error('push request fixture cases must be a list');
  const cases = root.cases.map((value, index) => requireRecord(value, `push request fixture.cases[${index}]`));
  requireUniqueNames(cases, 'push request fixture.cases');
  cases.forEach((value, index) => requireExactRequiredFields(value, caseFields, `push request fixture.cases[${index}]`));
  const names = cases.map((value) => String(value.name));
  if (names.length !== requiredNames.length || requiredNames.some((name) => !names.includes(name))) {
    throw new Error('push request fixture cases must exactly match requiredNames');
  }
  return { forbiddenKeys, cases: cases as unknown as PushRequestCase[] };
}

const fixture = loadFixture(source);

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('push request fixture contract', () => {
  it('is strict and complete', () => {
    expect(() => loadFixture(source.replace('  - several-sessions\n', ''))).toThrow(/exactly match requiredNames/);
    expect(() => loadFixture(source.replace('forbiddenKeys:', 'unknown: true\nforbiddenKeys:'))).toThrow(/unknown fields/);
    expect(() => loadFixture(`${source}\n---\n{}`)).toThrow(/exactly one YAML document/);
  });
});

describe.each(fixture.cases)('$name', (testCase) => {
  it('builds the typed body with no visibility or license', () => {
    const body = pushRequestBody(testCase.sessionIds, testCase.redactionLevel);
    expect(body).toEqual(testCase.expectedBody);
    for (const key of fixture.forbiddenKeys) expect(body).not.toHaveProperty(key);
  });

  it('sends exactly that body to the push route', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ new: 0, updated: 0, skipped: 0, errors: 0, sessions: [] }), { status: 200 }),
    );
    vi.stubGlobal('fetch', fetchMock);
    await runPush(testCase.sessionIds, testCase.redactionLevel);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toMatch(/\/api\/v1\/sync\/push$/);
    const sent = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(sent).toEqual(testCase.expectedBody);
    for (const key of fixture.forbiddenKeys) expect(sent).not.toHaveProperty(key);
  });
});
