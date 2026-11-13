/* The transcript-segment seeder's data-directory boundary.
 *
 * Each case runs the real seeder, exactly as the capture does (`go run
 * ./web/scripts/visual/seed-transcript-store.go -data-dir <dir>`), in a throwaway root under the
 * test's temporary directory. A refused case must leave every byte, entry and mode it found
 * unchanged: the sentinel store is never opened, migrated or written. A fresh case must seed the
 * expected session and entries, and the test reads them back through SQLite from the seeded file,
 * which is what the capture's server reads.
 *
 * This is a process-boundary test because the bug it protects against is at the boundary: the
 * program is separate (`//go:build ignore`) and the capture launches it by path, so no in-process
 * export can be the subject without inventing one. The harness is one test file and one YAML
 * fixture; it starts only short-lived `go run` processes in its own temporary directory, and the
 * Go build cache makes the whole file a few seconds.
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

const f = parse(readFileSync(new URL('./testdata/seed-transcript-store.yaml', import.meta.url), 'utf8'));
assert.deepEqual(new Set(f.cases.map((row) => row.name)), new Set(f.requiredNames));

const REPO = fileURLToPath(new URL('../../..', import.meta.url));
const SEEDER = './web/scripts/visual/seed-transcript-store.go';
// A relative argument this test never expects to be created, even when the guard is mutated.
const RELATIVE_ARGUMENT = 'relative-data-dir-that-must-not-be-created';

// Runs the seeder as the capture does and reports its status, output and errors.
function runSeeder(dataDir) {
  try {
    const stdout = execFileSync('go', ['run', SEEDER, '-data-dir', dataDir], { cwd: REPO, env: { ...process.env, GOWORK: 'off' }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 120000 });
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

function populate(mode, root, dataDir) {
  if (mode === 'existing-store') {
    sentinelDatabase(join(dataDir, 'peasant', 'peasant.db'));
    mkdirSync(join(dataDir, 'peasant-sync'));
    writeFileSync(join(dataDir, 'peasant-sync', 'kept.jsonl'), 'kept-transcript-bytes');
    return;
  }
  if (mode === 'empty-dir') { mkdirSync(dataDir); return; }
  if (mode === 'symlink') {
    const realStore = join(root, 'real-store');
    sentinelDatabase(join(realStore, 'peasant', 'peasant.db'));
    symlinkSync(realStore, dataDir);
    return;
  }
  if (mode === 'file') { writeFileSync(dataDir, 'sentinel-file-bytes'); return; }
  assert.ok(['relative', 'missing-parent', 'fresh'].includes(mode), `unknown mode ${mode}`);
}

for (const row of f.cases) test(row.name, { timeout: 180000 }, async (t) => {
  const root = mkdtempSync(join(tmpdir(), 'peasant-seed-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const dataDir = join(root, 'data');
  populate(row.mode, root, dataDir);
  const before = snapshot(root);
  const argument = row.mode === 'relative' ? RELATIVE_ARGUMENT : row.mode === 'missing-parent' ? join(root, 'absent', 'data') : dataDir;
  const result = runSeeder(argument);
  if (row.refused === false) {
    assert.equal(result.status, 0, `the seeder seeds a fresh directory: ${result.stderr}`);
    const said = JSON.parse(result.stdout);
    assert.equal(said.dataDir, dataDir);
    assert.equal(said.turns, row.turns);
    assert.equal(said.conversationTurns, row.conversationTurns);
    const dbPath = join(dataDir, 'peasant', 'peasant.db');
    assert.ok(existsSync(dbPath), 'the seeded store stays in the data directory for the capture server');
    const db = new DatabaseSync(dbPath, { readOnly: true });
    try {
      assert.equal(db.prepare('select count(*) as n from sessions').get().n, row.sessions);
      assert.equal(db.prepare('select count(*) as n from session_entries').get().n, row.entries);
      assert.equal(db.prepare('select count(*) as n from session_entries where session_id = ?').get(said.sessionId).n, row.entries);
    } finally { db.close(); }
    // The seeder created its data directory and nothing beside it.
    assert.deepEqual(readdirSync(root), ['data']);
    return;
  }
  assert.equal(result.status, 1, `the seeder refuses the ${row.mode} path: ${result.stdout}${result.stderr}`);
  assert.deepEqual(snapshot(root), before, 'everything the invocation found is unchanged');
  assert.match(result.stderr, new RegExp(row.error));
  assert.equal(existsSync(join(REPO, RELATIVE_ARGUMENT)), false, 'a relative argument creates nothing in the checkout');
});
