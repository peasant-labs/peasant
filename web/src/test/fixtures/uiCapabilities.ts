import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  parseStrictYAML,
  requireExactFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

/** One GET /api/v1/config/capabilities scenario (src/components/testdata/ui_capabilities.yaml). */
export type CapabilityCase = {
  name: string;
  expectAdvertised: boolean;
  pending?: boolean;
  throw?: boolean;
  status?: number;
  body?: unknown;
};

/** The scenario names the fixture must keep; a deleted row fails the loader. */
const REQUIRED_CASE_NAMES = [
  'loading',
  'fetch-throws',
  'http-500',
  'malformed-body',
  'null-capabilities',
  'empty-object',
  'empty-array',
  'unknown-token-only',
  'unknown-top-level-key-with-real-token',
  'code-map-token',
] as const;

function loadCases(): CapabilityCase[] {
  const path = resolve(process.cwd(), 'src/components/testdata/ui_capabilities.yaml');
  const fixture = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'ui capabilities fixture'), 'ui capabilities fixture');
  requireExactFields(fixture, ['cases'], 'ui capabilities fixture');
  if (!Array.isArray(fixture.cases)) throw new Error('ui capabilities fixture.cases must be a list');
  const rows = fixture.cases.map((row, index) => requireRecord(row, `ui capabilities fixture.cases[${index}]`));
  requireUniqueNames(rows, 'ui capabilities fixture.cases');
  rows.forEach((row, index) => {
    requireExactFields(row, ['name', 'expectAdvertised', 'pending', 'throw', 'status', 'body'], `ui capabilities fixture.cases[${index}]`);
    if (typeof row.expectAdvertised !== 'boolean') throw new Error(`ui capabilities fixture.cases[${index}].expectAdvertised must be a boolean`);
  });
  const names = new Set(rows.map((row) => row.name));
  const missing = REQUIRED_CASE_NAMES.filter((name) => !names.has(name));
  if (missing.length) throw new Error(`ui capabilities fixture is missing required cases: ${missing.join(', ')}`);
  return rows as unknown as CapabilityCase[];
}

export const UI_CAPABILITY_CASES = loadCases();

/** The capabilities endpoint's answer for one scenario, as a fetch mock returns it. */
export function capabilitiesResponse(row: CapabilityCase): Promise<Response> {
  if (row.throw) return Promise.reject(new Error('network down'));
  if (row.pending) return new Promise<Response>(() => {});
  const status = row.status ?? 200;
  if (row.body === undefined) return Promise.resolve(new Response(null, { status }));
  return Promise.resolve(
    new Response(JSON.stringify(row.body), { status, headers: { 'Content-Type': 'application/json' } }),
  );
}
