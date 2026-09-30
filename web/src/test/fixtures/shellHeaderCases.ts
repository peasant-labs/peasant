import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  parseStrictYAML,
  requireExactFields,
  requireExactRequiredFields,
  requireRecord,
  requireUniqueNames,
} from '@/test/strictYaml';

/** The pages and capability sets the shell component tests hold the manifest on. */
export interface ShellHeaderCases {
  pages: Array<{ name: string; pathname: string }>;
  paletteCapabilities: Array<{ name: string; tokens: string[] }>;
}

const REQUIRED_PAGES = ['home', 'analytics', 'changes', 'code-map', 'transcript', 'share'];
const REQUIRED_CAPABILITY_SETS = ['none', 'code-map-token'];

function rows(value: unknown, path: string, fields: readonly string[], required: readonly string[]) {
  if (!Array.isArray(value)) throw new Error(`${path} must be a list`);
  const records = value.map((row, index) => requireRecord(row, `${path}[${index}]`));
  records.forEach((row, index) => requireExactFields(row, fields, `${path}[${index}]`));
  requireUniqueNames(records, path);
  const names = new Set(records.map((row) => row.name));
  const missing = required.filter((name) => !names.has(name));
  if (missing.length) throw new Error(`${path} is missing required names: ${missing.join(', ')}`);
  return records;
}

function load(): ShellHeaderCases {
  const path = resolve(process.cwd(), 'src/components/testdata/shell_header_cases.yaml');
  const root = requireRecord(parseStrictYAML(readFileSync(path, 'utf8'), 'shell header cases'), 'shell header cases');
  requireExactRequiredFields(root, ['pages', 'paletteCapabilities'], 'shell header cases');
  const pages = rows(root.pages, 'shell header cases.pages', ['name', 'pathname'], REQUIRED_PAGES);
  pages.forEach((row) => {
    if (typeof row.pathname !== 'string' || !row.pathname.startsWith('/')) throw new Error(`shell header cases page ${String(row.name)} needs an absolute pathname`);
  });
  const sets = rows(root.paletteCapabilities, 'shell header cases.paletteCapabilities', ['name', 'tokens'], REQUIRED_CAPABILITY_SETS);
  sets.forEach((row) => {
    if (!Array.isArray(row.tokens) || row.tokens.some((token) => typeof token !== 'string')) throw new Error(`shell header cases capability set ${String(row.name)} needs a token list`);
  });
  return root as unknown as ShellHeaderCases;
}

export const SHELL_HEADER_CASES = load();
