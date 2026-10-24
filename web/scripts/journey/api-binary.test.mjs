// @vitest-environment node
import { afterAll, beforeAll, expect, it } from 'vitest';
import { createServer } from 'node:net';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { join, resolve } from 'node:path';
import { readFileSync, writeFileSync, mkdirSync, rmSync, realpathSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';
import { parse } from 'yaml';
import * as contract from '@peasant-labs/schema';
import { acquireLifecycle, bounded, ownedGroups } from './lib/owned-lifecycle.mjs';
import { startVillageFixture } from './lib/village-fixture.mjs';

const web = fileURLToPath(new URL('../../', import.meta.url)), repo = resolve(web, '..');
const fixture = parse(readFileSync(new URL('./testdata/api-binary.yaml', import.meta.url), 'utf8'));
if (new Set(fixture.cases.map(row => row.name)).size !== fixture.cases.length || fixture.requiredNames.some(name => !fixture.cases.some(row => row.name === name))) throw new Error('Required binary API scenario missing or repeated');
let life, exported, binary, seeder, env, cleaning, source;
const processes = new Set(), remotes = new Set();
const sha = bytes => createHash('sha256').update(bytes).digest('hex');
async function clean() {
  if (cleaning) return cleaning;
  cleaning = (async () => { try { for (const remote of remotes) await remote.close(); } finally { await life?.cleanup(); } })();
  return cleaning;
}
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.once(signal, () => { void clean().then(() => process.exit(1), () => process.exit(1)); });
function launch(command, args, options = {}) { return life.launch(command, args, { cwd: repo, stdio: ['ignore', 'pipe', 'pipe'], ...options }); }
async function capture(command, args, options = {}) {
  const child = launch(command, args, options), out = [], errors = []; let bytes = 0;
  const collect = target => chunk => { bytes += chunk.length; if (bytes > 1024 * 1024) child.child.kill('SIGTERM'); else target.push(chunk); };
  child.child.stdout.on('data', collect(out)); child.child.stderr.on('data', collect(errors));
  try { await bounded(child.done, 30000, 'binary API helper'); } catch (error) { throw new Error(`${error.message}: ${Buffer.concat(errors).toString().slice(0, 2000)}`); }
  if (bytes > 1024 * 1024) throw new Error('Binary API helper exceeded its bounded output');
  return Buffer.concat(out).toString();
}
async function port() {
  const socket = createServer(); await new Promise((done, fail) => { socket.once('error', fail); socket.listen(0, '127.0.0.1', done); });
  const value = socket.address().port; await new Promise(done => socket.close(done)); return value;
}
async function stop(app) {
  if (ownedGroups(life.identity.owner).has(app.child.pid)) { try { process.kill(-app.child.pid, 'SIGTERM'); } catch (e) { if (e.code !== 'ESRCH') throw e; } }
  await bounded(app.settled, 3000, 'binary API server shutdown');
  if (ownedGroups(life.identity.owner).has(app.child.pid)) throw new Error('Binary API server group survived shutdown; no fixture state will be reset');
  processes.delete(app);
}
beforeAll(async () => {
  if (Number(process.versions.node.split('.')[0]) < 26) throw new Error('API proof requires the repository Node >=26 toolchain; enter the Nix shell before running the fixture command');
  life = await acquireLifecycle(join(web, '.collective-observations-owner'));
  const sourcePaths = (await capture('git', ['ls-files', '-z', '--cached', '--others', '--exclude-standard'])).split('\0').filter(Boolean);
  const files = Object.fromEntries(sourcePaths.map(path => [path, sha(readFileSync(join(repo, path)))]));
  const schema = JSON.parse(await capture('go', ['list', '-m', '-json', 'github.com/peasant-labs/schema']));
  source = { gitHead: (await capture('git', ['rev-parse', 'HEAD'])).trim(), files, nodeVersion: process.version, goVersion: (await capture('go', ['version'])).trim(),
    gitDiffHash: sha(await capture('git', ['diff', 'HEAD'])),
    goWorkHash: process.env.GOWORK && process.env.GOWORK !== 'off' ? sha(readFileSync(process.env.GOWORK)) : undefined,
    schema: { module: schema.Path, version: schema.Version ?? 'unreleased-workspace-source', directory: schema.Dir } };
  if (schema.Main) {
    source.schema.gitHead = (await capture('git', ['rev-parse', 'HEAD'], { cwd: schema.Dir })).trim();
    source.schema.gitDiffHash = sha(await capture('git', ['diff', 'HEAD'], { cwd: schema.Dir }));
  }
  const root = life.identity.stateRoot;
  binary = join(root, 'peasant-api'); seeder = join(root, 'fixture-seeder');
  await bounded((async () => {
    await launch('go', ['build', '-o', binary, './cmd/peasant'], { stdio: 'inherit' }).done;
    await launch('go', ['build', '-o', seeder, './internal/testutil/cmd/collectiveobservations-fixture'], { stdio: 'inherit' }).done;
  })(), 600000, 'API-only binary and fixture build');
  for (const [path, digest] of Object.entries(files)) if (sha(readFileSync(join(repo, path))) !== digest) throw new Error(`API proof source changed while compiling: ${path}`);
  env = { ...process.env, XDG_CONFIG_HOME: join(root, 'config'), XDG_DATA_HOME: join(root, 'data'), XDG_STATE_HOME: join(root, 'state'), HOME: join(root, 'home'), PEASANT_OBSERVATIONS_OWNER: life.identity.owner, PEASANT_COLLECTIVE_FETCH_TIMEOUT: fixture.timeout };
  mkdirSync(env.HOME, { recursive: true });
  exported = JSON.parse(await capture(seeder, ['--action', 'fixture'], { env }));
}, 600000);
afterAll(async () => {
  for (const app of processes) await stop(app);
  await clean();
});

for (const row of fixture.cases) it(row.name, async () => {
  for (const path of [env.XDG_CONFIG_HOME, env.XDG_DATA_HOME, env.XDG_STATE_HOME]) rmSync(path, { recursive: true, force: true });
  const remote = await startVillageFixture(exported, contract, { token: life.identity.owner, scenarios: [row.scenario], actions: [], run() { throw new Error('No local test endpoint exists'); } });
  remotes.add(remote);
  let app;
  try {
    const seedArgs = ['--root', life.identity.stateRoot, '--village-url', remote.origin];
    const seeded = contract.zCollectiveObservationResponse.parse(JSON.parse(await capture(seeder, [...seedArgs, '--action', 'seed'], { env })));
    const origin = `http://127.0.0.1:${await port()}`;
    app = launch(binary, ['web', 'start', '--foreground', '--no-browser', '--port', new URL(origin).port], { env: { ...env, PEASANT_COLLECTIVE_FETCH_TIMEOUT: row.invalidTimeout ?? fixture.timeout } });
    processes.add(app);
    const errors = []; app.child.stderr.on('data', bytes => { if (errors.length < 100) errors.push(bytes); }); app.child.stdout.resume();
    if (row.invalidTimeout) {
      const result = await bounded(app.settled, 10000, 'invalid runtime startup refusal');
      expect(result.code).not.toBe(0); expect(Buffer.concat(errors).toString()).toContain('PEASANT_COLLECTIVE_FETCH_TIMEOUT');
      expect(remote.requests).toEqual([]);
      await expect(fetch(`${origin}/api/v1/health`, { signal: AbortSignal.timeout(1000) })).rejects.toThrow();
      return;
    }
    await bounded((async () => { for (;;) {
      if (app.child.exitCode !== null) throw new Error(`API server exited: ${Buffer.concat(errors)}`);
      try { if ((await fetch(`${origin}/api/v1/health`, { signal: AbortSignal.timeout(1000) })).ok) return; } catch { /* health response is the readiness wake, with an outer 30s deadline */ }
      await delay(100);
    } })(), 30000, 'actual binary API readiness');
    expect(realpathSync(`/proc/${app.child.pid}/exe`)).toBe(realpathSync(binary));
    expect(sha(readFileSync(`/proc/${app.child.pid}/exe`))).toBe(sha(readFileSync(binary)));
    const path = '/api/v1/village/collective-observations';
    const before = contract.zCollectiveObservationResponse.parse(await (await fetch(`${origin}${path}`)).json());
    expect(before).toEqual(seeded); expect(remote.requests).toEqual([]);
    const scenarioURL = new URL('/__fixture/control', remote.origin);
    scenarioURL.search = new URLSearchParams({ action: 'scenario', name: row.scenario }).toString();
    expect((await fetch(scenarioURL, { method: 'POST', headers: { 'x-fixture-control': life.identity.owner } })).ok).toBe(true);
    const started = Date.now();
    const response = await fetch(`${origin}${path}/fetch`, { method: 'POST', headers: { origin }, signal: AbortSignal.timeout(10000) });
    const report = contract.zCollectiveFetchReport.parse(await response.json());
    const elapsed = Date.now() - started;
    expect(response.status).toBe(row.status); expect(contract.collectiveFetchHTTPStatus(report)).toBe(row.status);
    expect(report.outcome).toBe('partial');
    const catalog = report.domains.find(d => d.domain === 'catalog'), history = report.domains.find(d => d.domain === 'history');
    expect(catalog.state).toBe('complete'); expect(catalog.commit).toBe('updated');
    expect(history.state).toBe(row.historyState); expect(history.commit).toBe('retained'); expect(history.issue.code).toBe(row.issueCode);
    expect(history.lastCompletedAt).toBe(before.fetch.history.lastCompletedAt);
    const after = contract.zCollectiveObservationResponse.parse(await (await fetch(`${origin}${path}`)).json());
    expect(after.collectives[0].name).toBe(exported.fixtures.NewName);
    expect(after.fetch.history.lastCompletedAt).toBe(before.fetch.history.lastCompletedAt);
    expect(after.fetch.lastCompletedFetchAt == null).toBe(true);
    const savedHistory = contract.zLocalContributionHistoryResponse.parse(await (await fetch(`${origin}/api/v1/village/contributions?kind=remote-attempts&page=1&limit=50`)).json());
    expect(savedHistory.remoteAttempts.map(attempt => attempt.attempt)).toEqual(JSON.parse(exported.fixtures.AttemptsJSON));
    const expected = Object.fromEntries(Object.entries(fixture.requiredRemotePaths).map(([key, count]) => [key.replace('{collective}', exported.fixtures.CollectiveID).replace('{transcript}', exported.fixtures.TranscriptID), count]));
    const actual = {};
    for (const call of remote.requests) { expect(call.method).toBe('GET'); actual[call.path] = (actual[call.path] ?? 0) + 1; }
    expect(actual).toEqual(expected);
    if (row.stalled) {
      expect(elapsed).toBeLessThan(fixture.maxDeadlineMilliseconds);
      await bounded((async () => { while (!remote.requests.find(call => call.path.endsWith('/events'))?.cancelled) await delay(25); })(), 1000, 'actual remote request cancellation');
    }
    const artifacts = join(repo, 'review-capture', 'collective-observations-api-only'); mkdirSync(artifacts, { recursive: true });
    writeFileSync(join(artifacts, `${row.name}.json`), JSON.stringify({ evidenceKind: process.env.GOWORK && process.env.GOWORK !== 'off' ? 'api-only-source-prototype' : 'api-only-released-modules', mountedUI: false,
      source, binaryHash: sha(readFileSync(binary)), fetchTimeout: fixture.timeout, status: response.status, elapsed, report, after, requests: remote.requests }, null, 2));
  } finally {
    if (app) await stop(app);
    await remote.close(); remotes.delete(remote);
  }
}, 60000);
