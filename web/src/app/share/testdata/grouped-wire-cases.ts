/**
 * Strict typed loader for the grouped wire-decode control cases. Each case
 * mutates one valid grouped payload into a contract violation; the loader
 * validates the closed `base` set, the mutation shape, unique case names, and
 * an exact two-way match against the required-NAME manifest.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';

export type WireCaseBase = 'list' | 'members';

export interface WireSetMutation {
  path: string;
  value: unknown;
}

export interface WireDecodeCase {
  name: string;
  base: WireCaseBase;
  drop: string[];
  set: WireSetMutation[];
}

function fail(where: string, reason: string): never {
  throw new Error(`grouped wire-case fixture is invalid at ${where}: ${reason}; fix the case table, then rerun the mounted chooser tests`);
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

export function loadWireDecodeFixture(source: string): WireDecodeCase[] {
  const parsed: unknown = parse(source, { strict: true, uniqueKeys: true });
  const root = requireRecord(parsed, '<root>');
  const keys = Object.keys(root).slice().sort().join(',');
  if (keys !== 'cases,requiredNames') fail('<root>', `expected exactly cases, requiredNames, received ${keys}`);
  if (!Array.isArray(root.requiredNames)) fail('requiredNames', 'expected an array');
  const requiredNames = root.requiredNames.map((name, index) => requireString(name, `requiredNames[${index}]`));
  if (new Set(requiredNames).size !== requiredNames.length) fail('requiredNames', 'case names must be unique');
  if (!Array.isArray(root.cases)) fail('cases', 'expected an array');
  const cases = root.cases.map((entry, index): WireDecodeCase => {
    const where = `cases[${index}]`;
    const record = requireRecord(entry, where);
    const name = requireString(record.name, `${where}.name`);
    const base = requireString(record.base, `${where}.base`);
    if (base !== 'list' && base !== 'members') fail(`${where}.base`, `expected list or members, received ${JSON.stringify(base)}`);
    const drop = record.drop === undefined ? [] : (Array.isArray(record.drop) ? record.drop : fail(`${where}.drop`, 'expected an array')).map((path, i) => requireString(path, `${where}.drop[${i}]`));
    const set = record.set === undefined ? [] : (Array.isArray(record.set) ? record.set : fail(`${where}.set`, 'expected an array')).map((mutation, i) => {
      const entryRecord = requireRecord(mutation, `${where}.set[${i}]`);
      if (entryRecord.value === undefined) fail(`${where}.set[${i}].value`, 'a set mutation must carry a value');
      return { path: requireString(entryRecord.path, `${where}.set[${i}].path`), value: entryRecord.value };
    });
    if (drop.length === 0 && set.length === 0) fail(where, 'a case must declare at least one drop or set mutation');
    return { name, base, drop, set };
  });
  const caseNames = cases.map((entry) => entry.name);
  if (new Set(caseNames).size !== caseNames.length) fail('cases', 'case names must be unique');
  const requiredSet = new Set(requiredNames);
  const caseSet = new Set(caseNames);
  for (const name of requiredSet) if (!caseSet.has(name)) fail('requiredNames', `required case ${name} is not present`);
  for (const name of caseSet) if (!requiredSet.has(name)) fail('cases', `case ${name} is not declared in requiredNames`);
  return cases;
}

export function readWireDecodeFixture(): WireDecodeCase[] {
  return loadWireDecodeFixture(readFileSync(resolve(process.cwd(), 'src/app/share/testdata/grouped-wire-cases.yaml'), 'utf8'));
}
