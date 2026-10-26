import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { checkingStage, type CheckingStep } from './checkingStage';

interface Case { name: string; step: CheckingStep; turns: number; elapsedMs: number; stage: string; note?: string }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/checking-stages.yaml'), 'utf8')) as { requiredNames: string[]; cases: Case[] };

describe('checkingStage', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });
  for (const row of fixture.cases) {
    it(row.name, () => {
      const said = checkingStage(row.step, { turns: row.turns, elapsedMs: row.elapsedMs });
      expect(said.stage).toBe(row.stage);
      expect(said.note).toBe(row.note);
    });
  }
});
