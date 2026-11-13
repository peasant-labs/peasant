// @ts-check
/**
 * The process `acquireLifecycle` runs under `flock` to hold the journey's
 * ownership lock. flock takes the lock, then starts this program; the lock is
 * held for exactly as long as this program runs.
 *
 *   flock <lock-file> node flock-keeper.mjs
 *     prints "locked" once it runs (so the lock is held), then waits and exits 0
 *     when its standard input ends, which releases the lock.
 *
 * It takes no arguments: the owner ends the hold by closing the pipe.
 */
if (process.argv.length !== 2) {
  throw new Error(`The flock keeper in web/scripts/journey/lib/flock-keeper.mjs did not hold the lock because it was given arguments ${JSON.stringify(process.argv.slice(2))} and takes none. flock releases the lock when it exits, so the journey has no ownership. Launch it as: flock <lock-file> node flock-keeper.mjs`);
}
process.stdout.write('locked\n');
process.stdin.resume();
process.stdin.on('end', () => process.exit(0));
