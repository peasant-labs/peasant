import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, existsSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { parse } from 'yaml';
import { acquireLifecycle, bounded, ownedGroups, start } from './lib/owned-lifecycle.mjs';

const f = parse(readFileSync(new URL('./testdata/lifecycle.yaml', import.meta.url), 'utf8'));
assert.deepEqual(new Set(f.cases.map(c => c.name)), new Set(f.requiredNames));
for (const row of f.cases) test(row.name, { timeout: 20000 }, async t => {
  const root = mkdtempSync(join(tmpdir(), 'peasant-journey-test-'));
  const cleanups = [() => rmSync(root, { recursive: true, force: true })];
  t.after(async () => { for (const cleanup of cleanups.reverse()) await cleanup(); });
  const directory = join(root, 'owner');
  const acquire = () => acquireLifecycle(directory, row);
  function peer(mode = 'hold', extraEnv = {}) {
    const child = start(process.execPath, [new URL('./lib/lifecycle-peer.mjs', import.meta.url).pathname, directory, mode], { stdio: ['pipe', 'pipe', 'inherit'], env: { ...process.env, ...extraEnv } });
    const ready = new Promise(done => child.child.stdout.once('data', data => done(JSON.parse(data.toString()))));
    cleanups.push(async () => { child.child.kill('SIGKILL'); await bounded(child.settled, 3000, 'test peer reaping'); });
    child.child.stdin.write('acquire');
    return { ...child, ready };
  }
  if (row.mode === 'forced' || row.mode === 'missing') {
    const life = await acquire(); cleanups.push(() => life.cleanup());
    const immutable = readFileSync(join(directory, 'owner.json'), 'utf8');
    const child = row.mode === 'missing'
      ? life.launch('/no-such-peasant-journey-command', [], { stdio: 'ignore' })
      : life.launch(process.execPath, ['-e', "process.on('SIGTERM',()=>{});process.stdout.write('ready\\n');setTimeout(()=>process.exit(3),20000)"], { stdio: ['ignore', 'pipe', 'inherit'] });
    if (row.mode === 'forced') await bounded(new Promise(done => child.child.stdout.once('data', done)), 3000, 'test child readiness');
    else await assert.rejects(child.done);
    assert.equal(readFileSync(join(directory, 'owner.json'), 'utf8'), immutable);
    let settlements = 0; void child.settled.then(() => settlements++);
    const cleaning = life.cleanup(); assert.equal(life.cleanup(), cleaning); await cleaning;
    assert.equal(settlements, 1); assert.equal(ownedGroups(life.identity.owner).size, 0);
    if (row.mode === 'forced') assert.equal((await child.settled).signal, 'SIGKILL');
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
