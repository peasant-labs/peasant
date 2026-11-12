import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:net';
import { randomUUID } from 'node:crypto';
import { accessSync, closeSync, constants, existsSync, mkdirSync, mkdtempSync, openSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { delimiter, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { parse } from 'yaml';
import { acquireLifecycle, bounded, ownedGroups } from './lib/owned-lifecycle.mjs';
import { hash } from './lib/provenance.mjs';
import { verifyKickstartServed, markerInCommit } from './lib/kickstart-provenance.mjs';
import { seedRecordings } from './lib/kickstart-seed.mjs';

/**
 * Runs the setup journey on the real binary.
 *
 * It owns everything it starts: it takes the journey lock, builds the web
 * export and the binary from the committed tree, and for each theme resets a
 * throwaway computer (its own HOME and XDG directories, seeded agent
 * recordings, real Git repositories), starts the binary on a free port,
 * proves that what answers is this build and this branch, runs the browser
 * cases, and tears everything down. A source change during the build, a dirty
 * tree, a stale server or a marker the base commit also had stops the run
 * before any capture is trusted.
 *
 * The build is `make build`: the web export, the binary and the provider-policy
 * guard that checks both. The guard's own lines are kept in the manifest, and a
 * build that does not print them is refused.
 */

const web = fileURLToPath(new URL('../../', import.meta.url));
const repo = resolve(web, '..');
const require = createRequire(join(web, 'package.json'));
let life;
let app;
let stopApp = async () => {};
let cleaning;
let stopping = false;
let sandbox;
const buildOutput = [];
let manifest;
let saveManifest = () => {};

async function cleanup() {
  if (cleaning) return cleaning;
  stopping = true;
  cleaning = (async () => { await life?.cleanup(); })();
  return cleaning;
}
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.once(signal, () => {
  stopping = true;
  void cleanup().then(() => process.exit(1), (error) => { console.error(error.message); process.exit(1); });
});

function launch(command, args, options = {}) {
  if (stopping) throw new Error('Journey teardown has begun; no child may start');
  return life.launch(command, args, { cwd: repo, stdio: 'inherit', ...options });
}

async function capture(command, args, options = {}) {
  const child = launch(command, args, { ...options, stdio: ['ignore', 'pipe', 'pipe'] });
  const out = [];
  const err = [];
  child.child.stdout.on('data', (chunk) => out.push(chunk));
  child.child.stderr.on('data', (chunk) => err.push(chunk));
  try { await bounded(child.done, 60000, `${command} ${args[0] ?? ''}`); } catch (error) { throw new Error(`${error.message}; ${Buffer.concat(err).toString().slice(0, 2000)}`); }
  return Buffer.concat(out).toString();
}

async function freePort() {
  const socket = createServer();
  await new Promise((done, fail) => { socket.once('error', fail); socket.listen(0, '127.0.0.1', done); });
  const port = socket.address().port;
  await new Promise((done) => socket.close(done));
  return port;
}

function findChrome() {
  const named = process.env.CHROME_PATH;
  if (named) { accessSync(named, constants.X_OK); return named; }
  for (const directory of (process.env.PATH ?? '').split(delimiter)) {
    const candidate = join(directory, 'chromium');
    if (existsSync(candidate)) return candidate;
  }
  throw new Error('No Chromium: set CHROME_PATH to an installed Chromium, or enter the repository Nix shell, which provides one. No browser is downloaded.');
}

try {
  if (Number(process.versions.node.split('.')[0]) < 26) throw new Error('The setup journey requires Node >=26; enter the repository Nix shell (nix develop) and run it from there');
  if (process.env.GOWORK && process.env.GOWORK !== 'off') throw new Error('The journey refuses an external GOWORK; unset it and use the released module pins');
  const fixture = parse(readFileSync(join(web, 'scripts/journey/testdata/kickstart.yaml'), 'utf8'));
  const chrome = findChrome();
  life = await acquireLifecycle(join(web, '.kickstart-journey-owner'));
  if (stopping) { await life.cleanup(); throw new Error('Journey cancelled during ownership acquisition'); }

  const dirty = (await capture('git', ['status', '--porcelain'])).trim();
  if (dirty) throw new Error(`The journey proves the committed tree, and this one has uncommitted changes:\n${dirty}\nCommit them, then run it again.`);
  const sourceCommit = (await capture('git', ['rev-parse', 'HEAD'])).trim();
  const sourceFiles = Object.fromEntries((await capture('git', ['ls-files', '-z'])).split('\0').filter(Boolean).map((path) => [path, hash(readFileSync(join(repo, path)))]));

  const artifacts = join(repo, 'review-capture', `kickstart-${randomUUID()}`);
  mkdirSync(artifacts, { recursive: true });
  const binary = join(repo, 'peasant');
  const out = join(web, 'out');

  await bounded((async () => {
    const build = launch('make', ['build'], { env: { ...process.env, GOWORK: 'off' }, stdio: ['ignore', 'pipe', 'inherit'] });
    build.child.stdout.on('data', (chunk) => { process.stdout.write(chunk); buildOutput.push(chunk); });
    await build.done;
  })(), 900000, 'make build (web export, binary, provider-policy guard)');
  const guardLines = Buffer.concat(buildOutput).toString().split('\n').filter((line) => line.startsWith('provider build provenance'));
  if (!guardLines.some((line) => /found in web\/out and \.\/peasant/.test(line))) {
    throw new Error(`make build finished without the provider-policy guard's verdict on web/out and ./peasant; its lines were ${JSON.stringify(guardLines)}. A build the guard did not vouch for is not the build to take captures from.`);
  }
  for (const [path, digest] of Object.entries(sourceFiles)) {
    if (hash(readFileSync(join(repo, path))) !== digest) throw new Error(`Source changed while building: ${path}`);
  }
  const binaryPath = realpathSync(binary);

  manifest = {
    sourceCommit,
    baseCommit: fixture.baseCommit,
    binary: binaryPath,
    binaryHash: hash(readFileSync(binaryPath)),
    node: process.version,
    chrome,
    providerBuildGuard: { command: 'make build', result: guardLines },
    projects: [],
  };
  saveManifest = () => writeFileSync(join(artifacts, 'manifest.json'), JSON.stringify(manifest, null, 2));
  saveManifest();

  // The seeded computer lives under a plain path: Claude Code's project names
  // flatten punctuation, and a root with dots or dashes (the lifecycle's own state
  // root and the Nix shell's TMPDIR both have them) can make discovery resolve the
  // projects to the enclosing checkout.
  sandbox = mkdtempSync('/tmp/ksjourney');
  const paths = (root) => ({
    root,
    home: join(root, 'home'),
    config: join(root, 'config'),
    data: join(root, 'data'),
    stateDir: join(root, 'state'),
  });

  async function startApp(computer, name) {
    const port = await freePort();
    const origin = `http://127.0.0.1:${port}`;
    const log = openSync(join(artifacts, `server-${name}.log`), 'w');
    const env = { ...process.env, HOME: computer.home, XDG_CONFIG_HOME: computer.config, XDG_DATA_HOME: computer.data, XDG_STATE_HOME: computer.stateDir };
    app = launch(binaryPath, ['web', 'start', '--foreground', '--no-browser', '--port', String(port)], { env, stdio: ['ignore', log, log] });
    closeSync(log);
    await bounded((async () => {
      for (;;) {
        if (app.child.exitCode !== null) throw new Error(`The owned binary exited during startup; see ${join(artifacts, `server-${name}.log`)}`);
        try { const r = await fetch(`${origin}/api/v1/health`, { signal: AbortSignal.timeout(1000) }); if (r.ok) return; } catch { /* health is the wake source, bounded by the startup deadline */ }
        await delay(100);
      }
    })(), 30000, 'owned binary startup');
    return { origin, env };
  }
  stopApp = async () => {
    if (!app) return;
    const group = app.child.pid;
    if (ownedGroups(life.identity.owner).has(group)) { try { process.kill(-group, 'SIGTERM'); } catch (error) { if (error.code !== 'ESRCH') throw error; } }
    await bounded(app.settled, 5000, 'owned app shutdown');
    if (ownedGroups(life.identity.owner).has(group)) throw new Error('The app group survived shutdown; refusing to reset the computer under it');
    app = undefined;
  };

  const requestedProjects = process.argv.slice(2).filter((argument) => !argument.startsWith('-'));
  const projects = fixture.projects.filter((candidate) => requestedProjects.length === 0 || requestedProjects.includes(candidate.name));
  if (!projects.length) throw new Error(`No theme project matches ${JSON.stringify(requestedProjects)}; the journey has ${fixture.projects.map((p) => p.name).join(', ')}`);

  for (const theme of projects) {
    // A fresh computer for each theme: nothing the last one saved is here.
    const computer = paths(join(sandbox, theme.theme));
    rmSync(computer.root, { recursive: true, force: true });
    mkdirSync(computer.home, { recursive: true });
    const seeded = seedRecordings({ home: computer.home, root: computer.root, repoRoot: repo, fixture: fixture.recordings });
    const seededPath = join(artifacts, `seeded-${theme.name}.json`);
    writeFileSync(seededPath, JSON.stringify(seeded, null, 2));
    const { origin, env } = await startApp(computer, theme.name);

    const provenance = await verifyKickstartServed({
      origin,
      out,
      binary: binaryPath,
      pid: app.child.pid,
      routes: fixture.provenance.routes,
      markers: fixture.provenance.markers,
      baseHasMarker: (marker) => markerInCommit(repo, fixture.baseCommit, marker),
    });
    manifest.projects.push({ name: theme.name, origin, seeded: seeded.length, provenance });
    saveManifest();
    console.log(`${theme.name}: serving ${provenance.executable} (${provenance.binaryHash.slice(0, 12)}); markers ${Object.keys(provenance.markers).join(', ')} found in the served bundle, absent from ${fixture.baseCommit.slice(0, 9)}`);

    const journeyEnv = {
      ...env,
      CHROME_PATH: chrome,
      KICKSTART_APP_URL: origin,
      KICKSTART_ARTIFACTS: artifacts,
      KICKSTART_CONFIG_PATH: join(computer.config, 'peasant', 'config.yaml'),
      KICKSTART_HOME: computer.home,
      KICKSTART_SEEDED: seededPath,
    };
    await bounded(
      launch(process.execPath, [require.resolve('@playwright/test/cli'), 'test', '--config', 'playwright.kickstart.config.mjs', '--project', theme.name], { cwd: web, env: journeyEnv }).done,
      420000,
      `the ${theme.name} journey`,
    );
    manifest.projects.at(-1).result = 'passed';
    saveManifest();
    await stopApp();
  }
  manifest.suiteResult = 'passed';
  saveManifest();
  console.log(`Setup journey passed in ${artifacts}`);
} catch (error) {
  if (manifest) { manifest.suiteResult = 'failed'; try { saveManifest(); } catch (writeError) { console.error(writeError.message); } }
  console.error(error.message);
  process.exitCode = 1;
} finally {
  try { await stopApp(); } catch (error) { console.error(error.message); }
  // Only the directory this run created, and only after the app that used it has stopped.
  if (sandbox && !app) rmSync(sandbox, { recursive: true, force: true });
  try { await cleanup(); } catch (error) { console.error(error.message); process.exit(1); }
}
