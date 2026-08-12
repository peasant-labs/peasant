import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseStrictYAML, requireExactRequiredFields, requireRecord } from '@/test/strictYaml';
import { decodeDiscovery, requireDiscoveryItem } from './discovery';

const fixturePath = resolve(process.cwd(), 'src/lib/api/testdata/discovery.yaml');
const fixture = requireRecord(parseStrictYAML(readFileSync(fixturePath, 'utf8'), 'discovery fixture'), 'discovery fixture');
requireExactRequiredFields(fixture, ['valid', 'invalid'], 'discovery fixture');
const invalid = fixture.invalid as Array<{ name: string; payload: unknown }>;
if (invalid.length !== 3) throw new Error(`discovery fixture must contain exactly 3 invalid rows, got ${invalid.length}`);

describe('decodeDiscovery', () => {
  it('decodes the exact endpoint shape into an exact session ID map', () => {
    const decoded = decodeDiscovery(fixture.valid);
    expect(decoded.size).toBe(2);
    expect(requireDiscoveryItem(decoded, 'session-b', 'search')).toEqual({
      sessionId: 'session-b', locationLabel: 'app 2', branch: 'feature', selectionStatus: 'unselected',
    });
  });

  it.each(invalid.map((row) => [row.name, row.payload] as const))('fails closed for %s', (_name, payload) => {
    expect(() => decodeDiscovery(payload)).toThrow('Could not decode GET /api/v1/web/discovery');
  });

  it('requires one joined row without rejecting unused extra rows', () => {
    const decoded = decodeDiscovery(fixture.valid);
    expect(requireDiscoveryItem(decoded, 'session-a', 'share').sessionId).toBe('session-a');
    expect(() => requireDiscoveryItem(decoded, 'missing', 'share')).toThrow('requires exactly one discovery row');
  });
});
