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
  bodyVariable: string;
  noticePinnedVariant: string;
  noticePinnedClass: string;
  headerRow: SourceToken;
  noticeHeight: SourceToken;
  consumers: SourceToken[];
  forbidden: string[];
}

const fixture = YAML.parse(
  readFileSync(resolve(process.cwd(), 'src/components/testdata/app-shell-geometry.yaml'), 'utf8'),
) as GeometryFixture;

const source = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8');

/** Every place that clears the top chrome; a consumer row deleted from the fixture fails here. */
const REQUIRED_CONSUMERS = ['src/app/layout.tsx', 'src/components/session-detail/v2/SessionDetailV2.tsx'];

describe('app shell geometry', () => {
  it('declares where content starts once: the header row plus the offline notice', () => {
    const globals = source('src/app/globals.css');
    expect(globals).toContain(`${fixture.variable}: ${fixture.value};`);
    expect(globals.split(`${fixture.variable}:`)).toHaveLength(2);
    // The full-height body is derived from it, once, and the share page fills it.
    expect(globals.split(`${fixture.bodyVariable}:`)).toHaveLength(2);
    expect(globals).toContain(`height: var(${fixture.bodyVariable});`);
  });

  it('declares the pinned-notice query once and uses it for the pin and the scroll padding', () => {
    const globals = source('src/app/globals.css');
    expect(globals.split(fixture.noticePinnedVariant)).toHaveLength(2);
    expect(globals).toMatch(/@variant notice-pinned \{\s*scroll-padding-top: var\(--app-header-height\);/);
    // No second, hand-written copy of the query.
    expect(globals.split('(min-height: 40rem) and (min-width: 48rem)')).toHaveLength(2);
    const notice = source(fixture.noticeHeight.path);
    expect(notice).toContain(`'${fixture.noticePinnedClass}'`);
  });

  it('keeps the header row at --nav-h, and lets only the notice move where content starts', () => {
    const header = source(fixture.headerRow.path);
    expect(header).toContain(fixture.headerRow.token);
    expect(header).not.toContain(`var(${fixture.variable})`);
    expect(source(fixture.noticeHeight.path)).toContain(fixture.noticeHeight.token);
  });

  it('clears the header and notice with the same height in the main offset and the transcript bound', () => {
    const paths = fixture.consumers.map((consumer) => consumer.path);
    expect(REQUIRED_CONSUMERS.filter((path) => !paths.includes(path))).toEqual([]);
    for (const consumer of fixture.consumers) {
      const text = source(consumer.path);
      expect(text, `${consumer.path} must consume the canonical shell height`).toContain(consumer.token);
      for (const forbidden of fixture.forbidden) {
        expect(text, `${consumer.path} must not restore a fixed desktop-only subtraction`).not.toContain(forbidden);
      }
    }
  });
});
