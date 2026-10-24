import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { plainReason } from './publishFailure';

interface Case { name: string; message: string; reason: string }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/plain-reasons.yaml'), 'utf8')) as { requiredNames: string[]; cases: Case[] };

describe('plainReason', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });
  for (const row of fixture.cases) {
    it(row.name, () => {
      const reason = plainReason(row.message);
      expect(reason ?? '').toBe(row.reason);
      expect(reason ?? '').not.toMatch(/https?:\/\/|\d+\.\d+\.\d+\.\d+|(?<!peasant )village|[{}]|\(\d{3}\)/i);
    });
  }
});
