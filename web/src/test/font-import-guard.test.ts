import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';

/* GUARD the font fix (commit 17572d4f, self-hosted via next/font/local in a later
 * round). The design-system's `@peasant-labs/fairtrade/fonts.css` loads Atkinson via
 * a remote `@import url(...)`. Next's PRODUCTION CSS bundling (Lightning CSS)
 * relocates that @import AFTER other rules — an invalid @import position the browser
 * silently DROPS — leaving the WHOLE app on a ui-monospace fallback (the
 * "everything went mono" regression the user hit).
 *
 * The fix originally loaded the fonts via a runtime `<link rel="stylesheet"
 * href="https://fonts.googleapis.com/...">` in the root layout instead (still safe
 * from the @import-drop bug, since Next preserves <link> tags) — but that still
 * depended on a THIRD-PARTY CDN at every real-user page load (a display:swap flash
 * of the fallback font while the round-trip resolves, and a real external
 * dependency in tension with peasant's local-first positioning). The current fix
 * self-hosts the identical woff2 files via next/font/local (src/app/fonts/index.ts,
 * fetched once at build time, no runtime request) — see globals.css for how its
 * --font-atkinson-hyperlegible(-mono) variables feed fairtrade's --font-body/
 * --font-display/--font-mono tokens.
 *
 * This test fails the build if anything reintroduces a drop-prone or flaky path:
 *   (a) a JS/TS/CSS import of `fonts.css`  (it carries the remote @import), or
 *   (b) a direct remote `@import url(https://…)` in any app source, or
 *   (c) a runtime <link> to fonts.googleapis.com/fonts.gstatic.com (reintroduces the
 *       external-CDN/FOUT dependency that self-hosting removes).
 * The allowed form is next/font/local (src/app/fonts/index.ts, imported by
 * layout.tsx).
 */

const SRC = resolve(__dirname, '..'); // web/src
const EXTS = new Set(['.ts', '.tsx', '.css']);

// Scope to SHIPPED app source: test/spec files are not bundled into the prod app (the
// font-drop only affects production-bundled CSS), and this guard itself documents the
// forbidden patterns in prose — scanning it would self-match.
const isShipped = (p: string) => !/\.(test|spec)\.[tj]sx?$/.test(p);

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    const st = statSync(p);
    if (st.isDirectory()) {
      if (name === 'node_modules' || name === '.next') continue;
      walk(p, out);
    } else if (EXTS.has(p.slice(p.lastIndexOf('.'))) && isShipped(p)) {
      out.push(p);
    }
  }
  return out;
}

// a JS/TS/CSS *import* of a fonts.css module (side-effect or named, or a CSS @import) — NOT a
// bare mention in a comment (those have no `import` + quote + fonts.css sequence).
const FONTS_CSS_IMPORT = /\bimport\s+(?:[^'"\n]*\sfrom\s+)?['"][^'"\n]*fonts\.css['"]/;
// a remote @import (the exact form Next drops in prod).
const REMOTE_AT_IMPORT = /@import\s+(?:url\(\s*)?['"]?https?:\/\//i;
// a runtime <link> to the Google Fonts CDN (the flaky/FOUT-prone predecessor fix).
const GOOGLE_FONTS_LINK = /<link[^>]*(fonts\.googleapis\.com|fonts\.gstatic\.com)/;

describe('font-import guard (keeps the self-hosted next/font/local form, blocks the dropped @import and the CDN <link>)', () => {
  const files = walk(SRC);

  it('scans a non-trivial set of app source files', () => {
    expect(files.length).toBeGreaterThan(20);
  });

  it('no app source imports fonts.css (it carries the drop-prone remote @import)', () => {
    const offenders: string[] = [];
    for (const f of files) {
      const lines = readFileSync(f, 'utf8').split('\n');
      lines.forEach((line, i) => {
        if (FONTS_CSS_IMPORT.test(line)) offenders.push(`${f.replace(SRC, 'src')}:${i + 1}  ${line.trim()}`);
      });
    }
    expect(offenders, `load Atkinson via next/font/local (src/app/fonts), not a fonts.css import:\n${offenders.join('\n')}`).toEqual([]);
  });

  it('no app source uses a remote @import (Next drops it in prod → whole-app mono)', () => {
    const offenders: string[] = [];
    for (const f of files) {
      const lines = readFileSync(f, 'utf8').split('\n');
      lines.forEach((line, i) => {
        if (REMOTE_AT_IMPORT.test(line)) offenders.push(`${f.replace(SRC, 'src')}:${i + 1}  ${line.trim()}`);
      });
    }
    expect(offenders, `load fonts via next/font/local, never a CSS @import:\n${offenders.join('\n')}`).toEqual([]);
  });

  it('no app source links the Google Fonts CDN at runtime (self-host via next/font/local instead)', () => {
    const offenders: string[] = [];
    for (const f of files) {
      const lines = readFileSync(f, 'utf8').split('\n');
      lines.forEach((line, i) => {
        if (GOOGLE_FONTS_LINK.test(line)) offenders.push(`${f.replace(SRC, 'src')}:${i + 1}  ${line.trim()}`);
      });
    }
    expect(
      offenders,
      `fonts are self-hosted via next/font/local (src/app/fonts) — a runtime <link> to Google Fonts reintroduces the external-CDN dependency and display:swap FOUT:\n${offenders.join('\n')}`,
    ).toEqual([]);
  });

  it('layout.tsx applies the self-hosted font variables', () => {
    const layout = readFileSync(join(SRC, 'app', 'layout.tsx'), 'utf8');
    expect(layout).toMatch(/from ['"]@\/app\/fonts['"]/);
    expect(layout).toMatch(/atkinsonHyperlegible\.variable/);
    expect(layout).toMatch(/atkinsonHyperlegibleMono\.variable/);
  });
});
