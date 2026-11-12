// @ts-check
/**
 * A stand-in for the server a visual script starts, for the tests of
 * `startOwnedServer`. It never opens a store; each mode is one way a started
 * server can behave.
 *
 *   node fake-server.mjs answers --port <port>
 *     answers GET /api/v1/health on localhost:<port>, as a running server does.
 *   node fake-server.mjs answers-and-notifies --port <port>
 *     the same, and sends "answered" to the parent over IPC after each answer;
 *     ends when the parent disconnects.
 *   node fake-server.mjs exits --status <status>
 *     exits at once with <status>, as a server that cannot start does.
 *   node fake-server.mjs silent
 *     runs and never listens, as a server stuck before it binds does.
 *   node fake-server.mjs exits-when-told --status <status> --after-ms <ms>
 *     on the first IPC message from the parent, waits <ms> and exits with
 *     <status>, as a server that lost its port to another process does.
 */
import { createServer } from 'node:http';
import { parseArgs } from 'node:util';

const MODES = Object.freeze(['answers', 'answers-and-notifies', 'exits', 'silent', 'exits-when-told']);
const USAGE = `node fake-server.mjs <${MODES.join('|')}> [--port <port>] [--status <status>] [--after-ms <ms>]`;

/**
 * @param {string} reason
 * @returns {never}
 */
function refuse(reason) {
  throw new Error(`The fake server in web/scripts/journey/lib/fake-server.mjs did not start because ${reason}. Nothing was started or listened on, so the test that launched it gets no server. Launch it as: ${USAGE}`);
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

/** @returns {NonNullable<typeof process.send>} */
function parentChannel() {
  if (typeof process.send !== 'function') refuse('its mode talks to the parent over IPC and it was launched without an IPC channel (add "ipc" to the stdio of the spawn)');
  return process.send.bind(process);
}

/** @param {number} port @param {() => void} [answered] */
function answerHealth(port, answered = () => {}) {
  createServer((_request, response) => { response.end('{}'); answered(); }).listen(port, 'localhost');
}

const { positionals, values } = (() => {
  try {
    return parseArgs({
      allowPositionals: true,
      options: { port: { type: 'string' }, status: { type: 'string' }, 'after-ms': { type: 'string' } },
    });
  } catch (error) {
    return refuse(`its arguments could not be read: ${error instanceof Error ? error.message : String(error)}`);
  }
})();
if (positionals.length !== 1) refuse(`it takes exactly one mode and was given ${JSON.stringify(positionals)}`);
const mode = positionals[0];

switch (mode) {
  case 'answers':
    answerHealth(integer(values.port, '--port', 1, 65535));
    break;
  case 'answers-and-notifies': {
    const send = parentChannel();
    process.once('disconnect', () => process.exit(0));
    answerHealth(integer(values.port, '--port', 1, 65535), () => send('answered'));
    break;
  }
  case 'exits':
    process.exit(integer(values.status, '--status', 0, 255));
    break;
  case 'silent':
    setInterval(() => {}, 1000);
    break;
  case 'exits-when-told': {
    const status = integer(values.status, '--status', 0, 255);
    const afterMs = integer(values['after-ms'], '--after-ms', 0, 60000);
    parentChannel();
    process.once('message', () => setTimeout(() => process.exit(status), afterMs));
    break;
  }
  default:
    refuse(`its mode is ${JSON.stringify(mode)}, not one of ${MODES.join(', ')}`);
}
