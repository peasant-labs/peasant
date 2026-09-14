#!/usr/bin/env node
// Emit the web (npm) section of THIRD_PARTY_NOTICES to stdout.
//
// WHY: peasant embeds the built web dashboard (web/out) into the Go binary via
// //go:embed. That bundle compiles in npm packages (React, @peasant-labs/*, and
// their transitive deps), and bundling/minification strips their license files.
// The permissive licenses require their notices to travel with the binary, so
// this reproduces them; the top-level scripts/gen-third-party-notices.sh appends
// this section to the shipped THIRD_PARTY_NOTICES.
//
// HOW: enumerate the production dependency closure via `pnpm licenses list
// --prod --json` (a superset of what tree-shaking actually bundles — safe
// over-inclusion for attribution), read each package's actual license text from
// its installed path, and concatenate in a stable order. Reproducible: versions
// are pinned by pnpm-lock.yaml, license text is content-addressed in the store,
// output is sorted, no timestamps.
//
// CLASSIFICATION (the copyleft guard): each dependency is
//   - PERMISSIVE (incl. CC-BY attribution) -> reproduced in the notices section;
//   - WEAK COPYLEFT (LGPL) -> reproduced AND listed in a source-availability
//     section (LGPL requires offering the library's source; it is a separable,
//     dynamically-loaded component the user may replace);
//   - STRONG COPYLEFT (GPL/AGPL/SSPL) / UNKNOWN / UNLICENSED -> FAIL CLOSED
//     (exit 1). A future such dependency must be removed, replaced, or (for a
//     new comply-able license) handled deliberately here.
//
// EXCLUSIONS: packages verified NOT present in this app's shipped bundle (see
// EXCLUDE below). peasant is Next output:'export' (client-only static), so the
// server-side image optimizer stack (sharp + its @img/* native libvips binaries,
// one of which is LGPL) never lands in web/out and is excluded from both the
// notices and the guard. Verified: no @img//sharp@/libvips markers in web/out.
//
// Requirements: run from web/ with node_modules installed (pnpm install).
import { execFileSync } from 'node:child_process';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

// --- policy (documented, minimal) -----------------------------------------
// Packages verified absent from peasant's client-only static export.
const EXCLUDE = (name) => name === 'sharp' || name.startsWith('@img/');

const PERMISSIVE = new Set([
  'MIT', 'ISC', '0BSD', 'BSD-2-Clause', 'BSD-3-Clause', 'Apache-2.0',
  'CC0-1.0', 'CC-BY-4.0', 'Unlicense', 'Zlib', 'BlueOak-1.0.0',
  'Python-2.0', 'WTFPL', 'Artistic-2.0',
]);
// Weak copyleft we comply with via a source-availability offer (not shipped by
// peasant today; used by village's standalone image).
const WEAK_COPYLEFT = new Set([
  'LGPL-2.0-only', 'LGPL-2.0-or-later', 'LGPL-2.1-only', 'LGPL-2.1-or-later',
  'LGPL-3.0-only', 'LGPL-3.0-or-later',
]);
const STRONG_COPYLEFT = new Set([
  'GPL-2.0-only', 'GPL-2.0-or-later', 'GPL-3.0-only', 'GPL-3.0-or-later',
  'AGPL-3.0-only', 'AGPL-3.0-or-later', 'SSPL-1.0',
]);

// Classify one dependency's SPDX license string into a bucket. Handles simple
// SPDX expressions: "A OR B" takes the most permissive branch; "A AND B" takes
// the most restrictive.
export function classify(spdx) {
  if (!spdx || spdx === 'UNLICENSED' || /SEE LICENSE/i.test(spdx)) return 'UNKNOWN';
  const expr = spdx.replace(/[()]/g, ' ').trim();
  if (/\bOR\b/.test(expr)) {
    const parts = expr.split(/\bOR\b/).map((p) => classify(p.trim()));
    for (const rank of ['PERMISSIVE', 'WEAK', 'STRONG', 'UNKNOWN']) {
      if (parts.includes(rank)) return rank;
    }
  }
  if (/\bAND\b/.test(expr)) {
    const parts = expr.split(/\bAND\b/).map((p) => classify(p.trim()));
    for (const rank of ['UNKNOWN', 'STRONG', 'WEAK', 'PERMISSIVE']) {
      if (parts.includes(rank)) return rank;
    }
  }
  const t = expr.replace(/\s+WITH\s+.*/i, '').trim();
  if (PERMISSIVE.has(t)) return 'PERMISSIVE';
  if (WEAK_COPYLEFT.has(t)) return 'WEAK';
  if (STRONG_COPYLEFT.has(t)) return 'STRONG';
  return 'UNKNOWN';
}

const LICENSE_FILE = /^(LICENSE|LICENCE|COPYING|NOTICE)/i;
function readLicenseText(dir) {
  let names;
  try {
    names = readdirSync(dir).filter(
      (n) => LICENSE_FILE.test(n) && !/\.(go|json|js|mjs|ts)$/i.test(n),
    ).sort();
  } catch {
    return [];
  }
  const out = [];
  for (const n of names) {
    try {
      const p = join(dir, n);
      if (statSync(p).isFile()) out.push({ name: n, text: readFileSync(p, 'utf8') });
    } catch { /* skip unreadable */ }
  }
  return out;
}

// --- main -------------------------------------------------------------------
function main() {
// --- enumerate --------------------------------------------------------------
const raw = execFileSync('pnpm', ['licenses', 'list', '--prod', '--json'], {
  encoding: 'utf8', maxBuffer: 64 * 1024 * 1024,
});
const grouped = JSON.parse(raw);

const pkgs = [];
for (const entries of Object.values(grouped)) {
  for (const e of entries) {
    if (EXCLUDE(e.name)) continue;
    if (e.name === 'peasant-web') continue; // the app itself
    pkgs.push(e);
  }
}
pkgs.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));

// --- classify + fail closed -------------------------------------------------
const blocked = [];
for (const p of pkgs) {
  p.bucket = classify(p.license);
  if (p.bucket === 'STRONG' || p.bucket === 'UNKNOWN') {
    blocked.push(`${p.name}@${p.versions.join(',')}  license=${p.license || '(none)'}  [${p.bucket}]`);
  }
}
if (blocked.length) {
  process.stderr.write(
    'gen-web-notices: disallowed or unclassifiable license in the bundled npm set.\n' +
    'Remove/replace the dependency, or (for a new comply-able license) handle it in gen-web-notices.mjs:\n  ' +
    blocked.join('\n  ') + '\n',
  );
  process.exit(1);
}

// --- render -----------------------------------------------------------------
const RULE = '='.repeat(80);
const out = [];
out.push('THIRD-PARTY SOFTWARE NOTICES — WEB DASHBOARD (npm)');
out.push('');
out.push('The embedded web dashboard (web/out) bundles the npm packages below.');
out.push('This reproduces their license texts as their licenses require. It lists');
out.push("the web app's production dependency closure, a superset of what the client");
out.push('bundle includes. Generated from web/pnpm-lock.yaml; do not edit by hand');
out.push('(run: make third-party-notices).');
out.push('');

const weak = pkgs.filter((p) => p.bucket === 'WEAK');

for (const p of pkgs) {
  const dir = p.paths[0];
  const files = dir ? readLicenseText(dir) : [];
  if (files.length === 0) {
    // No license file shipped: fall back to the declared SPDX id + author.
    out.push(RULE);
    out.push(`Package: ${p.name}@${p.versions.join(',')}`);
    out.push(`License: ${p.license}${p.author ? '  (' + p.author + ')' : ''}`);
    out.push('No license file is distributed with this package; the license above');
    out.push('is the identifier declared in its package.json.');
    out.push(RULE);
    out.push('');
    continue;
  }
  for (const f of files) {
    out.push(RULE);
    out.push(`Package: ${p.name}@${p.versions.join(',')}`);
    out.push(`File:    ${f.name}`);
    out.push(RULE);
    out.push(f.text.replace(/\s+$/, ''));
    out.push('');
  }
  // Carry a bundled @peasant-labs/* package's own third-party notices (e.g.
  // fairtrade reproduces the Pi brand-mark attribution) — redistributed too.
  if (p.name.startsWith('@peasant-labs/') && dir) {
    for (const n of ['THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES']) {
      try {
        const text = readFileSync(join(dir, n), 'utf8');
        out.push(RULE);
        out.push(`Package: ${p.name}@${p.versions.join(',')} — bundled ${n}`);
        out.push(RULE);
        out.push(text.replace(/\s+$/, ''));
        out.push('');
        break;
      } catch { /* none */ }
    }
  }
}

if (weak.length) {
  out.push(RULE);
  out.push('COPYLEFT COMPONENTS — SOURCE AVAILABILITY');
  out.push(RULE);
  out.push('The components below are covered by a weak-copyleft (LGPL) license.');
  out.push('They are separable, dynamically-loaded libraries; you may replace them');
  out.push('with a modified version. Their license text appears above. The');
  out.push('corresponding source is available from each project, and Peasant Labs');
  out.push('will provide the exact source for the bundled version on request at');
  out.push('admin@peasantlabs.org.');
  out.push('');
  for (const p of weak) {
    out.push(`- ${p.name}@${p.versions.join(',')} — ${p.license} — source: ${p.homepage || '(see package repository)'}`);
  }
  out.push('');
}

process.stdout.write(out.join('\n'));
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main();
}
