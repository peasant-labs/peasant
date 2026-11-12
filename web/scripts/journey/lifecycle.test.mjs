import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, existsSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { parse } from 'yaml';
import { acquireLifecycle, bounded, ownedGroups, start } from './lib/owned-lifecycle.mjs';
import { isolatedServerSandbox } from './lib/isolated-sandbox.mjs';
import { ownedServerSandbox } from './lib/owned-server-sandbox.mjs';

const f = parse(readFileSync(new URL('./testdata/lifecycle.yaml', import.meta.url), 'utf8'));
assert.deepEqual(new Set(f.cases.map(c => c.name)), new Set(f.requiredNames));
for (const row of f.cases) test(row.name, { timeout: 20000 }, async t => {
  const root = mkdtempSync(join(tmpdir(), 'peasant-journey-test-'));
  const cleanups = [() => rmSync(root, { recursive: true, force: true })];
  t.after(async () => { for (const cleanup of cleanups.reverse()) await cleanup(); });
  if (row.mode === 'sandbox-relative') {
    for (const [sandboxRoot, named] of [['sandbox', {}], [root, { configDir: 'config' }], [root, { dataDir: './data' }]]) assert.throws(() => isolatedServerSandbox(sandboxRoot, named), new RegExp(row.error));
    return;
  }
  if (row.mode === 'sandbox') {
    const other = join(root, 'other'), sandboxRoot = join(root, 'sandbox');
    const place = (path) => (typeof path !== 'string' ? path : path.startsWith('$OTHER') ? path.replace('$OTHER', other) : join(sandboxRoot, path));
    const named = Object.fromEntries(Object.entries(row.named).map(([key, path]) => [key, place(path)]));
    const sandbox = isolatedServerSandbox(sandboxRoot, named, row.inherited ?? {});
    const want = Object.fromEntries(Object.entries(row.expect).map(([key, value]) => [key, place(value)]));
    assert.deepEqual(
      { configDir: sandbox.configDir, dataDir: sandbox.dataDir, stateDir: sandbox.stateDir, home: sandbox.env.HOME, cache: sandbox.env.XDG_CACHE_HOME, ownsConfigDir: sandbox.ownsConfigDir, ownsDataDir: sandbox.ownsDataDir },
      want,
    );
    // The flags and the environment name the same three directories, so neither can fall back to the person's.
    assert.deepEqual([...sandbox.args], ['--config-dir', want.configDir, '--data-dir', want.dataDir, '--state-dir', want.stateDir]);
    assert.deepEqual(
      { config: sandbox.env.XDG_CONFIG_HOME, data: sandbox.env.XDG_DATA_HOME, state: sandbox.env.XDG_STATE_HOME },
      { config: want.configDir, data: want.dataDir, state: want.stateDir },
    );
    // Every place the binary can reach is under the sandbox root or is one the caller named.
    const allowed = [sandboxRoot, ...Object.values(named)];
    for (const key of ['HOME', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_STATE_HOME', 'XDG_CACHE_HOME']) {
      assert.ok(allowed.some((prefix) => sandbox.env[key] === prefix || sandbox.env[key].startsWith(`${prefix}/`)), `${key} is ${sandbox.env[key]}, outside the sandbox`);
    }
    for (const [key, value] of Object.entries(row.kept ?? {})) assert.equal(sandbox.env[key], value, `${key} is passed on unchanged`);
    return;
  }
  if (row.mode === 'owned-sandbox-name') {
    for (const name of row.names) assert.throws(() => ownedServerSandbox(name), new RegExp(row.error), JSON.stringify(name));
    return;
  }
  if (row.mode === 'owned-sandbox') {
    // A real script process: it makes its sandbox, says where, writes into it as a server would, and exits.
    const library = new URL('./lib/owned-server-sandbox.mjs', import.meta.url).href;
    for (const status of row.exits) {
      const source = `import { ownedServerSandbox } from ${JSON.stringify(library)}; import { mkdirSync, writeFileSync, existsSync } from 'node:fs'; import { join } from 'node:path';`
        + `const sandbox = ownedServerSandbox(${JSON.stringify(row.script)}); mkdirSync(join(sandbox.dataDir, 'peasant'), { recursive: true }); writeFileSync(join(sandbox.dataDir, 'peasant', 'peasant.db'), 'x');`
        + `console.log(JSON.stringify({ root: sandbox.root, dataDir: sandbox.dataDir, home: sandbox.env.HOME, existed: existsSync(sandbox.root) })); process.exit(${status});`;
      let out;
      try { out = execFileSync(process.execPath, ['--input-type=module', '-e', source], { env: { ...process.env, TMPDIR: root }, encoding: 'utf8' }); }
      catch (error) { assert.equal(error.status, status, error.stderr); out = error.stdout; }
      const said = JSON.parse(out);
      assert.equal(said.existed, true);
      assert.ok(said.root.startsWith(join(root, `peasant-${row.script}-`)), `${said.root} is in the temporary directory and named for the script`);
      assert.equal(said.dataDir, join(said.root, 'data'));
      assert.equal(said.home, join(said.root, 'home'));
      assert.equal(existsSync(said.root), false, `the sandbox is gone after an exit with status ${status}`);
    }
    return;
  }
  const directory = join(root, 'owner');
  const acquire = () => acquireLifecycle(directory, row);
  function peer(mode = 'hold', extraEnv = {}) {
    const child = start(process.execPath, [new URL('./lib/lifecycle-peer.mjs', import.meta.url).pathname, directory, mode], { stdio: ['pipe', 'pipe', 'inherit'], env: { ...process.env, ...extraEnv } });
    const ready = new Promise(done => child.child.stdout.once('data', data => done(JSON.parse(data.toString()))));
    cleanups.push(async () => { child.child.kill('SIGKILL'); await bounded(child.settled, 3000, 'test peer reaping'); });
    child.child.stdin.write('acquire');
    return { ...child, ready };
  }
  if (row.mode === 'forced' || row.mode === 'timeout' || row.mode === 'missing') {
    const life = await acquire(); cleanups.push(() => life.cleanup());
    const immutable = readFileSync(join(directory, 'owner.json'), 'utf8');
    const child = row.mode === 'missing'
      ? life.launch('/no-such-peasant-journey-command', [], { stdio: 'ignore' })
      : life.launch(process.execPath, ['-e', "process.on('SIGTERM',()=>{});process.stdout.write('ready\\n');setTimeout(()=>process.exit(3),20000)"], { stdio: ['ignore', 'pipe', 'inherit'] });
    if (row.mode !== 'missing') await bounded(new Promise(done => child.child.stdout.once('data', done)), 3000, 'test child readiness');
    else await assert.rejects(child.done);
    if (row.mode === 'timeout') await assert.rejects(bounded(child.done, 50, 'test compile deadline'), /test compile deadline exceeded/);
    assert.equal(readFileSync(join(directory, 'owner.json'), 'utf8'), immutable);
    let settlements = 0; void child.settled.then(() => settlements++);
    const cleaning = life.cleanup(); assert.equal(life.cleanup(), cleaning); await cleaning;
    assert.equal(settlements, 1); assert.equal(ownedGroups(life.identity.owner).size, 0);
    if (row.mode !== 'missing') assert.equal((await child.settled).signal, 'SIGKILL');
    assert.equal(existsSync(life.identity.stateRoot), false); assert.equal(existsSync(directory), false);
    assert.equal(existsSync(`${directory}.gate`), true);
    return;
  }
  const old = peer('orphan', row.mode === 'changed-temp' ? { TMPDIR: join(root, 'previous-shell-temp') } : {}); const oldIdentity = await bounded(old.ready, 3000, 'old owner readiness');
  old.child.kill('SIGKILL'); await bounded(old.settled, 3000, 'killed owner reaping');
  cleanups.push(async () => { const life = await acquire(); await life.cleanup(); });
  if (row.mode === 'stale' || row.mode === 'changed-temp') {
    const other = await acquireLifecycle(join(root, 'unrelated'), row); cleanups.push(() => other.cleanup());
    const child = other.launch(process.execPath, ['-e', 'setTimeout(()=>{},15000)'], { stdio: 'ignore' });
    const next = await acquire(); cleanups.push(() => next.cleanup());
    assert.equal(ownedGroups(oldIdentity.owner).size, 0); assert.equal(existsSync(oldIdentity.stateRoot), false);
    assert.equal(child.child.exitCode, null); await next.cleanup();
  } else {
    const a = peer(), b = peer();
    const result = await bounded(Promise.race([a.ready.then(identity => ({ winner: a, loser: b, identity })), b.ready.then(identity => ({ winner: b, loser: a, identity }))]), 5000, 'winner readiness');
    assert.equal((await bounded(result.loser.settled, 3000, 'loser reaping')).code, 2);
    assert.deepEqual(JSON.parse(readFileSync(join(directory, 'owner.json'), 'utf8')), result.identity);
    assert.equal(ownedGroups(oldIdentity.owner).size, 0);
    result.winner.child.stdin.end(); await bounded(result.winner.done, 3000, 'winner cleanup');
    assert.equal(existsSync(directory), false);
  }
});
