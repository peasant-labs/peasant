import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import * as fairtrade from '@peasant-labs/fairtrade/ui';
import * as schema from '@peasant-labs/schema';
import * as values from './values';
import { KickstartValueError } from './values';
import { loadCases, readFixture } from '@/test/fixtures/kickstart';
import { requireRecord } from '@/test/strictYaml';

/**
 * Each closed set of the kickstart wire has one definition site and is walked
 * here from a fixture: the tokens it must hold, the schema list it must equal,
 * and the inputs its parser must refuse. The consumer scan holds production
 * code to comparing members, never tokens.
 */

const PATH = 'src/lib/kickstart/testdata/values.yaml';

interface SetRow {
  name: string;
  all: string;
  parse: string;
  schemaAll: string;
  wires: string[];
  refuses: unknown[];
}

function rows(): SetRow[] {
  const root = requireRecord(readFixture(PATH), PATH);
  if (!Array.isArray(root.sets)) throw new Error(`${PATH}.sets must be a list`);
  return root.sets.map((row, index) => requireRecord(row, `${PATH}.sets[${index}]`) as unknown as SetRow);
}

type Member = { wire: string; label: string; String(): string; toString(): string; toJSON(): string };

const exported = values as unknown as Record<string, unknown>;
const fairtradeExports = fairtrade as unknown as Record<string, unknown>;
const schemaExports = schema as unknown as Record<string, unknown>;

function members(row: SetRow): readonly Member[] {
  const all = exported[row.all];
  if (!Array.isArray(all)) throw new Error(`${PATH}: values.ts exports no list named ${row.all}`);
  return all as readonly Member[];
}

function parser(row: SetRow): (raw: unknown, where?: string) => Member {
  const parse = exported[row.parse];
  if (typeof parse !== 'function') throw new Error(`${PATH}: values.ts exports no parser named ${row.parse}`);
  return parse as (raw: unknown, where?: string) => Member;
}

function productionFiles(roots: readonly string[], definitionSite: string): string[] {
  const out: string[] = [];
  const visit = (path: string) => {
    const info = statSync(path);
    if (info.isDirectory()) {
      if (path.endsWith('testdata')) return;
      for (const entry of readdirSync(path)) visit(join(path, entry));
      return;
    }
    const relativePath = relative(process.cwd(), path);
    if (!/\.(ts|tsx)$/.test(path) || /\.test\.tsx?$/.test(path) || relativePath === definitionSite) return;
    out.push(path);
  };
  for (const root of roots) visit(resolve(process.cwd(), root));
  return out;
}

const escape = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

const CHECKS: Record<string, () => void> = {
  'one-definition-site-per-set': () => {
    const root = requireRecord(readFixture(PATH), PATH);
    const scan = requireRecord(root.consumerScan, `${PATH}.consumerScan`);
    const wires = [...new Set(rows().flatMap((row) => row.wires))];
    const definition = new RegExp(`closed\\(\\s*['"\`](${wires.map(escape).join('|')})['"\`]`);
    const offenders = productionFiles(scan.roots as string[], scan.definitionSite as string)
      .filter((file) => definition.test(readFileSync(file, 'utf8')))
      .map((file) => relative(process.cwd(), file));
    expect(offenders, 'a kickstart token is defined outside values.ts').toEqual([]);
    const source = readFileSync(resolve(process.cwd(), scan.definitionSite as string), 'utf8');
    for (const row of rows()) {
      for (const wire of row.wires) {
        expect(source, `${row.name}: ${wire} is defined in ${scan.definitionSite}`).toMatch(new RegExp(`closed\\(\\s*['"\`]${escape(wire)}['"\`]`));
      }
    }
  },

  'all-coverage': () => {
    for (const row of rows()) {
      const list = members(row);
      expect(list.map((member) => member.wire), `${row.name}: ALL list`).toEqual(row.wires);
      expect(new Set(list).size, `${row.name}: members are distinct`).toBe(list.length);
      for (const member of list) {
        expect(Object.isFrozen(member), `${row.name}.${member.wire} is frozen`).toBe(true);
        expect(member.label.length, `${row.name}.${member.wire} has a label`).toBeGreaterThan(0);
        expect(member.String()).toBe(member.wire);
        expect(`${member}`).toBe(member.wire);
        expect(JSON.stringify({ member })).toBe(JSON.stringify({ member: member.wire }));
      }
    }
  },

  'boundary-parser-accepts-every-wire': () => {
    for (const row of rows()) {
      const parse = parser(row);
      const list = members(row);
      for (const wire of row.wires) {
        const member = parse(wire, `fixture ${row.name}`);
        expect(member, `${row.name}: ${wire} parses to the one member`).toBe(list.find((candidate) => candidate.wire === wire));
      }
    }
  },

  'boundary-parser-fails-closed': () => {
    for (const row of rows()) {
      const parse = parser(row);
      for (const raw of row.refuses) {
        let failure: unknown;
        try {
          parse(raw, 'a fixture read');
        } catch (error) {
          failure = error;
        }
        expect(failure, `${row.name}: ${JSON.stringify(raw)} must be refused`).toBeInstanceOf(KickstartValueError);
        const error = failure as KickstartValueError;
        expect(error.raw).toBe(raw);
        expect(error.where).toBe('a fixture read');
        // The message names what it saw, what it knows, where, and what to do.
        expect(error.message).toContain('a fixture read');
        for (const wire of row.wires) expect(error.message).toContain(wire);
        expect(error.message).toMatch(/update peasant/);
      }
    }
  },

  'drift-both-ways-against-schema': () => {
    for (const row of rows()) {
      const fromSchema = schemaExports[row.schemaAll];
      if (!Array.isArray(fromSchema)) throw new Error(`${PATH}: @peasant-labs/schema exports no list named ${row.schemaAll}`);
      const client = members(row).map((member) => member.wire);
      expect(client.filter((wire) => !fromSchema.includes(wire)), `${row.name}: client tokens the schema does not publish`).toEqual([]);
      expect(fromSchema.filter((wire) => !client.includes(wire as string)), `${row.name}: schema tokens the client does not know`).toEqual([]);
    }
  },

  'forest-values-are-the-trees-own-and-match-the-schema': () => {
    const root = requireRecord(readFixture(PATH), PATH);
    if (!Array.isArray(root.forestValues)) throw new Error(`${PATH}.forestValues must be a list`);
    for (const entry of root.forestValues) {
      const row = requireRecord(entry, `${PATH}.forestValues`) as { name: string; fairtrade: string; schemaAll: string; wires: string[] };
      const own = fairtradeExports[row.fairtrade];
      if (!Array.isArray(own)) throw new Error(`${PATH}: @peasant-labs/fairtrade/ui exports no list named ${row.fairtrade}`);
      const fromSchema = schemaExports[row.schemaAll];
      if (!Array.isArray(fromSchema)) throw new Error(`${PATH}: @peasant-labs/schema exports no list named ${row.schemaAll}`);
      const treeWires = (own as { wire: string }[]).map((member) => member.wire);
      expect(treeWires, `${row.name}: the tree's own list`).toEqual(row.wires);
      expect(treeWires.filter((wire) => !fromSchema.includes(wire)), `${row.name}: tree tokens the schema does not publish`).toEqual([]);
      expect(fromSchema.filter((wire) => !treeWires.includes(wire as string)), `${row.name}: schema tokens the tree does not know`).toEqual([]);
    }
    // And this module defines none of them: the sets are not its own.
    expect(Object.keys(exported).filter((key) => /NODE_(KINDS|STATES)$|^Kickstart(NodeKind|NodeState)$/.test(key))).toEqual([]);
  },

  'no-literal-comparison-in-consumers': () => {
    const root = requireRecord(readFixture(PATH), PATH);
    const scan = requireRecord(root.consumerScan, `${PATH}.consumerScan`);
    // The refusal codes are the schema's own closed set, not a set of this module,
    // so the scan reads their tokens from the schema's list.
    const schemaLists = Array.isArray(scan.schemaLists) ? (scan.schemaLists as string[]) : [];
    const schemaTokens = schemaLists.flatMap((name) => {
      const list = schemaExports[name];
      if (!Array.isArray(list)) throw new Error(`${PATH}: @peasant-labs/schema exports no list named ${name}`);
      return list as string[];
    });
    expect(schemaTokens.length, 'the scan reads at least one schema list').toBeGreaterThan(0);
    const wires = [...new Set([...rows().flatMap((row) => row.wires), ...schemaTokens])].map(escape).join('|');
    const quoted = `['"\`](?:${wires})['"\`]`;
    const comparison = new RegExp(`(?:[!=]==?\\s*${quoted})|(?:${quoted}\\s*[!=]==?)|(?:case\\s+${quoted}\\s*:)`);
    const offenders = productionFiles(scan.roots as string[], scan.definitionSite as string)
      .filter((file) => comparison.test(readFileSync(file, 'utf8')))
      .map((file) => relative(process.cwd(), file));
    expect(offenders, 'compare the closed-value members, not their tokens').toEqual([]);
  },
};

describe('kickstart closed sets', () => {
  const cases = loadCases(PATH);

  it('has exactly the checks the fixture names', () => {
    expect(Object.keys(CHECKS).sort()).toEqual(cases.map((entry) => entry.name).sort());
  });

  it('names every set the module exports', () => {
    const named = new Set(rows().map((row) => row.all));
    const exportedLists = Object.keys(exported).filter((key) => /^ALL_KICKSTART_/.test(key));
    expect(exportedLists.filter((key) => !named.has(key))).toEqual([]);
    expect(rows().map((row) => row.name)).toEqual(expect.arrayContaining(['scope', 'event-kind', 'diff-line-type', 'field-kind', 'stop-kind']));
  });

  for (const entry of cases) {
    it(entry.name, () => CHECKS[entry.name]());
  }
});
