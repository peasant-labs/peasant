import { acquireLifecycle } from './owned-lifecycle.mjs';
let lifetime;
process.stdin.once('data', async () => {
  try {
    lifetime = await acquireLifecycle(process.argv[2], { termMs: 50, killMs: 2000 });
    if (process.argv[3] === 'orphan') {
      const child = lifetime.launch(process.execPath, ['-e', "process.on('SIGTERM',()=>{}); process.stdout.write('ready\\n'); setTimeout(()=>process.exit(3),20000)"], { stdio: ['ignore', 'pipe', 'inherit'] });
      await new Promise(done => child.child.stdout.once('data', done));
    }
    process.stdout.write(`${JSON.stringify(lifetime.identity)}\n`);
  } catch { process.exitCode = 2; process.stdin.destroy(); }
});
process.stdin.on('end', async () => { if (lifetime) await lifetime.cleanup(); });
