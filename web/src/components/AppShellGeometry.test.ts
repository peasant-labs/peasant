import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import YAML from 'yaml';

interface SourceToken {
  path: string;
  token: string;
}

interface GeometryFixture {
  variable: string;
  value: string;
  headerRow: SourceToken;
  noticeHeight: SourceToken;
  consumers: SourceToken[];
  forbidden: string[];
}

const fixture = YAML.parse(
  readFileSync(resolve(process.cwd(), 'src/components/testdata/app-shell-geometry.yaml'), 'utf8'),
) as GeometryFixture;

const source = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8');

describe('app shell geometry', () => {
  it('declares the top chrome height once: the header row plus the offline notice', () => {
    const globals = source('src/app/globals.css');
    expect(globals).toContain(`${fixture.variable}: ${fixture.value};`);
    expect(globals.split(`${fixture.variable}:`)).toHaveLength(2);
  });

  it('keeps the header row at --nav-h, and lets only the notice grow the chrome', () => {
    const header = source(fixture.headerRow.path);
    expect(header).toContain(fixture.headerRow.token);
    expect(header).not.toContain(`var(${fixture.variable})`);
    expect(source(fixture.noticeHeight.path)).toContain(fixture.noticeHeight.token);
  });

  it('clears the chrome with the same height in the main offset and the transcript bound', () => {
    for (const consumer of fixture.consumers) {
      const text = source(consumer.path);
      expect(text, `${consumer.path} must consume the canonical shell height`).toContain(consumer.token);
      for (const forbidden of fixture.forbidden) {
        expect(text, `${consumer.path} must not restore a fixed desktop-only subtraction`).not.toContain(forbidden);
      }
    }
  });
});
