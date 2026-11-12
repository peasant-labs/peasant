import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  zLocalKickstartCommitRefusal,
  zLocalKickstartCommitted,
  zLocalKickstartErrorResponse,
  zLocalKickstartEvent,
  zLocalKickstartPatchResult,
  zLocalKickstartPreview,
  zLocalKickstartScan,
  zLocalKickstartSession,
  zLocalKickstartStatus,
  zLocalKickstartTree,
} from '@peasant-labs/schema';
import { parseStrictYAML, requireRecord, requireUniqueNames } from '@/test/strictYaml';

/**
 * Loaders for the kickstart fixtures.
 *
 * Every case file is a required-name manifest: its cases must be exactly the
 * names it lists, so a deleted or renamed case fails instead of vanishing.
 * Everything a fixture makes the server say (the named sessions, each scripted
 * route answer, each pushed event) is written as wire JSON and checked against
 * the generated Local API contract before a test sees it, so a fixture cannot
 * describe a server that the contract forbids. The one way out is to say so: an
 * answer or event marked `malformed: true` is a deliberate breach, and is the
 * only thing that is not checked.
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

// ── the contract check of scripted answers and pushed events ─────────────────

interface Decoder {
  safeParse(value: unknown):
    | { success: true }
    | { success: false; error: { issues: readonly { path: PropertyKey[]; message: string }[] } };
}

const KICKSTART_API = '/api/v1/kickstart';
const ANY = '[^/]+';

/**
 * The bodies the kickstart routes serve, each with the schema the generated
 * contract gives it. A route not listed here is not served by the kickstart
 * contract (the import route and the settings routes belong to others), so it
 * is not checked; a route under the kickstart path that is missing here is an
 * error, so a new route cannot go unchecked.
 */
const KICKSTART_REFUSAL_STATUSES: readonly number[] = [400, 404, 500, 503];

/** `*` stands for any method. */
const SERVED: readonly { method: string; path: RegExp; status: number; schema: Decoder }[] = [
  { method: 'GET', path: new RegExp(`^${KICKSTART_API}/status$`), status: 200, schema: zLocalKickstartStatus },
  { method: 'POST', path: new RegExp(`^${KICKSTART_API}/sessions$`), status: 200, schema: zLocalKickstartSession },
  { method: 'GET', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}$`), status: 200, schema: zLocalKickstartSession },
  { method: 'PATCH', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}$`), status: 200, schema: zLocalKickstartPatchResult },
  { method: 'GET', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}/tree$`), status: 200, schema: zLocalKickstartTree },
  { method: 'GET', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}/tree/${ANY}/preview$`), status: 200, schema: zLocalKickstartPreview },
  { method: 'POST', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}/scan$`), status: 200, schema: zLocalKickstartScan },
  { method: 'POST', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}/commit$`), status: 200, schema: zLocalKickstartCommitted },
  { method: 'POST', path: new RegExp(`^${KICKSTART_API}/sessions/${ANY}/commit$`), status: 409, schema: zLocalKickstartCommitRefusal },
  // Every other refusal of a kickstart route is the closed `{error, code}` body.
  ...KICKSTART_REFUSAL_STATUSES.map((status) => ({ method: '*', path: new RegExp(`^${KICKSTART_API}/`), status, schema: zLocalKickstartErrorResponse })),
];

function firstIssue(result: { error: { issues: readonly { path: PropertyKey[]; message: string }[] } }): string {
  const issue = result.error.issues[0];
  return `${issue?.path.map(String).join('.') || '<root>'}: ${issue?.message ?? 'unknown shape'}`;
}

/** A scripted answer of one route, as a fixture writes it. */
export interface ScriptedAnswer {
  status?: number;
  body?: unknown;
  /** A body that is not JSON, sent as written. */
  text?: string;
  /** A deliberate breach of the contract: the one answer that is not checked. */
  malformed?: boolean;
  /** The answer is not sent until a step releases its route; the request is recorded at once. */
  held?: boolean;
}

const ANSWER_KEYS: readonly string[] = ['status', 'body', 'text', 'malformed', 'held'];

/**
 * Refuse a scripted answer that the contract forbids, unless the fixture says
 * it is deliberately malformed, and refuse a `malformed` mark on an answer the
 * contract accepts, so the mark cannot outlive the breach. `route` is
 * `METHOD /path`, as the server's script is keyed.
 */
export function checkScriptedAnswer(route: string, answer: ScriptedAnswer): void {
  const [method, path] = route.split(' ');
  const status = answer.status ?? 200;
  const where = `the scripted answer of ${route} (status ${status})`;
  const unknown = Object.keys(answer).filter((key) => !ANSWER_KEYS.includes(key));
  if (unknown.length > 0) {
    throw new Error(`${where} has ${unknown.map((key) => JSON.stringify(key)).join(', ')}, which a scripted answer does not read; it reads ${ANSWER_KEYS.join(', ')}`);
  }
  const served = SERVED.find((candidate) => (candidate.method === '*' || candidate.method === method) && candidate.path.test(path) && candidate.status === status);
  if (answer.malformed === true) {
    if (served !== undefined && answer.text === undefined && served.schema.safeParse(answer.body).success) {
      throw new Error(`${where} is marked malformed: true, but the Local API contract accepts it; drop the mark`);
    }
    return;
  }
  if (answer.text !== undefined) {
    throw new Error(`${where} is text, not wire JSON; if the page is meant to meet a body the contract forbids, mark the answer malformed: true`);
  }
  if (served !== undefined) {
    const result = served.schema.safeParse(answer.body);
    if (!result.success) {
      throw new Error(`${where} breaks the Local API contract (${firstIssue(result)}); fix the fixture, or mark the answer malformed: true if the breach is the point`);
    }
    return;
  }
  if (status >= 400) {
    const body = answer.body as { error?: unknown } | null | undefined;
    if (body === null || typeof body !== 'object' || typeof body.error !== 'string' || body.error === '') {
      throw new Error(`${where} is a refusal without an error sentence; the server's refusals are {error, code?}`);
    }
    return;
  }
  if (path.startsWith(`${KICKSTART_API}/`) || path === KICKSTART_API) {
    throw new Error(`${where} is a kickstart route and status that test/fixtures/kickstart.ts has no contract schema for; add it to SERVED`);
  }
}

/**
 * Refuse a pushed kickstart topic message that the contract forbids, unless it
 * is deliberately malformed; and refuse the mark on one the contract accepts.
 */
export function checkPushedEvent(data: unknown, malformed: boolean | undefined, where: string): void {
  const result = zLocalKickstartEvent.safeParse(data);
  if (malformed === true) {
    if (result.success) throw new Error(`${where} pushes an event marked malformed: true, but the Local API contract accepts it; drop the mark`);
    return;
  }
  if (!result.success) {
    throw new Error(`${where} pushes an event that breaks the Local API contract (${firstIssue(result)}); fix the fixture, or mark the step malformed: true if the breach is the point`);
  }
}
