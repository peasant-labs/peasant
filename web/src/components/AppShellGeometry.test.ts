import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import YAML from 'yaml';

interface SourceToken {
  path: string;
  token: string;
}

interface GeometryFixture {
  chromeVariable: string;
  chromeValue: string;
  chromeNarrow: string;
  chromeNarrowValue: string;
  variable: string;
  value: string;
  bodyVariable: string;
  bodyFloor: string;
  noticePinnedVariant: string;
  noticePinnedClass: string;
  noticeHeight: SourceToken;
  consumers: SourceToken[];
  forbidden: string[];
  restingMain: { path: string };
}

/** The media query inside the `@custom-variant notice-pinned (@media …);` declaration. */
function pinnedQuery(variant: string): string {
  const match = /@media\s+(\(.+\))\s*\)\s*;\s*$/.exec(variant);
  if (!match) throw new Error(`app-shell-geometry.yaml noticePinnedVariant has no media query: ${variant}`);
  return match[1];
}

const fixture = YAML.parse(
  readFileSync(resolve(process.cwd(), 'src/components/testdata/app-shell-geometry.yaml'), 'utf8'),
) as GeometryFixture;

const source = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8');

/** Every place that clears the top chrome; a consumer row deleted from the fixture fails here. */
const REQUIRED_CONSUMERS = ['src/components/session-detail/v2/SessionDetailV2.tsx'];

describe('app shell geometry', () => {
  it('declares where content starts once: the chrome plus the offline notice', () => {
    const globals = source('src/app/globals.css');
    // The chrome: declared once, overridden once, below the sidebar's breakpoint, to the top bar.
    expect(globals).toContain(`${fixture.chromeVariable}: ${fixture.chromeValue};`);
    expect(globals.split(`${fixture.chromeVariable}:`)).toHaveLength(3);
    expect(globals).toContain(`${fixture.chromeNarrow}\n  :root { ${fixture.chromeVariable}: ${fixture.chromeNarrowValue}; }\n}`);
    expect(globals).toContain(`${fixture.variable}: ${fixture.value};`);
    expect(globals.split(`${fixture.variable}:`)).toHaveLength(2);
    // The full-height body is derived from it, once, with its floor, and the share page fills it.
    expect(globals.split(`${fixture.bodyVariable}:`)).toHaveLength(2);
    expect(globals).toContain(`min(${fixture.bodyFloor}, calc(100dvh - var(${fixture.chromeVariable})))`);
    expect(globals).toContain(`height: var(${fixture.bodyVariable});`);
  });

  it('declares the pinned-notice query once and uses it for the pin and the scroll padding', () => {
    const globals = source('src/app/globals.css');
    expect(globals.split(fixture.noticePinnedVariant)).toHaveLength(2);
    expect(globals).toMatch(/@variant notice-pinned \{\s*scroll-padding-top: var\(--app-header-height\);/);
    // No second, hand-written copy of the query.
    expect(globals.split(pinnedQuery(fixture.noticePinnedVariant))).toHaveLength(2);
    const notice = source(fixture.noticeHeight.path);
    expect(notice).toContain(`'${fixture.noticePinnedClass}'`);
  });

  it('keeps <main> out of the tab order at rest, with no top padding to clear', () => {
    const main = /<main\s[^>]*className=[^>]*>/.exec(source(fixture.restingMain.path));
    expect(main, `${fixture.restingMain.path} must render <main>`).not.toBeNull();
    expect(main![0], '<main> must carry no tabIndex at rest').not.toMatch(/tabIndex/i);
    expect(main![0], 'nothing fixed sits over <main>, so it pads no top').not.toMatch(/\bpt-|padding-top/);
  });

  it('lets only the notice move where content starts', () => {
    expect(source(fixture.noticeHeight.path)).toContain(fixture.noticeHeight.token);
  });

  it('bounds the transcript by the same height the chrome and notice leave', () => {
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
