// @ts-check
/**
 * A workload child for the tests of `acquireLifecycle`: a process the owned
 * lifetime launches and then has to reap. It never touches a store; each mode
 * is one way a launched child can behave.
 *
 *   node lifecycle-child.mjs ignores-term --exit-after-ms <ms> --status <status>
 *     ignores SIGTERM, prints "ready" once its handler is in place, and exits
 *     with <status> after <ms>, so only SIGKILL ends it sooner.
 *   node lifecycle-child.mjs waits --exit-after-ms <ms>
 *     does nothing and exits 0 after <ms>, as a quiet workload does.
 */
import { parseArgs } from 'node:util';

const MODES = Object.freeze(['ignores-term', 'waits']);
const USAGE = `node lifecycle-child.mjs <${MODES.join('|')}> --exit-after-ms <ms> [--status <status>]`;

/**
 * @param {string} reason
 * @returns {never}
 */
function refuse(reason) {
  throw new Error(`The lifecycle child in web/scripts/journey/lib/lifecycle-child.mjs did not start because ${reason}. It printed nothing and holds nothing, so the test that launched it gets no workload to reap. Launch it as: ${USAGE}`);
}

/**
 * @param {string | undefined} value
 * @param {string} flag
 * @param {number} min
 * @param {number} max
 */
function integer(value, flag, min, max) {
  if (value === undefined) refuse(`its mode needs ${flag} and none was given`);
  const number = Number(value);
  if (!/^\d+$/.test(value) || !Number.isInteger(number) || number < min || number > max) {
    refuse(`${flag} is ${JSON.stringify(value)}, not a whole number from ${min} to ${max}`);
  }
  return number;
}

const { positionals, values } = (() => {
  try {
    return parseArgs({
      allowPositionals: true,
      options: { 'exit-after-ms': { type: 'string' }, status: { type: 'string' } },
    });
  } catch (error) {
    return refuse(`its arguments could not be read: ${error instanceof Error ? error.message : String(error)}`);
  }
})();
if (positionals.length !== 1) refuse(`it takes exactly one mode and was given ${JSON.stringify(positionals)}`);
const mode = positionals[0];

switch (mode) {
  case 'ignores-term': {
    const afterMs = integer(values['exit-after-ms'], '--exit-after-ms', 0, 600000);
    const status = integer(values.status, '--status', 0, 255);
    process.on('SIGTERM', () => {});
    process.stdout.write('ready\n');
    setTimeout(() => process.exit(status), afterMs);
    break;
  }
  case 'waits': {
    if (values.status !== undefined) refuse('--status is given with mode waits, which always exits 0');
    setTimeout(() => {}, integer(values['exit-after-ms'], '--exit-after-ms', 0, 600000));
    break;
  }
  default:
    refuse(`its mode is ${JSON.stringify(mode)}, not one of ${MODES.join(', ')}`);
}
