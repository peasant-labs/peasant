import {
  AllEvidenceKinds,
  AllPublicSourceAnchorKinds,
  AllRelationshipTargetStates,
  AllSessionRelationshipKinds,
} from '@peasant-labs/schema';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from './strictYaml';

/**
 * Strict loader for the mounted context/starter navigation fixture.
 *
 * The fixture carries the durable relationship sets and the authorized read
 * navigation the mounted route must turn into rows, plus the host's route
 * decision over the closed relationship-navigation status set. The loader
 * fails closed on unknown fields, names that drift from their independent
 * required-name manifest, values outside their published closed sets, and rows
 * that contradict themselves, so a case can never be silently dropped or
 * re-interpreted.
 */

const CASE_FIELDS = ['name', 'relationships', 'navigation', 'expected', 'search', 'earlier_turn_content'] as const;
const CASE_REQUIRED = ['name', 'relationships', 'navigation', 'expected'] as const;
const MANIFEST_FIELDS = [
  'expectedCaseCount',
  'requiredNames',
  'expectedLinkCaseCount',
  'requiredLinkNames',
  'requiredStatuses',
  'backCase',
  'expectedLoaderMutationCount',
  'loaderMutations',
  'expectedMutationCount',
  'mutations',
] as const;
const MUTATION_FIELDS = ['name', 'target', 'find', 'replace', 'expectedError'] as const;
const PRODUCTION_MUTATION_FIELDS = ['name', 'find', 'replace', 'expectedTestFile', 'expectedFailedTestNames', 'expectedFailurePattern'] as const;
const ROW_FIELDS = ['label', 'link', 'status', 'note'] as const;
const RELATIONSHIP_FIELDS = ['kind', 'targetState', 'targetLocalId', 'evidence', 'anchor'] as const;
const RELATIONSHIP_REQUIRED = ['kind', 'targetState', 'evidence'] as const;
const NAVIGATION_FIELDS = ['kind', 'status', 'localId'] as const;
const ANCHOR_FIELDS = ['kind'] as const;
const LINK_CASE_FIELDS = ['name', 'kind', 'status', 'localId', 'expectedTarget'] as const;

/** The row labels Fairtrade's one adapter cooks from the two relationship kinds. */
const ROW_LABELS = [
  'context inherited from',
  'started by',
  'context inherited from and started by',
] as const;

/** The honest non-linkable statuses the adapter renders instead of a control. */
const ROW_STATUSES = [
  'source unavailable',
  'unknown source',
  'source inaccessible',
  'conflicting source evidence',
] as const;

export interface ContextNavigationRow {
  label: string;
  link: string | null;
  status: string | null;
  note: string | null;
}

export interface ContextNavigationCase {
  name: string;
  search: string | null;
  earlierTurnContent: string | null;
  relationships: Record<string, unknown>[];
  navigation: Record<string, unknown>[];
  expectedRows: ContextNavigationRow[];
}

export interface ContextLinkCase {
  name: string;
  kind: string;
  status: string;
  localId: string | null;
  expectedTarget: string | null;
}

export interface LoaderMutation {
  name: string;
  target: 'cases' | 'manifest';
  find: string;
  replace: string;
  expectedError: string;
}

/**
 * One executable production mutation for this navigation surface. The
 * `transcript-position.mutations.mjs` runner applies it in memory and requires
 * exactly `expectedFailedTestNames` to fail, so the inventory is part of the
 * fixture contract rather than an adjacent list that can drift from it.
 */
export interface ProductionMutation {
  name: string;
  find: string;
  replace: string;
  expectedTestFile: string;
  expectedFailedTestNames: string[];
  expectedFailurePattern: string;
}

export interface ContextNavigationFixture {
  backCase: ContextNavigationCase;
  cases: ContextNavigationCase[];
  linkCases: ContextLinkCase[];
  requiredStatuses: string[];
  mutations: LoaderMutation[];
  productionMutations: ProductionMutation[];
}

/** Reject a path that omits a required field. Optional fields are declared in the allowed set. */
function requirePresent(value: Record<string, unknown>, fields: readonly string[], path: string): void {
  const missing = fields.filter((key) => !(key in value));
  if (missing.length > 0) throw new Error(`${path} is missing required fields: ${missing.join(', ')}`);
}

function requireString(value: unknown, path: string): string {
  if (typeof value !== 'string' || value.length === 0) throw new Error(`${path} must be a nonempty string`);
  return value;
}

function requireNullableString(value: unknown, path: string): string | null {
  if (value === null || value === undefined) return null;
  return requireString(value, path);
}

function requireMembership(value: unknown, allowed: readonly string[], path: string): string {
  const member = requireString(value, path);
  if (!allowed.includes(member)) {
    throw new Error(`${path} value ${JSON.stringify(member)} is outside its closed set: ${allowed.join(', ')}`);
  }
  return member;
}

function requireStringArray(value: unknown, path: string): string[] {
  if (!Array.isArray(value)) throw new Error(`${path} must be an array`);
  return value.map((entry, index) => requireString(entry, `${path}[${index}]`));
}

function requireRecords(value: unknown, path: string): Record<string, unknown>[] {
  if (!Array.isArray(value)) throw new Error(`${path} must be an array`);
  return value.map((entry, index) => requireRecord(entry, `${path}[${index}]`));
}

function requireCount(value: unknown, path: string): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${path} must be a non-negative safe integer`);
  }
  return value;
}

/** A producer-emitted relationship, validated against the published closed sets. */
function loadRelationship(value: Record<string, unknown>, path: string): Record<string, unknown> {
  requireExactFields(value, RELATIONSHIP_FIELDS, path);
  requirePresent(value, RELATIONSHIP_REQUIRED, path);
  requireMembership(value.kind, AllSessionRelationshipKinds, `${path}.kind`);
  const targetState = requireMembership(value.targetState, AllRelationshipTargetStates, `${path}.targetState`);
  requireMembership(value.evidence, AllEvidenceKinds, `${path}.evidence`);
  const known = targetState === 'target_known' || targetState === 'target_known_retained';
  const target = requireNullableString(value.targetLocalId, `${path}.targetLocalId`);
  if (known !== (target !== null)) {
    throw new Error(`${path} target state and targetLocalId presence disagree: known states require one identifier and every other state forbids it`);
  }
  if (value.anchor !== undefined) {
    const anchor = requireRecord(value.anchor, `${path}.anchor`);
    requireExactFields(anchor, ANCHOR_FIELDS, `${path}.anchor`);
    requireExactRequiredFields(anchor, ANCHOR_FIELDS, `${path}.anchor`);
    requireMembership(anchor.kind, AllPublicSourceAnchorKinds, `${path}.anchor.kind`);
  }
  return value;
}

/** One authorized read-navigation entry, as the local read emits it. */
function loadNavigation(value: Record<string, unknown>, path: string): Record<string, unknown> {
  requireExactFields(value, NAVIGATION_FIELDS, path);
  // A non-linkable read omits the identifier on the wire; a linkable one must
  // carry exactly one, which loadNavigation enforces below.
  requirePresent(value, ['kind', 'status'], path);
  requireMembership(value.kind, AllSessionRelationshipKinds, `${path}.kind`);
  const status = requireString(value.status, `${path}.status`);
  const localId = requireNullableString(value.localId, `${path}.localId`);
  const linkable = status === 'resolved' || status === 'general_link_only';
  if (linkable !== (localId !== null)) {
    throw new Error(`${path} must carry exactly one identifier when its status is linkable and none otherwise`);
  }
  return value;
}

function loadRow(value: Record<string, unknown>, path: string): ContextNavigationRow {
  requireExactFields(value, ROW_FIELDS, path);
  requireExactRequiredFields(value, ROW_FIELDS, path);
  const label = requireMembership(value.label, ROW_LABELS, `${path}.label`);
  const link = requireNullableString(value.link, `${path}.link`);
  const status = requireNullableString(value.status, `${path}.status`);
  const note = requireNullableString(value.note, `${path}.note`);
  if (link !== null && status !== null) {
    throw new Error(`${path} must render either a link or a status, never both: the mounted viewer shows the control it can follow and the status text otherwise`);
  }
  if (link === null) {
    if (status === null) throw new Error(`${path} must render a link or an honest status`);
    requireMembership(status, ROW_STATUSES, `${path}.status`);
    if (note !== null) throw new Error(`${path} renders no note without a link`);
  }
  return { label, link, status, note };
}

function loadCase(value: Record<string, unknown>, path: string): ContextNavigationCase {
  requireExactFields(value, CASE_FIELDS, path);
  requirePresent(value, CASE_REQUIRED, path);
  const relationshipPath = `${path}.relationships`;
  return {
    name: requireString(value.name, `${path}.name`),
    search: requireNullableString(value.search, `${path}.search`),
    earlierTurnContent: requireNullableString(value.earlier_turn_content, `${path}.earlier_turn_content`),
    relationships: requireRecords(value.relationships, relationshipPath).map((row, index) =>
      loadRelationship(row, `${relationshipPath}[${index}]`),
    ),
    navigation: requireRecords(value.navigation, `${path}.navigation`).map((row, index) =>
      loadNavigation(row, `${path}.navigation[${index}]`),
    ),
    expectedRows: requireRecords(requireRecord(value.expected, `${path}.expected`).rows, `${path}.expected.rows`).map(
      (row, index) => loadRow(row, `${path}.expected.rows[${index}]`),
    ),
  };
}

function loadLinkCase(value: Record<string, unknown>, path: string): ContextLinkCase {
  requireExactFields(value, LINK_CASE_FIELDS, path);
  requireExactRequiredFields(value, LINK_CASE_FIELDS, path);
  return {
    name: requireString(value.name, `${path}.name`),
    kind: requireString(value.kind, `${path}.kind`),
    status: requireString(value.status, `${path}.status`),
    localId: requireNullableString(value.localId, `${path}.localId`),
    expectedTarget: requireNullableString(value.expectedTarget, `${path}.expectedTarget`),
  };
}

function loadMutations(value: unknown, path: string): LoaderMutation[] {
  return requireRecords(value, path).map((row, index) => {
    const rowPath = `${path}[${index}]`;
    requireExactFields(row, MUTATION_FIELDS, rowPath);
    requireExactRequiredFields(row, MUTATION_FIELDS, rowPath);
    const target = requireMembership(row.target, ['cases', 'manifest'], `${rowPath}.target`);
    return {
      name: requireString(row.name, `${rowPath}.name`),
      target: target as LoaderMutation['target'],
      find: requireString(row.find, `${rowPath}.find`),
      replace: typeof row.replace === 'string' ? row.replace : (() => {
        throw new Error(`${rowPath}.replace must be a string`);
      })(),
      expectedError: requireString(row.expectedError, `${rowPath}.expectedError`),
    };
  });
}

function loadProductionMutations(value: unknown, path: string): ProductionMutation[] {
  return requireRecords(value, path).map((row, index) => {
    const rowPath = `${path}[${index}]`;
    requireExactFields(row, PRODUCTION_MUTATION_FIELDS, rowPath);
    requireExactRequiredFields(row, PRODUCTION_MUTATION_FIELDS, rowPath);
    const names = row.expectedFailedTestNames;
    if (!Array.isArray(names) || names.length === 0) {
      throw new Error(`${rowPath}.expectedFailedTestNames must be a nonempty array`);
    }
    return {
      name: requireString(row.name, `${rowPath}.name`),
      find: requireString(row.find, `${rowPath}.find`),
      replace: typeof row.replace === 'string' ? row.replace : (() => {
        throw new Error(`${rowPath}.replace must be a string`);
      })(),
      expectedTestFile: requireString(row.expectedTestFile, `${rowPath}.expectedTestFile`),
      expectedFailedTestNames: names.map((name, nameIndex) => requireString(name, `${rowPath}.expectedFailedTestNames[${nameIndex}]`)),
      expectedFailurePattern: requireString(row.expectedFailurePattern, `${rowPath}.expectedFailurePattern`),
    };
  });
}

export function replaceExactlyOnce(source: string, find: string, replacement: string, label: string): string {
  const occurrences = source.split(find).length - 1;
  if (occurrences !== 1) throw new Error(`${label} mutation anchor must occur exactly once, found ${occurrences}`);
  return source.replace(find, replacement);
}

export function loadContextNavigationFixture(
  source: string,
  manifestSource: string,
): ContextNavigationFixture {
  const manifest = requireRecord(parseStrictYAML(manifestSource, 'context navigation manifest'), 'context navigation manifest');
  requireExactFields(manifest, MANIFEST_FIELDS, 'context navigation manifest');
  requireExactRequiredFields(manifest, MANIFEST_FIELDS, 'context navigation manifest');

  const casesRoot = requireRecord(parseStrictYAML(source, 'context navigation cases'), 'context navigation cases');
  requireExactFields(casesRoot, ['back_case', 'cases', 'link_cases'], 'context navigation cases');
  requireExactRequiredFields(casesRoot, ['back_case', 'cases', 'link_cases'], 'context navigation cases');

  const caseRecords = requireRecords(casesRoot.cases, 'context navigation cases.cases');
  requireUniqueNames(caseRecords, 'context navigation cases.cases');
  const cases = caseRecords.map((row, index) => loadCase(row, `context navigation cases.cases[${index}]`));

  const requiredNames = requireStringArray(manifest.requiredNames, 'context navigation manifest.requiredNames');
  const expectedCaseCount = requireCount(manifest.expectedCaseCount, 'context navigation manifest.expectedCaseCount');
  if (new Set(requiredNames).size !== requiredNames.length) throw new Error('context navigation manifest.requiredNames must be unique');
  const caseNames = cases.map((entry) => entry.name);
  if (
    expectedCaseCount !== requiredNames.length ||
    cases.length !== requiredNames.length ||
    requiredNames.some((name) => !caseNames.includes(name)) ||
    caseNames.some((name) => !requiredNames.includes(name))
  ) {
    throw new Error('context navigation fixture cases and their independent required-name manifest require exact set equality and count');
  }

  const linkRecords = requireRecords(casesRoot.link_cases, 'context navigation cases.link_cases');
  requireUniqueNames(linkRecords, 'context navigation cases.link_cases');
  const linkCases = linkRecords.map((row, index) => loadLinkCase(row, `context navigation cases.link_cases[${index}]`));
  const requiredLinkNames = requireStringArray(manifest.requiredLinkNames, 'context navigation manifest.requiredLinkNames');
  const expectedLinkCaseCount = requireCount(manifest.expectedLinkCaseCount, 'context navigation manifest.expectedLinkCaseCount');
  const linkNames = linkCases.map((entry) => entry.name);
  if (
    expectedLinkCaseCount !== requiredLinkNames.length ||
    linkCases.length !== requiredLinkNames.length ||
    requiredLinkNames.some((name) => !linkNames.includes(name)) ||
    linkNames.some((name) => !requiredLinkNames.includes(name))
  ) {
    throw new Error('context navigation link cases and their independent required-name manifest require exact set equality and count');
  }

  const backCaseName = requireString(manifest.backCase, 'context navigation manifest.backCase');
  const backCase = cases.find((entry) => entry.name === backCaseName);
  if (!backCase) throw new Error(`context navigation manifest.backCase ${JSON.stringify(backCaseName)} names no fixture case`);
  if (backCase.earlierTurnContent === null) {
    throw new Error(`context navigation back_case ${JSON.stringify(backCaseName)} must declare earlier_turn_content: the Back scenario restores an expanded earlier-history disclosure`);
  }

  const mutations = loadMutations(manifest.loaderMutations, 'context navigation manifest.loaderMutations');
  const expectedMutationCount = requireCount(manifest.expectedLoaderMutationCount, 'context navigation manifest.expectedLoaderMutationCount');
  if (mutations.length !== expectedMutationCount || new Set(mutations.map((entry) => entry.name)).size !== mutations.length) {
    throw new Error('context navigation manifest loader-mutation inventory count or names are invalid');
  }

  const productionMutations = loadProductionMutations(manifest.mutations, 'context navigation manifest.mutations');
  const expectedProductionMutationCount = requireCount(manifest.expectedMutationCount, 'context navigation manifest.expectedMutationCount');
  if (productionMutations.length !== expectedProductionMutationCount) {
    throw new Error('context navigation manifest production-mutation inventory count is invalid');
  }
  const mutationNames = [...mutations.map((entry) => entry.name), ...productionMutations.map((entry) => entry.name)];
  if (new Set(mutationNames).size !== mutationNames.length) {
    throw new Error('context navigation manifest mutation names must be unique');
  }

  return {
    backCase,
    cases,
    linkCases,
    requiredStatuses: requireStringArray(manifest.requiredStatuses, 'context navigation manifest.requiredStatuses'),
    mutations,
    productionMutations,
  };
}
