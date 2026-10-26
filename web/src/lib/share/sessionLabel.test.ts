import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { sessionDay, sessionPreviewTitle } from './sessionLabel';

interface Case { name: string; preview: string; id: string; title: string; startTime?: string; day?: string }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/session-labels.yaml'), 'utf8')) as { requiredNames: string[]; cases: Case[] };

describe('session labels', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });
  for (const row of fixture.cases) {
    it(row.name, () => {
      expect(sessionPreviewTitle(row.preview, row.id)).toBe(row.title);
      if (row.startTime) expect(sessionDay(row.startTime)).toBe(row.day);
    });
  }
});
