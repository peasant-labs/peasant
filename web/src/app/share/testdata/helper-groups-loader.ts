/**
 * Strict typed loader for the helper-group corpus loader-mutation cases. The
 * case list is a fixture, never an inline table: each case declares exactly one
 * closed-set mutation, its name must be unique, and the manifest must match the
 * declared case names in both directions.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';

export type LoaderMutation =
  | { name: string; operation: 'set-required-names'; value: string[] }
  | { name: string; operation: 'remove-required-name'; role: string }
  | { name: string; operation: 'rename-corpus-role'; from: string; to: string }
  | { name: string; operation: 'set-member-field'; role: string; field: string; value: unknown }
  | { name: string; operation: 'set-context-field'; role: string; field: string; value: unknown }
  | { name: string; operation: 'set-root-field'; field: string; value: unknown };

const OPERATIONS = new Set<LoaderMutation['operation']>([
  'set-required-names',
  'remove-required-name',
  'rename-corpus-role',
  'set-member-field',
  'set-context-field',
  'set-root-field',
]);

function fail(where: string, reason: string): never {
  throw new Error(`helper-group loader fixture is invalid at ${where}: ${reason}; fix the case table, then rerun the mounted chooser tests`);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function requireRecord(value: unknown, where: string): Record<string, unknown> {
  if (!isRecord(value)) fail(where, `expected an object, received ${JSON.stringify(value)}`);
  return value;
}

function requireString(value: unknown, where: string): string {
  if (typeof value !== 'string' || value.trim().length === 0) fail(where, `expected a non-empty string, received ${JSON.stringify(value)}`);
  return value;
}

function requireStrings(value: unknown, where: string): string[] {
  if (!Array.isArray(value)) fail(where, `expected an array, received ${JSON.stringify(value)}`);
  return value.map((entry, index) => requireString(entry, `${where}[${index}]`));
}

function parseCase(value: unknown, where: string): LoaderMutation {
  const record = requireRecord(value, where);
  const name = requireString(record.name, `${where}.name`);
  const operation = requireString(record.operation, `${where}.operation`) as LoaderMutation['operation'];
  if (!OPERATIONS.has(operation)) fail(`${where}.operation`, `unknown operation ${JSON.stringify(operation)}`);
  switch (operation) {
    case 'set-required-names':
      return { name, operation, value: requireStrings(record.value, `${where}.value`) };
    case 'remove-required-name':
      return { name, operation, role: requireString(record.role, `${where}.role`) };
    case 'rename-corpus-role':
      return { name, operation, from: requireString(record.from, `${where}.from`), to: requireString(record.to, `${where}.to`) };
    case 'set-member-field':
    case 'set-context-field': {
      if (record.value === undefined) fail(`${where}.value`, 'a field mutation must carry a value');
      return { name, operation, role: requireString(record.role, `${where}.role`), field: requireString(record.field, `${where}.field`), value: record.value };
    }
    case 'set-root-field': {
      if (record.value === undefined) fail(`${where}.value`, 'a root-field mutation must carry a value');
      return { name, operation, field: requireString(record.field, `${where}.field`), value: record.value };
    }
  }
}

export interface LoaderMutationFixture {
  requiredNames: string[];
  cases: LoaderMutation[];
}

export function loadLoaderMutationFixture(source: string): LoaderMutationFixture {
  const parsed: unknown = parse(source, { strict: true, uniqueKeys: true });
  const root = requireRecord(parsed, '<root>');
  const keys = Object.keys(root).slice().sort().join(',');
  if (keys !== 'cases,requiredNames') fail('<root>', `expected exactly cases, requiredNames, received ${keys}`);
  const requiredNames = requireStrings(root.requiredNames, 'requiredNames');
  if (new Set(requiredNames).size !== requiredNames.length) fail('requiredNames', 'case names must be unique');
  const cases = (Array.isArray(root.cases) ? root.cases : fail('cases', 'expected an array')).map((entry, index) => parseCase(entry, `cases[${index}]`));
  const caseNames = cases.map((entry) => entry.name);
  if (new Set(caseNames).size !== caseNames.length) fail('cases', 'case names must be unique');
  const requiredSet = new Set(requiredNames);
  const caseSet = new Set(caseNames);
  for (const name of requiredSet) if (!caseSet.has(name)) fail('requiredNames', `required case ${name} is not present`);
  for (const name of caseSet) if (!requiredSet.has(name)) fail('cases', `case ${name} is not declared in requiredNames`);
  return { requiredNames, cases };
}

export function readLoaderMutationFixture(): LoaderMutationFixture {
  return loadLoaderMutationFixture(readFileSync(resolve(process.cwd(), 'src/app/share/testdata/helper-groups-loader.yaml'), 'utf8'));
}
