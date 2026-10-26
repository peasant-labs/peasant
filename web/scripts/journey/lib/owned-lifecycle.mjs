import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

export async function bounded(promise, ms, operation) {
  let timer;
  try { return await Promise.race([promise, new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${operation} exceeded ${ms}ms; owned resources will be stopped; inspect retained evidence before retrying`)), ms);
  })]); } finally { clearTimeout(timer); }
}

/** One waiter is attached immediately, including failed spawns and forced termination. */
export function start(command, args, options = {}) {
  const child = spawn(command, args, options);
  const settled = new Promise(done => {
    let finished = false;
    const finish = result => { if (!finished) { finished = true; done(result); } };
    child.once('error', error => finish({ error }));
    child.once('exit', (code, signal) => finish({ code, signal }));
  });
  const done = settled.then(({ code, signal, error }) => {
    if (error) throw error;
    if (code !== 0) throw new Error(`${command} failed (${code ?? signal}); inspect its retained log before retrying`);
  });
  done.catch(() => {});
  return { child, settled, done };
}
function stat(pid) { return readFileSync(`/proc/${pid}/stat`, 'utf8').split(') ')[1].split(' '); }
function live(identity) {
  try { const s = stat(identity.pid); return s[0] !== 'Z' && s[19] === identity.start; } catch { return false; }
}
export function ownedGroups(owner) {
  const groups = new Set();
  for (const pid of readdirSync('/proc').filter(p => /^\d+$/.test(p))) {
    try {
      const s = stat(pid);
      if (s[0] !== 'Z' && readFileSync(`/proc/${pid}/environ`, 'utf8').split('\0').includes(`PEASANT_OBSERVATIONS_OWNER=${owner}`)) groups.add(Number(s[2]));
    } catch { /* a process may exit during this bounded snapshot */ }
  }
  return groups;
}
async function absent(owner, ms) {
  const until = Date.now() + ms;
  do { if (!ownedGroups(owner).size) return true; await delay(25); } while (Date.now() < until);
  return !ownedGroups(owner).size;
}
async function reap(owner, children, termMs, killMs) {
  for (const group of ownedGroups(owner)) { try { process.kill(-group, 'SIGTERM'); } catch (e) { if (e.code !== 'ESRCH') throw e; } }
  if (!await absent(owner, termMs)) {
    for (const group of ownedGroups(owner)) { try { process.kill(-group, 'SIGKILL'); } catch (e) { if (e.code !== 'ESRCH') throw e; } }
    if (!await absent(owner, killMs)) throw new Error('Owned process groups remain after KILL; ownership and flock are retained; inspect owner.json, then retry recovery after the owner exits');
  }
  await bounded(Promise.all(children.map(c => c.settled)), killMs, 'owned child reaping');
  if (ownedGroups(owner).size) throw new Error('Owned groups survived child reaping; ownership is retained, not released');
}
function validIdentity(identity, directory) {
  if (!Number.isSafeInteger(identity.pid) || !/^\d+$/.test(identity.start) || !/^[a-f0-9-]{36}$/.test(identity.owner)) return false;
  if (identity.stateRoot === join(directory, `state-${identity.owner}`)) return true;
  // Recover a previous runner whose Nix-shell TMPDIR disappeared after SIGKILL.
  // A surviving legacy root additionally needs its actual random ownership marker.
  const legacy = /^\/tmp\/(?:nix-shell\.[A-Za-z0-9]+\/)?peasant-observations-[a-f0-9-]{36}$/.test(identity.stateRoot) &&
    identity.stateRoot.endsWith(`/peasant-observations-${identity.owner}`);
  if (!legacy) return false;
  if (!existsSync(identity.stateRoot)) return true;
  try { return readFileSync(join(identity.stateRoot, '.collective-observations-fixture'), 'utf8') === identity.owner; } catch { return false; }
}

/** Linux-only persistent-inode flock covers acquisition, stale recovery, workload and teardown.
 * Identity is atomically published ONCE before workload spawn; PID alone never proves ownership.
 * This deliberately small app-owned lifetime follows the reviewed built-runner protocol.
 */
export async function acquireLifecycle(directory, { termMs = 1500, killMs = 2000 } = {}) {
  if (process.platform !== 'linux' || !existsSync('/proc/self/stat')) throw new Error('This focused binary journey requires Linux /proc and util-linux flock; run it in the documented Nix shell');
  directory = resolve(directory);
  mkdirSync(resolve(directory, '..'), { recursive: true });
  const gate = start('flock', ['--wait', '0.5', '--conflict-exit-code', '73', `${directory}.gate`, process.execPath, '-e',
    "process.stdout.write('locked\\n'); process.stdin.resume(); process.stdin.on('end',()=>process.exit(0))"], { stdio: ['pipe', 'pipe', 'inherit'] });
  try {
    const ready = new Promise((done, fail) => gate.child.stdout.once('data', bytes => bytes.toString().trim() === 'locked' ? done() : fail(new Error('flock did not confirm ownership'))));
    await bounded(Promise.race([ready, gate.settled.then(() => { throw new Error('Journey ownership is busy or flock is unavailable; no ownership metadata changed; wait for the current owner or install util-linux'); })]), 3000, 'journey ownership acquisition');
  } catch (error) {
    gate.child.stdin.destroy(); gate.child.kill('SIGTERM');
    await bounded(gate.settled, 2000, 'failed flock keeper exit');
    throw error;
  }
  const owner = randomUUID();
  const identity = Object.freeze({ pid: process.pid, start: stat(process.pid)[19], owner, stateRoot: join(directory, `state-${owner}`) });
  const children = [];
  let stopping;
  try {
    if (existsSync(directory)) {
      const path = join(directory, 'owner.json');
      if (existsSync(path)) {
        const previous = JSON.parse(readFileSync(path, 'utf8'));
        if (!validIdentity(previous, directory)) throw new Error('Invalid stale journey identity; metadata retained; inspect owner.json instead of deleting arbitrary state');
        if (live(previous)) throw new Error('A live journey owner still holds the build; wait for it before retrying');
        await reap(previous.owner, [], termMs, killMs);
        rmSync(previous.stateRoot, { recursive: true, force: true });
      }
      rmSync(directory, { recursive: true });
    }
    mkdirSync(directory, { mode: 0o700 });
    const temporary = join(directory, `owner-${owner}.tmp`);
    writeFileSync(temporary, JSON.stringify(identity), { flag: 'wx', mode: 0o600 });
    renameSync(temporary, join(directory, 'owner.json'));
    mkdirSync(identity.stateRoot, { mode: 0o700 });
    writeFileSync(join(identity.stateRoot, '.collective-observations-fixture'), owner, { flag: 'wx', mode: 0o600 });
  } catch (error) {
    gate.child.stdin.end(); await bounded(gate.settled, 2000, 'recovery flock keeper exit'); throw error;
  }
  return {
    identity,
    launch(command, args, options = {}) {
      if (stopping || gate.child.exitCode !== null || gate.child.signalCode !== null) throw new Error('Journey ownership is stopping or its flock keeper exited; no new child may start');
      const child = start(command, args, { ...options, detached: true, env: { ...process.env, ...options.env, PEASANT_OBSERVATIONS_OWNER: owner } });
      children.push(child); return child;
    },
    cleanup() {
      if (stopping) return stopping;
      stopping = (async () => {
        // A failed reap retains the immutable identity AND keeper; do not unlock an unproven workload.
        await reap(owner, children, termMs, killMs);
        rmSync(identity.stateRoot, { recursive: true, force: true });
        rmSync(directory, { recursive: true });
        gate.child.stdin.end(); await bounded(gate.settled, 2000, 'flock keeper reaping');
      })();
      return stopping;
    },
  };
}
