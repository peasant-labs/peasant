import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { zLocalSettingsResponse, type AutoPublishRule, type LocalSettingsResponse } from '@peasant-labs/schema';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

/**
 * Loaders for the settings page fixtures. Each file is a required-name
 * manifest: its cases must be exactly the names it lists, so a deleted or
 * renamed case fails instead of vanishing.
 */

// Tests run from web/, the package root.
const WEB = process.cwd();

function read(path: string): unknown {
  return parseStrictYAML(readFileSync(resolve(WEB, path), 'utf8'), path);
}

function requireNames(root: Record<string, unknown>, rows: Record<string, unknown>[], path: string): void {
  requireUniqueNames(rows, path);
  const required = root.requiredNames;
  if (!Array.isArray(required) || required.some((name) => typeof name !== 'string')) {
    throw new Error(`${path}.requiredNames must be a list of names`);
  }
  const names = rows.map((row) => row.name as string);
  const missing = required.filter((name) => !names.includes(name));
  const extra = names.filter((name) => !required.includes(name));
  if (missing.length > 0 || extra.length > 0) {
    throw new Error(`${path}: cases must be exactly requiredNames (missing: ${missing.join(', ') || 'none'}; not listed: ${extra.join(', ') || 'none'})`);
  }
}

/** The server's key manifest: every key GET /api/v1/settings serves. */
export function loadServerKeyNames(): string[] {
  const path = '../internal/api/testdata/settings-keys.yaml';
  const root = requireRecord(read(path), path);
  const names = root.requiredNames;
  if (!Array.isArray(names) || names.some((name) => typeof name !== 'string')) {
    throw new Error(`${path}.requiredNames must be a list of keys`);
  }
  return names as string[];
}

/** GET /api/v1/settings on a server with no config.yaml, checked against the contract. */
export function loadSettingsResponse(): LocalSettingsResponse {
  const path = 'src/app/settings/testdata/settings-response.yaml';
  const parsed = zLocalSettingsResponse.safeParse(read(path));
  if (!parsed.success) throw new Error(`${path} breaks the Local API contract: ${parsed.error.issues[0]?.message}`);
  return parsed.data;
}

export interface SettingsAuthRecoveryCase {
  name: string;
  status: number;
  error: string;
  afterLogout: boolean;
}

export function loadAuthRecoveryCases(): SettingsAuthRecoveryCase[] {
  const path = 'src/app/settings/testdata/settings-auth-recovery.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value) => {
    const row = requireRecord(value, path);
    requireExactRequiredFields(row, ['name', 'status', 'error', 'afterLogout'], path);
    if (typeof row.status !== 'number' || typeof row.error !== 'string' || typeof row.afterLogout !== 'boolean') throw new Error(`${path}: invalid recovery case`);
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as SettingsAuthRecoveryCase[];
}

export interface SettingsGroupFixture {
  name: string;
  open: boolean;
}

export function loadSettingsGroups(): SettingsGroupFixture[] {
  const path = 'src/app/testdata/settings-groups.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'groups'], path);
  if (!Array.isArray(root.groups)) throw new Error(`${path}.groups must be a list`);
  const groups = root.groups.map((value, index) => {
    const group = requireRecord(value, `${path}.groups[${index}]`);
    requireExactRequiredFields(group, ['name', 'open'], `${path}.groups[${index}]`);
    if (typeof group.open !== 'boolean') throw new Error(`${path}.groups[${index}].open must be true or false`);
    return group;
  });
  requireNames(root, groups, path);
  return groups as unknown as SettingsGroupFixture[];
}

export interface SettingWriteCase {
  name: string;
  key: string;
  input: { toggle?: true; select?: string; type?: string };
  answer: null | { hold: true } | { status: number; body: unknown };
  expect: { request: unknown; status: string; shown: string; alert: string | null };
}

export function loadSettingWrites(): SettingWriteCase[] {
  const path = 'src/app/settings/testdata/settings-writes.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value, index) => {
    const at = `${path}.cases[${index}]`;
    const row = requireRecord(value, at);
    requireExactRequiredFields(row, ['name', 'key', 'input', 'answer', 'expect'], at);
    const input = requireRecord(row.input, `${at}.input`);
    requireExactFields(input, ['toggle', 'select', 'type'], `${at}.input`);
    if (Object.keys(input).length !== 1) throw new Error(`${at}.input must name exactly one action`);
    if (row.answer !== null) {
      const answer = requireRecord(row.answer, `${at}.answer`);
      if ('hold' in answer) requireExactRequiredFields(answer, ['hold'], `${at}.answer`);
      else requireExactRequiredFields(answer, ['status', 'body'], `${at}.answer`);
    }
    requireExactRequiredFields(requireRecord(row.expect, `${at}.expect`), ['request', 'status', 'shown', 'alert'], `${at}.expect`);
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as SettingWriteCase[];
}

export interface InstallCase {
  name: string;
  rules: AutoPublishRule[];
  answers: Record<string, { status: number; body: unknown }>;
  offer: string | null;
  calls: string[];
  /** Per repository path, the texts its install preview shows before the click. */
  preview: Record<string, string[]>;
  /** Per repository path, the texts its install result shows after the click. */
  shows: Record<string, string[]>;
  /** Per rule id, the texts its rule view shows after the install. */
  refreshed: Record<string, string[]>;
}

export function loadInstallCases(): InstallCase[] {
  const path = 'src/app/settings/testdata/settings-install.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value, index) => {
    const at = `${path}.cases[${index}]`;
    const row = requireRecord(value, at);
    requireExactRequiredFields(row, ['name', 'rules', 'answers', 'offer', 'calls', 'preview', 'shows', 'refreshed'], at);
    const parsed = zLocalSettingsResponse.safeParse({ settings: [], autoPublish: row.rules });
    if (!parsed.success) throw new Error(`${at}.rules break the Local API contract: ${parsed.error.issues[0]?.message}`);
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as InstallCase[];
}

export interface PendingInstallCase {
  name: string;
  openForm: boolean;
  rule: AutoPublishRule;
  nextMatch: string;
}

/** The real install POST is held while conflicting rule controls are observed. */
export function loadPendingInstallCases(): PendingInstallCase[] {
  const path = 'src/app/settings/testdata/settings-install-pending.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  const expectedNames = ["installing locks existing rule actions until its response completes", "installing locks an open edit form until its response completes"];
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== expectedNames.sort().join()) {
    throw new Error(`${path}.requiredNames must retain every pending-mutation case`);
  }
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value, index) => {
    const at = `${path}.cases[${index}]`;
    const row = requireRecord(value, at);
    requireExactRequiredFields(row, ['name', 'openForm', 'rule', 'nextMatch'], at);
    if (typeof row.openForm !== 'boolean' || typeof row.nextMatch !== 'string') throw new Error(`${at}: invalid pending install case`);
    const parsed = zLocalSettingsResponse.safeParse({ settings: [], autoPublish: [row.rule] });
    if (!parsed.success) throw new Error(`${at}.rule breaks the Local API contract: ${parsed.error.issues[0]?.message}`);
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as PendingInstallCase[];
}

export interface PendingRuleWriteCase {
  name: string;
  rules: AutoPublishRule[];
}

export function loadPendingRuleWriteCases(): PendingRuleWriteCase[] {
  const path = 'src/app/settings/testdata/settings-rule-write-pending.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  const expectedNames = ["a pending rule write serializes independent rule decisions"];
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== expectedNames.sort().join()) {
    throw new Error(`${path}.requiredNames must retain every pending-mutation case`);
  }
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value, index) => {
    const at = `${path}.cases[${index}]`;
    const row = requireRecord(value, at);
    requireExactRequiredFields(row, ['name', 'rules'], at);
    const parsed = zLocalSettingsResponse.safeParse({ settings: [], autoPublish: row.rules });
    if (!parsed.success || parsed.data.autoPublish.length !== 2) throw new Error(`${at}.rules must be two valid rules`);
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as PendingRuleWriteCase[];
}

export interface PendingPatternWriteCase {
  name: string;
  patterns: { id: string; category: string; pattern: string; replacement: string }[];
}

export function loadPendingPatternWriteCases(): PendingPatternWriteCase[] {
  const path = 'src/app/settings/testdata/settings-pattern-write-pending.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  const expectedNames = ['a pending pattern removal serializes whole-list writes'];
  if (!Array.isArray(root.requiredNames) || [...root.requiredNames].sort().join() !== expectedNames.sort().join()) {
    throw new Error(`${path}.requiredNames must retain the pending whole-list write case`);
  }
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value, index) => {
    const at = `${path}.cases[${index}]`;
    const row = requireRecord(value, at);
    requireExactRequiredFields(row, ['name', 'patterns'], at);
    // Two entries are the invariant: removing one must retain the other until its own save.
    if (!Array.isArray(row.patterns) || row.patterns.length !== 2) throw new Error(`${at}.patterns must contain two independent patterns`);
    row.patterns.forEach((value, index) => {
      const pattern = requireRecord(value, `${at}.patterns[${index}]`);
      requireExactRequiredFields(pattern, ['id', 'category', 'pattern', 'replacement'], at);
      if (Object.values(pattern).some((field) => typeof field !== 'string')) throw new Error(`${at}: pattern fields must be strings`);
    });
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as PendingPatternWriteCase[];
}
