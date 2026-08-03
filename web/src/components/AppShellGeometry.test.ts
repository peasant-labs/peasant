import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import YAML from 'yaml';

interface GeometryFixture {
  variable: string;
  breakpoints: { mobile: string; desktop: string };
  consumers: Array<{ path: string; token: string }>;
  forbidden: string[];
}

const fixture = YAML.parse(
  readFileSync(resolve(process.cwd(), 'src/components/testdata/app-shell-geometry.yaml'), 'utf8'),
) as GeometryFixture;

describe('app shell geometry', () => {
  it('uses one responsive header height for the navbar, main offset, and transcript bound', () => {
    expect(fixture.consumers).toHaveLength(3);
    const globals = readFileSync(resolve(process.cwd(), 'src/app/globals.css'), 'utf8');
    expect(globals).toContain(`${fixture.variable}: ${fixture.breakpoints.mobile}`);
    expect(globals).toContain(`${fixture.variable}: ${fixture.breakpoints.desktop}`);

    for (const consumer of fixture.consumers) {
      const source = readFileSync(resolve(process.cwd(), consumer.path), 'utf8');
      expect(source, `${consumer.path} must consume the canonical shell height`).toContain(consumer.token);
      for (const forbidden of fixture.forbidden) {
        expect(source, `${consumer.path} must not restore a fixed desktop-only subtraction`).not.toContain(forbidden);
      }
    }
  });
});
