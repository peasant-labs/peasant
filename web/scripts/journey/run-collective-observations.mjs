import { createRequire } from 'node:module';
import { pathToFileURL, fileURLToPath } from 'node:url';
import { createServer } from 'node:net';
import { createHash, randomUUID } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync, mkdirSync, realpathSync, accessSync, constants, rmSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { parse } from 'yaml';
import { acquireLifecycle, bounded, ownedGroups } from './lib/owned-lifecycle.mjs';
import { assertExportedSurface, assertPublishedFiles, hash, inventory, verifyServed } from './lib/provenance.mjs';
import { startVillageFixture } from './lib/village-fixture.mjs';

const web = fileURLToPath(new URL('../../', import.meta.url)), repo = resolve(web, '..');
const require = createRequire(join(web, 'package.json'));
let life, remote, app, cleaning, stopping = false;
async function cleanup() {
  if (cleaning) return cleaning;
  stopping = true;
  cleaning = (async () => { try { await remote?.close(); } finally { await life?.cleanup(); } })();
  return cleaning;
}
// Installed before even the flock keeper is spawned.
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.once(signal, () => {
  stopping = true;
  void cleanup().then(() => process.exit(1), error => { console.error(error.message); process.exit(1); });
});
function launch(command, args, options = {}) {
  if (stopping) throw new Error('Journey teardown has begun; no child may start');
  return life.launch(command, args, { cwd: repo, stdio: 'inherit', ...options });
}
async function capture(command, args, options = {}) {
  const child = launch(command, args, { ...options, stdio: ['ignore', 'pipe', 'pipe'] });
  const chunks = [], errors = []; let size = 0, overflow = false;
  const collect = target => chunk => { size += chunk.length; if (size > 4 * 1024 * 1024) { overflow = true; child.child.kill('SIGTERM'); } else target.push(chunk); };
  child.child.stdout.on('data', collect(chunks)); child.child.stderr.on('data', collect(errors));
  try { await bounded(child.done, 30000, 'owned command'); }
  catch (error) { throw new Error(`${error.message}; ${Buffer.concat(errors).toString().slice(0, 2000)}`); }
  if (overflow) throw new Error('Owned command output exceeded the fixture evidence bound');
  return Buffer.concat(chunks).toString();
}
async function freePort() {
  const socket = createServer(); await new Promise((done, fail) => { socket.once('error', fail); socket.listen(0, '127.0.0.1', done); });
  const port = socket.address().port; await new Promise(done => socket.close(done)); return port;
}
async function packageRoot(name) {
  let dir = dirname(require.resolve(name === '@peasant-labs/fairtrade' ? `${name}/ui` : name));
  for (;;) {
    if (existsSync(join(dir, 'package.json')) && JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')).name === name) return realpathSync(dir);
    const parent = dirname(dir); if (parent === dir) throw new Error(`Cannot identify installed package root for ${name}`); dir = parent;
  }
}
async function publishedPackage(name, lock) {
  const installed = await packageRoot(name), version = JSON.parse(readFileSync(join(installed, 'package.json'), 'utf8')).version;
  const declaration = JSON.parse(readFileSync(join(web, 'package.json'), 'utf8')).dependencies[name];
  if (version !== declaration || !/^\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?$/.test(version)) throw new Error(`${name} must use an exact published version pin; source/link/range proof is refused`);
  const integrity = lock.packages[`${name}@${version}`]?.resolution?.integrity;
  const response = await fetch(`https://registry.npmjs.org/${encodeURIComponent(name)}/${version}`, { signal: AbortSignal.timeout(10000) });
  if (!response.ok) throw new Error(`npm does not publish ${name}@${version}; wait for its actual release`);
  const metadata = await response.json();
  if (!integrity || integrity !== metadata.dist?.integrity || !integrity.startsWith('sha512-')) throw new Error(`${name} registry and frozen-lock integrity disagree`);
  const tarURL = new URL(metadata.dist.tarball);
  if (tarURL.protocol !== 'https:' || tarURL.hostname !== 'registry.npmjs.org') throw new Error('Published package tarball is not on the canonical npm registry');
  const tarResponse = await fetch(tarURL, { signal: AbortSignal.timeout(15000) });
  if (!tarResponse.ok) throw new Error(`Cannot read published ${name} artifact`);
  const bytes = Buffer.from(await tarResponse.arrayBuffer());
  if (`sha512-${createHash('sha512').update(bytes).digest('base64')}` !== integrity) throw new Error(`Published ${name} tarball integrity failed`);
  const root = join(life.identity.stateRoot, name.split('/').at(-1)); mkdirSync(root);
  const tar = join(root, 'published.tgz'); writeFileSync(tar, bytes, { mode: 0o600 });
  const paths = (await capture('tar', ['-tzf', tar])).trim().split('\n');
  if (paths.some(path => !path.startsWith('package/') || path.split('/').includes('..'))) throw new Error('Published archive has unsafe paths');
  const listing = await capture('tar', ['-tvzf', tar]);
  if (listing.split('\n').some(line => line.startsWith('l') || line.startsWith('h'))) throw new Error('Published archive contains unsupported links');
  await launch('tar', ['--no-same-owner', '-xzf', tar, '-C', root]).done;
  const files = assertPublishedFiles(installed, join(root, 'package'));
  return { name, version, integrity, files };
}

try {
  if (Number(process.versions.node.split('.')[0]) < 26) throw new Error('The built journey requires Node >=26 for the runner and browser worker; enter the repository Nix shell before running the documented command');
  life = await acquireLifecycle(join(web, '.collective-observations-owner'));
  if (stopping) { await life.cleanup(); throw new Error('Journey cancelled during ownership acquisition'); }
  const ui = await import(pathToFileURL(require.resolve('@peasant-labs/fairtrade/ui')));
  const graph = await import(pathToFileURL(require.resolve('@peasant-labs/fairtrade/graph')));
  assertExportedSurface(ui, graph); // Fail before building or trusting any candidate-derived pixels.
  if (process.env.GOWORK && process.env.GOWORK !== 'off') throw new Error('Mounted proof refuses an external source-prototype GOWORK; unset it and use released module pins');
  const chrome = process.env.CHROME_PATH;
  if (!chrome) throw new Error('CHROME_PATH must name an explicitly installed Chromium executable; use the documented Nix shell, no runtime download is attempted');
  accessSync(chrome, constants.X_OK);
  const sourceStatus = (await capture('git', ['status', '--porcelain'])).trim();
  if (sourceStatus) throw new Error('Mounted review evidence requires a committed clean source tree; commit the coherent published adoption first (unpublished page preparation remains uncommitted)');
  const lock = parse(readFileSync(join(web, 'pnpm-lock.yaml'), 'utf8'));
  const packages = [await publishedPackage('@peasant-labs/fairtrade', lock), await publishedPackage('@peasant-labs/schema', lock)];
  const sourceCommit = (await capture('git', ['rev-parse', 'HEAD'])).trim();
  const sourcePaths = (await capture('git', ['ls-files', '-z'])).split('\0').filter(Boolean);
  const sourceFiles = Object.fromEntries(sourcePaths.map(path => [path, hash(readFileSync(join(repo, path)))]));
  const artifacts = join(repo, 'review-capture', `collective-observations${process.env.OBSERVATIONS_MUTANTS === '1' ? '-mutants' : ''}-${randomUUID()}`); mkdirSync(artifacts, { recursive: true });
  const state = life.identity.stateRoot;
  const env = { ...process.env, XDG_CONFIG_HOME: join(state, 'config'), XDG_DATA_HOME: join(state, 'data'), XDG_STATE_HOME: join(state, 'state'), HOME: join(state, 'home'), PEASANT_OBSERVATIONS_OWNER: life.identity.owner,
    PEASANT_COLLECTIVE_FETCH_TIMEOUT: '2s' };
  mkdirSync(env.HOME, { recursive: true });
  const seeder = join(state, 'fixture-seeder');
  await bounded((async () => {
    await launch('make', ['build'], { env: { ...process.env, GOWORK: 'off' } }).done;
    await launch('go', ['build', '-o', seeder, './internal/testutil/cmd/collectiveobservations-fixture'], { env: { ...process.env, GOWORK: 'off' } }).done;
  })(), 600000, 'production binary and fixture seeder build');
  for (const [path, digest] of Object.entries(sourceFiles)) if (hash(readFileSync(join(repo, path))) !== digest) throw new Error(`Source changed while building: ${path}`);
  const binary = realpathSync(join(repo, 'peasant')), out = join(web, 'out');
  const exported = JSON.parse(await capture(seeder, ['--action', 'fixture'], { env }));
  const config = parse(readFileSync(join(web, 'scripts/journey/testdata/collective-observations.yaml'), 'utf8'));
  let appURL = `http://127.0.0.1:${await freePort()}`;
  const mutationMode = process.env.OBSERVATIONS_MUTANTS === '1';
  const manifest = { sourceCommit, sourceFiles, counterfactual: mutationMode, lockHash: hash(readFileSync(join(web, 'pnpm-lock.yaml'))), packages, binary, binaryHash: hash(readFileSync(binary)), webAssets: inventory(out), nodeExecutable: process.execPath, nodeVersion: process.version, fetchTimeout: env.PEASANT_COLLECTIVE_FETCH_TIMEOUT, served: [], appURL };
  const saveManifest = () => writeFileSync(join(artifacts, 'build-manifest.json'), JSON.stringify(manifest, null, 2));
  async function startApp() {
    app = launch(binary, ['web', 'start', '--foreground', '--no-browser', '--port', new URL(appURL).port], { env });
    await bounded((async () => { for (;;) {
      if (app.child.exitCode !== null) throw new Error('The owned production binary exited during startup');
      try { const r = await fetch(`${appURL}/api/v1/health`, { signal: AbortSignal.timeout(1000) }); if (r.ok) return; } catch { /* readiness wake is health response, bounded by startup deadline */ }
      await delay(100);
    } })(), 30000, 'owned binary startup');
    manifest.served.push(await verifyServed(appURL, out, binary, app.child.pid)); saveManifest();
  }
  async function stopApp() {
    if (!app) return;
    const group = app.child.pid;
    if (ownedGroups(life.identity.owner).has(group)) { try { process.kill(-group, 'SIGTERM'); } catch (e) { if (e.code !== 'ESRCH') throw e; } }
    await bounded(app.settled, 3000, 'owned app shutdown');
    if (ownedGroups(life.identity.owner).has(group)) throw new Error('App group survived shutdown; refusing account reseed or restart');
    app = undefined;
  }
  const seed = async (action, account = 'alice') => JSON.parse(await capture(seeder, ['--root', state, '--village-url', remote.origin, '--action', action, '--account', account], { env }));
  const control = {
    token: life.identity.owner, scenarios: config.scenarios, actions: config.controlActions,
    async run(action, selected) {
      if (action === 'reset') {
        await stopApp();
        // Only disposable directories below our immutable random state root are removed.
        for (const directory of [env.XDG_CONFIG_HOME, env.XDG_DATA_HOME, env.XDG_STATE_HOME]) rmSync(directory, { recursive: true, force: true });
        const result = await seed('seed', 'alice'); await startApp(); return result;
      }
      if (action === 'account') { await stopApp(); const result = await seed('seed', selected); await startApp(); return result; }
      if (action === 'stop-app') { await stopApp(); return {}; }
      if (action === 'start-app') { await startApp(); return {}; }
      if (action === 'pull') {
        const text = await capture(binary, ['village', 'transcripts', 'pull', exported.fixtures.TranscriptID, '--json'], { env });
        const result = JSON.parse(text);
        const directory = realpathSync(result.pullDir);
        if (!directory.startsWith(`${realpathSync(env.XDG_DATA_HOME)}/`)) throw new Error('Pull result escaped the owned fixture data root');
        if (!readFileSync(join(directory, exported.transcriptFilename)).equals(Buffer.from(exported.contentBytesBase64, 'base64'))) throw new Error('Optional metadata failure did not preserve the actual validated content bytes');
        return result;
      }
      if (action === 'cli') {
        const text = await capture(binary, ['village', 'collectives', 'list', '--json'], { env }); return JSON.parse(text);
      }
      if (action === 'history') {
        const schema = await import(pathToFileURL(require.resolve('@peasant-labs/schema')));
        const kind = schema.zLocalContributionEvidenceKind.parse(selected.get('kind'));
        const page = Number(selected.get('page') ?? '1'), limit = Number(selected.get('limit') ?? '2');
        if (!Number.isSafeInteger(page) || page < 1 || !Number.isSafeInteger(limit) || limit < 1 || limit > 200) throw new Error('Fixture CLI history needs bounded page/limit');
        const args = ['village', 'contributions', 'list', '--json', '--kind', kind, '--page', String(page), '--limit', String(limit)];
        if (selected.has('revision')) args.push('--revision', selected.get('revision'));
        return JSON.parse(await capture(binary, args, { env }));
      }
      return seed(action);
    },
  };
  remote = await startVillageFixture(exported, await import(pathToFileURL(require.resolve('@peasant-labs/schema'))), control);
  await seed('seed'); await startApp(); saveManifest();
  writeFileSync(join(artifacts, 'fixture-export.json'), JSON.stringify(exported)); // synthetic public fixtures only, no saved credentials
  const journeyEnv = { ...env, CHROME_PATH: chrome, OBSERVATIONS_APP_URL: appURL, OBSERVATIONS_REMOTE_URL: remote.origin, OBSERVATIONS_CONTROL_TOKEN: control.token, OBSERVATIONS_ARTIFACTS: artifacts };
  // Resolve the installed CLI before entering isolated HOME. A package-manager
  // launcher may otherwise try to read its user's Corepack cache from that HOME.
  const selection = mutationMode ? ['--project', 'dark-desktop'] : [];
  await bounded(launch(process.execPath, [require.resolve('@playwright/test/cli'), 'test', '--config', 'playwright.collective-observations.config.mjs', ...selection], { cwd: web, env: journeyEnv }).done, 300000, 'collective observations mounted suite');
  writeFileSync(join(artifacts, 'fixture-requests.json'), JSON.stringify(remote.requests, null, 2));
  console.log(mutationMode ? `Counterfactual rejection evidence in ${artifacts}; not positive UI proof.` : `Inspect retained full-shell evidence in ${artifacts}; generation alone is not visual acceptance.`);
} catch (error) { console.error(error.message); process.exitCode = 1; }
finally { try { await cleanup(); } catch (error) { console.error(error.message); process.exit(1); } }
