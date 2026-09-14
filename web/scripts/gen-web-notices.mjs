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
// Packages absent from peasant's client-only static export (output:'export'
// ships no native binaries): the server-side image stack (sharp + its @img/*
// natives, one LGPL) and platform-specific native binaries such as @next/swc-*
// (Next's build-time SWC compiler). Excluding every platform-native package also
// keeps this file ARCHITECTURE-INDEPENDENT — pnpm licenses list reports only the
// installed platform's optional natives, so without this the committed file
// (generated on x64) would not match a CI regen on arm64.
const PLATFORM_NATIVE = /-(?:linux|darwin|win32|freebsd|android)-(?:x64|arm64|arm|ia32|s390x|ppc64le?|riscv64)(?:-\w+)?$/;
const EXCLUDE = (name) =>
  name === 'sharp' || name.startsWith('@img/') || PLATFORM_NATIVE.test(name);

// The app's own package is not a third-party notice. Derived from package.json
// so this generator ports to other web apps (e.g. the village frontend) without
// editing a hardcoded name.
const SELF = JSON.parse(
  readFileSync(new URL('../package.json', import.meta.url), 'utf8'),
).name;

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

// Classify one bare SPDX license id (ignoring any "WITH <exception>").
function classifyId(id) {
  const t = id.replace(/\s+WITH\s+.*/i, '').trim();
  if (PERMISSIVE.has(t)) return 'PERMISSIVE';
  if (WEAK_COPYLEFT.has(t)) return 'WEAK';
  if (STRONG_COPYLEFT.has(t)) return 'STRONG';
  return 'UNKNOWN';
}

// Restrictiveness order; higher = more restrictive.
const RANK = { PERMISSIVE: 0, WEAK: 1, STRONG: 2, UNKNOWN: 3 };
const mostPermissive = (a, b) => (RANK[b] < RANK[a] ? b : a); // OR: may choose the least restrictive
const mostRestrictive = (a, b) => (RANK[b] > RANK[a] ? b : a); // AND: must satisfy all

// Classify an SPDX license expression into a bucket, respecting parentheses and
// SPDX precedence (AND binds tighter than OR). A recursive-descent parse — NOT a
// global paren-strip — so a paren'd OR inside an AND (e.g.
// "GPL-3.0-only AND (MIT OR Apache-2.0)") stays STRONG. Anything that does not
// parse cleanly falls through to UNKNOWN, so the guard fails closed.
export function classify(spdx) {
  if (!spdx || spdx === 'UNLICENSED' || /SEE LICENSE/i.test(spdx)) return 'UNKNOWN';
  // Tokenize: parens are their own tokens; AND/OR are operators; any other run
  // of words is one license id ("Apache-2.0 WITH LLVM-exception").
  const words = spdx.replace(/([()])/g, ' $1 ').trim().split(/\s+/);
  const toks = [];
  let cur = [];
  const flush = () => { if (cur.length) { toks.push({ t: 'id', v: cur.join(' ') }); cur = []; } };
  for (const w of words) {
    if (w === '(' || w === ')') { flush(); toks.push({ t: w }); }
    else if (w === 'AND' || w === 'OR') { flush(); toks.push({ t: w }); }
    else cur.push(w);
  }
  flush();

  let pos = 0;
  const peek = () => toks[pos];
  const parseOr = () => {
    let v = parseAnd();
    while (peek() && peek().t === 'OR') { pos += 1; v = mostPermissive(v, parseAnd()); }
    return v;
  };
  function parseAnd() {
    let v = parseAtom();
    while (peek() && peek().t === 'AND') { pos += 1; v = mostRestrictive(v, parseAtom()); }
    return v;
  }
  function parseAtom() {
    const tk = peek();
    if (!tk) return 'UNKNOWN';
    if (tk.t === '(') {
      pos += 1;
      const v = parseOr();
      if (peek() && peek().t === ')') { pos += 1; return v; }
      return 'UNKNOWN'; // unbalanced -> fail closed
    }
    if (tk.t === 'id') { pos += 1; return classifyId(tk.v); }
    return 'UNKNOWN'; // stray operator -> fail closed
  }

  const result = parseOr();
  return pos === toks.length ? result : 'UNKNOWN'; // trailing tokens -> fail closed
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
    if (e.name === SELF) continue; // the app itself
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
// NOTE (porting to another web app, e.g. the village frontend): SELF (the app's
// own package) is read from package.json, and the sharp/@img EXCLUDE assumes a
// client-only static export. An app that ships server code (a standalone image)
// bundles sharp and must NOT exclude it — its LGPL then ships and is handled by
// the WEAK / source-availability path. The header lines below say "web/out" and
// "browser"; adjust them for a server bundle.
const RULE = '='.repeat(80);
const out = [];
out.push('<!-- BEGIN GENERATED npm-web-notices -->');
out.push('THIRD-PARTY SOFTWARE NOTICES — WEB DASHBOARD (npm)');
out.push('');
out.push('The embedded web dashboard (web/out) bundles the npm packages below.');
out.push('This reproduces their license texts as their licenses require. The list');
out.push('includes every production dependency of the web app — more packages than');
out.push('the browser loads, which is safe for attribution. Generated from the pnpm');
out.push('lockfile; do not edit by hand (run: make third-party-notices).');
out.push('');

const weak = pkgs.filter((p) => p.bucket === 'WEAK');

for (const p of pkgs) {
  // Collect license files across ALL installed paths (a multi-version package
  // installs more than one), de-duplicated by text so distinct notices are all
  // reproduced rather than only the first version's.
  const seenText = new Set();
  const files = [];
  for (const dir of p.paths) {
    for (const f of readLicenseText(dir)) {
      if (!seenText.has(f.text)) { seenText.add(f.text); files.push(f); }
    }
  }
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
  if (p.name.startsWith('@peasant-labs/')) {
    let carried = false;
    for (const dir of p.paths) {
      for (const n of ['THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES']) {
        try {
          const text = readFileSync(join(dir, n), 'utf8');
          out.push(RULE);
          out.push(`Package: ${p.name}@${p.versions.join(',')} — bundled ${n}`);
          out.push(RULE);
          out.push(text.replace(/\s+$/, ''));
          out.push('');
          carried = true;
          break;
        } catch { /* none */ }
      }
      if (carried) break;
    }
  }
}

if (weak.length) {
  out.push(RULE);
  out.push('COPYLEFT COMPONENTS — WRITTEN OFFER OF SOURCE');
  out.push(RULE);
  out.push('The components below are covered by a weak-copyleft (LGPL) license,');
  out.push('reproduced above. Each is a separable, dynamically-loaded library that');
  out.push('you may replace with your own modified version.');
  out.push('');
  out.push('WRITTEN OFFER: For at least three (3) years from the date this software');
  out.push('was distributed, Peasant Labs will give any third party, on request at');
  out.push('admin@peasantlabs.org, the complete corresponding source code of each');
  out.push('component below (for the exact version distributed), for no more than the');
  out.push('cost of physically performing the distribution. The source is also');
  out.push('available from each project at the location listed.');
  out.push('');
  for (const p of weak) {
    out.push(`- ${p.name}@${p.versions.join(',')} — ${p.license} — source: ${p.homepage || '(see package repository)'}`);
  }
  out.push('');
}

out.push('<!-- END GENERATED npm-web-notices -->');

process.stdout.write(out.join('\n'));
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main();
}
