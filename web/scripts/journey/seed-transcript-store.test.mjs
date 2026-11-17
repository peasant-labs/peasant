/* The transcript-segment seeder's data-directory boundary.
 *
 * The seeder creates the data directory it writes to and accepts no path, so a real store cannot
 * be named. Each case runs the real program, exactly as the capture does (`go run
 * ./web/scripts/visual/seed-transcript-store.go`), in a throwaway root under the test's temporary
 * directory. A supplied path must be refused before anything is created or opened, and every byte,
 * entry and mode the invocation found must be unchanged. A run with no path must create a fresh
 * private directory under TMPDIR, seed it, and print its path; the test reads the seeded session
 * and entries back through SQLite, which is what the capture's server reads.
 *
 * This is a process-boundary test because the bug it protects against is at the boundary: the
 * program is separate (`//go:build ignore`) and the capture launches it by path, so no in-process
 * export can be the subject without inventing one. The harness is one test file and one YAML
 * fixture; it starts only short-lived `go run` processes in its own temporary directory, and the
 * Go build cache keeps the whole file to a few seconds.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { DatabaseSync } from 'node:sqlite';
import { parse } from 'yaml';

const FIXTURE = new URL('./testdata/seed-transcript-store.yaml', import.meta.url);
const f = parse(readFileSync(FIXTURE, 'utf8'));
assert.deepEqual(new Set(f.cases.map((row) => row.name)), new Set(f.requiredNames));

const REPO = fileURLToPath(new URL('../../..', import.meta.url));
const SEEDER = './web/scripts/visual/seed-transcript-store.go';
const PREFIX = 'peasant-transcript-segments-';
// A relative argument this test never expects to be created, even when the refusal is mutated.
const RELATIVE_ARGUMENT = 'relative-data-dir-that-must-not-be-created';
const Mode = Object.freeze({ ExistingStore: 'existing-store', EmptyDir: 'empty-dir', Symlink: 'symlink', File: 'file', Relative: 'relative', Fresh: 'fresh' });

// Runs the seeder as the capture does and reports its status, output and errors.
function runSeeder(args, environment = {}) {
  try {
    const stdout = execFileSync('go', ['run', SEEDER, ...args], { cwd: REPO, env: { ...process.env, GOWORK: 'off', ...environment }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 120000 });
    return { status: 0, stdout, stderr: '' };
  } catch (error) {
    return { status: error.status ?? -1, stdout: error.stdout ?? '', stderr: error.stderr ?? String(error) };
  }
}

// Every entry under root with what an invocation must not change: a file's bytes, size and
// modification time, a directory's mode, and a symlink's target (never followed).
function snapshot(root) {
  const entries = [];
  const walk = (directory, prefix) => {
    for (const name of readdirSync(directory).sort()) {
      const path = join(directory, name), relative = `${prefix}${name}`, stat = lstatSync(path);
      if (stat.isSymbolicLink()) { entries.push([relative, 'symlink', readlinkSync(path), stat.mode, stat.mtimeMs]); continue; }
      if (stat.isDirectory()) { entries.push([relative, 'directory', stat.mode, stat.mtimeMs]); walk(path, `${relative}/`); continue; }
      entries.push([relative, 'file', stat.mode, stat.mtimeMs, stat.size, createHash('sha256').update(readFileSync(path)).digest('hex')]);
    }
  };
  walk(root, '');
  return entries;
}

// A real SQLite database holding unrelated data, so a regression that opens and migrates an
// existing store changes bytes this test can see.
function sentinelDatabase(path) {
  mkdirSync(dirname(path), { recursive: true });
  const db = new DatabaseSync(path);
  try {
    db.exec("create table sentinel(value text); insert into sentinel values ('kept')");
  } finally { db.close(); }
}

function populate(mode, root, supplied) {
  if (mode === Mode.ExistingStore) {
    sentinelDatabase(join(supplied, 'peasant', 'peasant.db'));
    mkdirSync(join(supplied, 'peasant-sync'));
    writeFileSync(join(supplied, 'peasant-sync', 'kept.jsonl'), 'kept-transcript-bytes');
    return;
  }
  if (mode === Mode.EmptyDir) { mkdirSync(supplied); return; }
  if (mode === Mode.Symlink) {
    const realStore = join(root, 'real-store');
    sentinelDatabase(join(realStore, 'peasant', 'peasant.db'));
    symlinkSync(realStore, supplied);
    return;
  }
  if (mode === Mode.File) { writeFileSync(supplied, 'sentinel-file-bytes'); return; }
  assert.ok([Mode.Relative, Mode.Fresh].includes(mode), `unknown mode ${mode}`);
}

for (const row of f.cases) test(row.name, { timeout: 180000 }, async (t) => {
  const root = mkdtempSync(join(tmpdir(), 'peasant-seed-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  if (row.refused === false) {
    const result = runSeeder([], { TMPDIR: root });
    assert.equal(result.status, 0, `the seeder seeds a fresh directory: ${result.stderr}`);
    const said = JSON.parse(result.stdout);
    assert.ok(said.dataDir.startsWith(join(root, PREFIX)), `the created directory is under TMPDIR: ${said.dataDir}`);
    const stat = lstatSync(said.dataDir);
    assert.ok(stat.isDirectory(), 'the printed path is the directory the run created');
    assert.equal(stat.mode & 0o777, 0o700, 'the created directory is private to this run');
    assert.equal(said.turns, row.turns);
    assert.equal(said.conversationTurns, row.conversationTurns);
    const dbPath = join(said.dataDir, 'peasant', 'peasant.db');
    assert.ok(existsSync(dbPath), 'the seeded store is in the directory the seeder printed');
    const db = new DatabaseSync(dbPath, { readOnly: true });
    try {
      assert.equal(db.prepare('select count(*) as n from sessions').get().n, row.sessions);
      assert.equal(db.prepare('select count(*) as n from session_entries').get().n, row.entries);
      assert.equal(db.prepare('select count(*) as n from session_entries where session_id = ?').get(said.sessionId).n, row.entries);
    } finally { db.close(); }
    assert.deepEqual(readdirSync(root), [said.dataDir.slice(root.length + 1)], 'the run created only its own data directory');
    return;
  }
  const supplied = row.mode === Mode.Relative ? RELATIVE_ARGUMENT : join(root, 'supplied-store');
  populate(row.mode, root, supplied);
  const before = snapshot(root);
  const result = runSeeder(row.form === 'positional' ? [supplied] : ['-data-dir', supplied]);
  assert.equal(result.status, 1, `the seeder refuses a supplied path: ${result.stdout}${result.stderr}`);
  assert.deepEqual(snapshot(root), before, 'everything the invocation found is unchanged');
  assert.match(result.stderr, /not accepted/);
  assert.equal(existsSync(join(REPO, RELATIVE_ARGUMENT)), false, 'a relative argument creates nothing in the checkout');
});
