import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { loadCases, type FixtureCase } from '@/test/fixtures/kickstart';

/**
 * The host classes that make a wizard frame own the viewport are defined once.
 * Each case reads the sources that would let a second copy, written against
 * fairtrade's own class names, grow back beside it.
 */

const PATH = 'src/test/testdata/wizard_host.yaml';
const read = (file: string) => readFileSync(resolve(process.cwd(), file), 'utf8');

function stylesheets(root: string): string[] {
  const found: string[] = [];
  const visit = (path: string) => {
    if (statSync(path).isDirectory()) {
      for (const entry of readdirSync(path)) visit(join(path, entry));
    } else if (path.endsWith('.css')) {
      found.push(relative(process.cwd(), path));
    }
  };
  visit(resolve(process.cwd(), root));
  return found;
}

/** The selectors of every rule in a stylesheet, one per comma-separated part, comments removed. */
function selectors(css: string): string[] {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, '');
  return [...bare.matchAll(/([^{}]+)\{/g)].flatMap((match) => match[1].split(',').map((part) => part.trim())).filter((part) => part !== '' && !part.startsWith('@'));
}

const CHECKS: Record<string, (entry: FixtureCase) => void> = {
  'the-host-classes-are-defined-once': (entry) => {
    const css = read(entry.stylesheet as string);
    for (const opening of entry.defined as string[]) {
      expect(css.split(opening).length - 1, `${opening} is written once in ${entry.stylesheet}`).toBe(1);
    }
  },
  'no-other-stylesheet-styles-the-frames-regions': (entry) => {
    const offenders = stylesheets('src')
      .filter((file) => file !== entry.owner)
      .filter((file) => selectors(read(file)).some((selector) => (entry.regions as string[]).some((region) => selector.includes(region))));
    expect(offenders, 'a stylesheet other than the owner styles the frame\'s own regions').toEqual([]);
  },
  'the-frames-regions-are-styled-only-by-the-host-and-the-publish-phone-override': (entry) => {
    const allowed = entry.allowedPrefixes as string[];
    const offenders = selectors(read(entry.stylesheet as string))
      .filter((selector) => (entry.regions as string[]).some((region) => selector.includes(region)))
      .filter((selector) => !allowed.some((prefix) => selector.startsWith(`${prefix} `)));
    expect(offenders, 'a rule styles a frame region without going through the host class').toEqual([]);
  },
  'every-page-that-draws-the-frame-uses-the-host-classes': (entry) => {
    for (const page of entry.pages as { file: string; uses: string[] }[]) {
      const source = read(page.file);
      for (const name of page.uses) expect(source, `${page.file} puts ${name} on its markup`).toMatch(new RegExp(`className="[^"]*\\b${name}\\b`));
    }
  },
};

describe('the wizard host classes', () => {
  const cases = loadCases(PATH);
  it('has exactly the checks the fixture names', () => {
    expect(Object.keys(CHECKS).sort()).toEqual(cases.map((entry) => entry.name).sort());
  });
  for (const entry of cases) it(entry.name, () => CHECKS[entry.name](entry));
});
