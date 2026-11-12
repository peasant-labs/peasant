import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, readdirSync, existsSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { join, relative } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { parse } from 'yaml';
import { isolatedServerSandbox } from '../visual/lib/isolated-sandbox.mjs';
import { directoryNamedBy, ownedServerSandbox } from '../visual/lib/owned-server-sandbox.mjs';
import { startOwnedServer } from '../visual/lib/owned-server-start.mjs';

const f = parse(readFileSync(new URL('./testdata/server-sandbox.yaml', import.meta.url), 'utf8'));
const WEB = fileURLToPath(new URL('../..', import.meta.url));
assert.deepEqual(new Set([...f.cases.map(c => c.name), 'every-script-that-starts-the-binary-is-sandboxed-or-exempt']), new Set(f.requiredNames));

// The child programs the cases launch; each reads its case's values from its arguments.
const FAKE_SERVER = fileURLToPath(new URL('./lib/fake-server.mjs', import.meta.url));
const OWNED_SANDBOX_SCRIPT = fileURLToPath(new URL('./lib/owned-sandbox-script.mjs', import.meta.url));

// Starts `program` with `args` and kills it when the case ends, if it is still running.
function launch(cleanups, program, args, { ipc = false } = {}) {
  const child = spawn(process.execPath, [program, ...args.map(String)], { stdio: ['ignore', 'ignore', 'pipe', ...(ipc ? ['ipc'] : [])] });
  cleanups.push(() => { if (!gone(child)) child.kill('SIGKILL'); });
  return child;
}

// Runs the sandbox script to its end in the case's temporary directory: its status, its output and its errors.
function runOwnedSandboxScript(root, args) {
  try {
    return { status: 0, stdout: execFileSync(process.execPath, [OWNED_SANDBOX_SCRIPT, ...args.map(String)], { env: { ...process.env, TMPDIR: root }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }), stderr: '' };
  } catch (error) {
    return { status: error.status, stdout: error.stdout, stderr: error.stderr };
  }
}

// The sandbox script's arguments for the directories a case names.
const namedArgs = (named) => [
  ...(named.configDir === undefined ? [] : ['--config-dir', named.configDir]),
  ...(named.dataDir === undefined ? [] : ['--data-dir', named.dataDir]),
];

const freePort = () => new Promise((done) => { const s = createServer(); s.listen(0, 'localhost', () => { const { port } = s.address(); s.close(() => done(port)); }); });
const gone = (child) => child.exitCode !== null || child.signalCode !== null;

const modes = {
  layout(row, root) {
    const other = join(root, 'other'), sandboxRoot = join(root, 'sandbox');
    const place = (path) => (typeof path !== 'string' || path === '' ? path : path.startsWith('$OTHER') ? path.replace('$OTHER', other) : join(sandboxRoot, path));
    const named = Object.fromEntries(Object.entries(row.named).map(([key, path]) => [key, place(path)]));
    const sandbox = isolatedServerSandbox(sandboxRoot, named, row.inherited ?? {});
    const want = Object.fromEntries(Object.entries(row.expect).map(([key, value]) => [key, place(value)]));
    assert.deepEqual({ configDir: sandbox.configDir, dataDir: sandbox.dataDir, stateDir: sandbox.stateDir, home: sandbox.env.HOME, cache: sandbox.env.XDG_CACHE_HOME }, want);
    // The flags and the environment name the same three directories, so neither can fall back to the person's.
    assert.deepEqual([...sandbox.args], ['--config-dir', want.configDir, '--data-dir', want.dataDir, '--state-dir', want.stateDir]);
    assert.deepEqual(
      { config: sandbox.env.XDG_CONFIG_HOME, data: sandbox.env.XDG_DATA_HOME, state: sandbox.env.XDG_STATE_HOME },
      { config: want.configDir, data: want.dataDir, state: want.stateDir },
    );
    // Every place the binary can reach is under the sandbox root or is one the caller named.
    const allowed = [sandboxRoot, ...Object.values(named).filter(Boolean)];
    for (const key of ['HOME', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_STATE_HOME', 'XDG_CACHE_HOME']) {
      assert.ok(allowed.some((prefix) => sandbox.env[key] === prefix || sandbox.env[key].startsWith(`${prefix}/`)), `${key} is ${sandbox.env[key]}, outside the sandbox`);
    }
    for (const [key, value] of Object.entries(row.kept ?? {})) assert.equal(sandbox.env[key], value, `${key} is passed on unchanged`);
    for (const key of row.dropped ?? []) assert.equal(key in sandbox.env, false, `${key} must not reach the binary`);
  },
  'refused-layout'(row, root) {
    assert.throws(() => isolatedServerSandbox(row.root.replace('$ROOT', root), row.named, {}), (error) => error.message.includes(row.error));
  },
  owned(row, root) {
    const other = join(root, 'other');
    const named = Object.fromEntries(Object.entries(row.named ?? {}).map(([key, path]) => [key, path.replace('$OTHER', other)]));
    for (const directory of Object.values(named)) { mkdirSync(directory, { recursive: true }); writeFileSync(join(directory, 'marker'), 'theirs'); }
    for (const end of row.ends) {
      const how = typeof end === 'number' ? ['--end', 'exit', '--status', end] : ['--end', end];
      const wantStatus = typeof end === 'number' ? end : end === 'uncaught-error' ? 1 : 0;
      const { status, stdout, stderr } = runOwnedSandboxScript(root, ['--name', row.script, ...namedArgs(named), ...how]);
      assert.equal(status, wantStatus, `the script that ends by ${end}: ${stderr}`);
      const said = JSON.parse(stdout);
      assert.equal(said.existed, true);
      assert.ok(said.root.startsWith(join(root, `peasant-${row.script}-`)), `${said.root} is in the temporary directory and named for the script`);
      assert.equal(said.home, join(said.root, 'home'));
      assert.equal(said.dataDir, named.dataDir ?? join(said.root, 'data'));
      assert.equal(said.configDir, named.configDir ?? join(said.root, 'config'));
      assert.equal(existsSync(said.root), false, `the sandbox is gone after the script ends by ${end}`);
      for (const directory of Object.values(named)) assert.equal(readFileSync(join(directory, 'marker'), 'utf8'), 'theirs', `${directory} is the caller's and is kept after the script ends by ${end}`);
      if (named.dataDir) assert.equal(existsSync(join(named.dataDir, 'peasant', 'peasant.db')), true, 'what the server wrote in the named data directory is kept');
    }
  },
  'owned-refused'(row, root) {
    const { status, stdout, stderr } = runOwnedSandboxScript(root, ['--name', row.script, ...namedArgs(row.named), '--end', 'last-line']);
    assert.equal(status, 1, `the refused layout ends the script with an error: ${stdout}`);
    assert.ok(stderr.includes(row.error), stderr);
    assert.deepEqual(readdirSync(root), [], 'no root is left in the temporary directory');
  },
  'owned-name'(row) {
    for (const name of row.names) assert.throws(() => ownedServerSandbox(name), (error) => error.message.includes(row.error), JSON.stringify(name));
  },
  'named-directory'(row, root) {
    for (const value of row.values ?? [row.value]) {
      const got = directoryNamedBy('PEASANT_EXAMPLE_DIR', value === null ? {} : { PEASANT_EXAMPLE_DIR: value }, root);
      assert.equal(got ?? null, row.expect === null ? null : row.expect.replace('$CWD', root), JSON.stringify(value));
    }
  },
  async start(row, root, cleanups) {
    const port = await freePort();
    if (row.occupied) {
      const other = launch(cleanups, FAKE_SERVER, ['answers', '--port', port]);
      for (let i = 0; i < 100 && !(await fetch(`http://localhost:${port}/api/v1/health`).catch(() => null))?.ok; i++) await new Promise((done) => setTimeout(done, 50));
      assert.equal(gone(other), false, 'the other process holds the port');
    }
    // What the script's own start launches, by the case's `server`.
    const serverArgs = {
      answers: () => ['answers', '--port', port],
      exits: () => ['exits', '--status', row.status],
      silent: () => ['silent'],
      'answers-elsewhere': () => ['exits-when-told', '--status', row.status, '--after-ms', row.exitAfterAnswerMs],
    }[row.server];
    assert.ok(serverArgs, `case ${row.name} has the unknown server ${row.server}`);
    let started = null;
    let answeredElsewhere = false;
    const options = { port, portVariable: 'PEASANT_EXAMPLE_PORT', start: () => {
      if (row.server === 'answers-elsewhere') {
        const other = launch(cleanups, FAKE_SERVER, ['answers-and-notifies', '--port', port], { ipc: true });
        other.once('message', () => { answeredElsewhere = true; started.send('exit'); });
      }
      return (started = launch(cleanups, FAKE_SERVER, serverArgs(), { ipc: row.server === 'answers-elsewhere' }));
    }, ...(row.withinMs ? { withinMs: row.withinMs } : {}) };
    const before = Date.now();
    if (row.error) {
      await assert.rejects(startOwnedServer(options), (error) => [row.error, String(port), ...(row.errorNames ?? [])].every((part) => error.message.includes(part)) || assert.fail(error.message));
      if (row.endsWithinMs) assert.ok(Date.now() - before < row.endsWithinMs, `the wait ended after ${Date.now() - before} ms, long before the ${row.withinMs} ms allowed`);
    } else {
      const server = await startOwnedServer(options);
      assert.equal(server, started);
      assert.equal(gone(server), false, 'the returned server is running');
      assert.equal((await fetch(`http://localhost:${port}/api/v1/health`)).ok, true);
    }
    assert.equal(started !== null, row.started, row.started ? 'the script started its server' : 'nothing was started on an occupied port');
    if (row.server === 'answers-elsewhere') assert.equal(answeredElsewhere, true, 'the other process answered health before the started server exited');
    if (row.stopped) { await new Promise((done) => (gone(started) ? done() : started.once('exit', done))); assert.equal(started.signalCode, 'SIGKILL'); }
  },
};

for (const row of f.cases) test(row.name, { timeout: 30000 }, async (t) => {
  const root = mkdtempSync(join(tmpdir(), 'peasant-sandbox-test-'));
  const cleanups = [() => rmSync(root, { recursive: true, force: true })];
  t.after(() => { for (const cleanup of cleanups.reverse()) cleanup(); });
  assert.ok(modes[row.mode], `case ${row.name} has the unknown mode ${row.mode}`);
  await modes[row.mode](row, root, cleanups);
});

test('every-script-that-starts-the-binary-is-sandboxed-or-exempt', () => {
  const startsTheBinary = /(['"])web\1,\s*(['"])start\2/;
  const scripts = (directory) => readdirSync(directory, { withFileTypes: true }).flatMap((entry) => (
    entry.name === 'node_modules' ? [] : entry.isDirectory() ? scripts(join(directory, entry.name)) : /\.(mjs|js|ts)$/.test(entry.name) ? [join(directory, entry.name)] : []
  ));
  const starting = scripts(join(WEB, 'scripts')).filter((path) => startsTheBinary.test(readFileSync(path, 'utf8'))).map((path) => relative(WEB, path));
  for (const row of f.scripts.exempt) assert.ok(typeof row.reason === 'string' && row.reason.length > 20, `${row.path} is exempt without a reason`);
  assert.deepEqual(
    new Set(starting),
    new Set([...f.scripts.required, ...f.scripts.exempt.map((row) => row.path)]),
    'A script under web/scripts that starts `peasant web start` must be listed in testdata/server-sandbox.yaml: under scripts.required once it uses ownedServerSandbox and startOwnedServer, or under scripts.exempt with what it isolates instead',
  );
  for (const path of f.scripts.required) {
    const source = readFileSync(join(WEB, path), 'utf8');
    assert.match(source, /\bownedServerSandbox\(/, `${path} makes its sandbox with ownedServerSandbox`);
    assert.match(source, /\bstartOwnedServer\(/, `${path} starts its server through startOwnedServer`);
    const spawns = source.split('\n').filter((line) => startsTheBinary.test(line));
    assert.ok(spawns.length > 0, `${path}: the spawn must stay on one source line with the web start arguments, ...sandbox.args and env: sandbox.env; restore the one-line call so the sandbox check can verify it`);
    for (const line of spawns) {
      assert.ok(/\bspawn\(/.test(line) && line.includes('...sandbox.args') && /\benv: sandbox\.env\b/.test(line), `${path}: the spawn must stay on one source line with the web start arguments, ...sandbox.args and env: sandbox.env so the sandbox check can verify it: ${line.trim()}`);
    }
  }
});
