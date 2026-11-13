import { fileURLToPath } from 'node:url';
import { acquireLifecycle } from './owned-lifecycle.mjs';
const LIFECYCLE_CHILD = fileURLToPath(new URL('./lifecycle-child.mjs', import.meta.url));
let lifetime;
process.stdin.once('data', async () => {
  try {
    lifetime = await acquireLifecycle(process.argv[2], { termMs: 50, killMs: 2000 });
    if (process.argv[3] === 'orphan') {
      const child = lifetime.launch(process.execPath, [LIFECYCLE_CHILD, 'ignores-term', '--exit-after-ms', '20000', '--status', '3'], { stdio: ['ignore', 'pipe', 'inherit'] });
      await new Promise(done => child.child.stdout.once('data', done));
    }
    process.stdout.write(`${JSON.stringify(lifetime.identity)}\n`);
  } catch { process.exitCode = 2; process.stdin.destroy(); }
});
process.stdin.on('end', async () => { if (lifetime) await lifetime.cleanup(); });
