import { describe, expect, it } from 'vitest';
import { parseStrictYAML, requireExactFields, requireRecord, requireUniqueNames } from './strictYaml';

describe('strict YAML fixture loading', () => {
  it('rejects duplicate keys, trailing documents, unknown fields, and duplicate case names', () => {
    expect(() => parseStrictYAML('value: 1\nvalue: 2\n', 'fixture')).toThrow(/unique/);
    expect(() => parseStrictYAML('value: 1\n---\nvalue: 2\n', 'fixture')).toThrow(/exactly one/);
    const row = requireRecord(parseStrictYAML('name: case\nunexpected: true\n', 'fixture'), 'fixture');
    expect(() => requireExactFields(row, ['name'], 'fixture')).toThrow(/unknown fields/);
    expect(() => requireUniqueNames([{ name: 'same' }, { name: 'same' }], 'fixture.cases')).toThrow(/duplicates name/);
  });
});
