import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { zLocalKickstartSession } from '@peasant-labs/schema';
import { parseStrictYAML, requireRecord, requireUniqueNames } from '@/test/strictYaml';

/**
 * Loaders for the kickstart fixtures.
 *
 * Every case file is a required-name manifest: its cases must be exactly the
 * names it lists, so a deleted or renamed case fails instead of vanishing.
 * Server answers are written as wire JSON and checked against the generated
 * Local API contract before a test sees them, so a fixture cannot describe a
 * server that the contract forbids.
 */

// Tests run from web/, the package root.
const WEB = process.cwd();

export function readFixture(path: string): unknown {
  return parseStrictYAML(readFileSync(resolve(WEB, path), 'utf8'), path);
}

export interface FixtureCase {
  name: string;
  [field: string]: unknown;
}

/**
 * The cases of a fixture file, after checking that they are exactly its
 * required names. `section` reads a nested `{requiredNames, cases}` block.
 */
export function loadCases(path: string, section?: string): FixtureCase[] {
  const top = requireRecord(readFixture(path), path);
  const root = section === undefined ? top : requireRecord(top[section], `${path}.${section}`);
  const rows = root.cases;
  if (!Array.isArray(rows)) throw new Error(`${path}.cases must be a list`);
  const cases = rows.map((row, index) => requireRecord(row, `${path}.cases[${index}]`) as FixtureCase);
  requireUniqueNames(cases, `${path}.cases`);
  const required = root.requiredNames;
  if (!Array.isArray(required) || required.some((name) => typeof name !== 'string')) {
    throw new Error(`${path}.requiredNames must be a list of names`);
  }
  const names = cases.map((row) => row.name);
  const missing = required.filter((name) => !names.includes(name));
  const extra = names.filter((name) => !required.includes(name));
  if (missing.length > 0 || extra.length > 0) {
    throw new Error(`${path}: cases must be exactly requiredNames (missing: ${missing.join(', ') || 'none'}; not listed: ${extra.join(', ') || 'none'})`);
  }
  return cases;
}

type Json = unknown;

function need<T>(table: Record<string, T>, name: string, what: string): T {
  const found = table[name];
  if (found === undefined) throw new Error(`fixture names ${what} ${JSON.stringify(name)}, which is not defined`);
  return found;
}

/**
 * Replace `{$model: name}` and `{$tree: name}` markers with the fixtures they
 * name. Other keys beside the marker overwrite the fixture's own, so a case can
 * state one deliberate departure from a served session.
 */
export function expandRefs(value: Json, models: Record<string, Json>, trees: Record<string, Json>): Json {
  if (Array.isArray(value)) return value.map((entry) => expandRefs(entry, models, trees));
  if (value === null || typeof value !== 'object') return value;
  const record = value as Record<string, Json>;
  const marker = typeof record.$model === 'string' ? 'model' : typeof record.$tree === 'string' ? 'tree' : null;
  if (marker !== null) {
    const name = (marker === 'model' ? record.$model : record.$tree) as string;
    const base = structuredClone(marker === 'model' ? need(models, name, 'model') : need(trees, name, 'tree')) as Record<string, Json>;
    for (const [key, entry] of Object.entries(record)) {
      if (key !== '$model' && key !== '$tree') base[key] = expandRefs(entry, models, trees);
    }
    return base;
  }
  return Object.fromEntries(Object.entries(record).map(([key, entry]) => [key, expandRefs(entry, models, trees)]));
}

/** Set one value inside a model by a path of keys: `stops[privacy].fields[redaction-level].value`. */
function setAt(root: Json, path: string, value: Json): void {
  const segments = path.split('.');
  let node: Json = root;
  segments.forEach((segment, index) => {
    const keyed = /^([^[\]]+)\[([^\]]+)\]$/.exec(segment);
    const last = index === segments.length - 1;
    if (keyed) {
      const list = requireRecord(node, path)[keyed[1]];
      if (!Array.isArray(list)) throw new Error(`fixture override ${path}: ${keyed[1]} is not a list`);
      const found = list.find((entry) => requireRecord(entry, path).key === keyed[2]);
      if (found === undefined) throw new Error(`fixture override ${path}: no entry with key ${keyed[2]} in ${keyed[1]}`);
      node = found;
      if (last) throw new Error(`fixture override ${path} must end at a property`);
      return;
    }
    const record = requireRecord(node, path);
    if (last) {
      record[segment] = value;
      return;
    }
    node = record[segment];
  });
}

export interface ModelEntry {
  name: string;
  extends?: string;
  session: Record<string, unknown>;
  set?: Record<string, Json>;
}

/**
 * The named served sessions. A model either states a whole session or extends
 * another by `set` overrides at key paths, and each result is checked against
 * the contract. The returned values are wire JSON.
 */
export function loadModels(path = 'src/test/fixtures/kickstart/models.yaml'): Record<string, Record<string, unknown>> {
  const cases = loadCases(path);
  const built: Record<string, Record<string, unknown>> = {};
  for (const entry of cases) {
    const base = typeof entry.extends === 'string' ? built[entry.extends] : undefined;
    if (typeof entry.extends === 'string' && base === undefined) {
      throw new Error(`${path}: model ${entry.name} extends ${entry.extends}, which is not defined above it`);
    }
    const session = structuredClone(base ?? requireRecord(entry.session, `${path}.${entry.name}.session`));
    for (const [at, value] of Object.entries((entry.set ?? {}) as Record<string, Json>)) setAt(session, at, structuredClone(value));
    const parsed = zLocalKickstartSession.safeParse(session);
    if (!parsed.success) {
      throw new Error(`${path}: model ${entry.name} breaks the Local API contract: ${parsed.error.issues[0]?.path.join('.')} ${parsed.error.issues[0]?.message}`);
    }
    built[entry.name] = session;
  }
  return built;
}

/** The named forests, as the tree route answers them. */
export function loadTrees(path = 'src/test/fixtures/kickstart/trees.yaml'): Record<string, Record<string, unknown>> {
  const cases = loadCases(path);
  return Object.fromEntries(cases.map((entry) => [entry.name, requireRecord(entry.tree, `${path}.${entry.name}.tree`)]));
}
