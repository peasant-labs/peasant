import { fileURLToPath } from 'node:url';
import { join, resolve } from 'node:path';
import { writeFileSync } from 'node:fs';
import { acquireLifecycle, bounded } from './lib/owned-lifecycle.mjs';
const web = fileURLToPath(new URL('../../', import.meta.url)), repo = resolve(web, '..');
let life, stopping = false;
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.once(signal, () => {
  stopping = true; void life?.cleanup().finally(() => process.exit(1));
});
try {
  life = await acquireLifecycle(join(web, '.collective-observations-owner'));
  if (stopping) throw new Error('Fixture tests cancelled before build');
  if (process.env.GOWORK && process.env.GOWORK !== 'off') throw new Error('This ordinary fixture gate uses published module pins; source-only verification must use an explicitly external prototype command/config, not this shipping gate');
  const binary = join(life.identity.stateRoot, 'fixture-seeder');
  // Cold hosted runners must download modules and compile the full Go graph.
  // Match the built-binary journey's bounded build budget; cleanup still owns
  // and terminates the entire child process group if the deadline expires.
  await bounded(life.launch('go', ['build', '-o', binary, './internal/testutil/cmd/collectiveobservations-fixture'], { cwd: repo, stdio: 'inherit', env: { ...process.env, GOWORK: 'off' } }).done, 600000, 'fixture seeder compilation');
  const child = life.launch(binary, ['--action', 'fixture'], { cwd: repo, stdio: ['ignore', 'pipe', 'inherit'] });
  const chunks = []; let bytes = 0;
  child.child.stdout.on('data', chunk => { bytes += chunk.length; if (bytes > 1024 * 1024) child.child.kill('SIGTERM'); else chunks.push(chunk); });
  await bounded(child.done, 10000, 'canonical fixture export');
  if (bytes > 1024 * 1024) throw new Error('Canonical fixture export exceeded its explicit size bound');
  const path = join(life.identity.stateRoot, 'fixture.json'); writeFileSync(path, Buffer.concat(chunks), { mode: 0o600 });
  await bounded(life.launch('pnpm', ['exec', 'vitest', 'run', '--config', 'vitest.journey-fixture.config.mjs'], { cwd: web, stdio: 'inherit', env: { ...process.env, PEASANT_FIXTURE_EXPORT: path } }).done, 30000, 'remote fixture validation');
} catch (error) { console.error(error.message); process.exitCode = 1; }
finally { try { await life?.cleanup(); } catch (error) { console.error(error.message); process.exit(1); } }
