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
  shows: Record<string, string[]>;
}

export function loadInstallCases(): InstallCase[] {
  const path = 'src/app/settings/testdata/settings-install.yaml';
  const root = requireRecord(read(path), path);
  requireExactRequiredFields(root, ['requiredNames', 'cases'], path);
  if (!Array.isArray(root.cases)) throw new Error(`${path}.cases must be a list`);
  const cases = root.cases.map((value, index) => {
    const at = `${path}.cases[${index}]`;
    const row = requireRecord(value, at);
    requireExactRequiredFields(row, ['name', 'rules', 'answers', 'offer', 'calls', 'shows'], at);
    const parsed = zLocalSettingsResponse.safeParse({ settings: [], autoPublish: row.rules });
    if (!parsed.success) throw new Error(`${at}.rules break the Local API contract: ${parsed.error.issues[0]?.message}`);
    return row;
  });
  requireNames(root, cases, path);
  return cases as unknown as InstallCase[];
}
