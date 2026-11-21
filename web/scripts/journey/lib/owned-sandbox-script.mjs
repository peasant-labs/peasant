// @ts-check
/**
 * A visual script reduced to what `ownedServerSandbox` sees of it, for the
 * tests of that helper: it makes its sandbox, writes a store into the data
 * directory as its server would, prints what it made as one JSON line (with the
 * configuration file the server would read, or null when there is none), and
 * ends the way it is told. It never starts a server.
 *
 *   node owned-sandbox-script.mjs --name <script> [--config-dir <dir>] [--data-dir <dir>]
 *     [--home <dir>] [--keep] --end <exit|uncaught-error|last-line> [--status <status>]
 *
 * `--name` is the script name the sandbox root is named for. `--config-dir`,
 * `--data-dir` and `--home` are the directories the script names itself, passed to
 * `ownedServerSandbox` as given, so a refused one is refused there. `--keep` asks
 * for a root the script removes itself instead of one removed when it exits. `--end`
 * says how the process ends: `exit` calls process.exit with `--status`,
 * `uncaught-error` throws, `last-line` runs off the end of the module.
 */
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { parseArgs } from 'node:util';
import { ownedServerSandbox } from '../../visual/lib/owned-server-sandbox.mjs';

const ENDS = Object.freeze(['exit', 'uncaught-error', 'last-line']);
const USAGE = `node owned-sandbox-script.mjs --name <script> [--config-dir <dir>] [--data-dir <dir>] [--home <dir>] [--keep] --end <${ENDS.join('|')}> [--status <status>]`;

/**
 * @param {string} reason
 * @returns {never}
 */
function refuse(reason) {
  throw new Error(`The sandbox script in web/scripts/journey/lib/owned-sandbox-script.mjs did not run because ${reason}. No sandbox was made. Launch it as: ${USAGE}`);
}

const { values } = (() => {
  try {
    return parseArgs({
      options: {
        name: { type: 'string' },
        'config-dir': { type: 'string' },
        'data-dir': { type: 'string' },
        home: { type: 'string' },
        keep: { type: 'boolean' },
        end: { type: 'string' },
        status: { type: 'string' },
      },
    });
  } catch (error) {
    return refuse(`its arguments could not be read: ${error instanceof Error ? error.message : String(error)}`);
  }
})();

if (values.name === undefined) refuse('--name is missing');
const end = values.end;
if (end === undefined || !ENDS.includes(end)) refuse(`--end is ${JSON.stringify(end)}, not one of ${ENDS.join(', ')}`);
let status = 0;
if (end === 'exit') {
  const given = values.status;
  if (given === undefined || !/^\d+$/.test(given) || Number(given) > 255) refuse(`--end exit needs --status as a whole number from 0 to 255, and it is ${JSON.stringify(given)}`);
  status = Number(given);
} else if (values.status !== undefined) {
  refuse(`--status is given with --end ${end}, which does not exit with a status`);
}

/** @type {{configDir?: string, dataDir?: string, home?: string}} */
const named = {};
if (values['config-dir'] !== undefined) named.configDir = values['config-dir'];
if (values['data-dir'] !== undefined) named.dataDir = values['data-dir'];
if (values.home !== undefined) named.home = values.home;

const sandbox = ownedServerSandbox(values.name, named, process.env, { removeOnExit: values.keep !== true });
// The file the binary reads below the configuration directory it is started with.
const configFile = join(sandbox.configDir, 'peasant', 'config.yaml');
const config = existsSync(configFile) ? readFileSync(configFile, 'utf8') : null;
mkdirSync(join(sandbox.dataDir, 'peasant'), { recursive: true });
writeFileSync(join(sandbox.dataDir, 'peasant', 'peasant.db'), 'x');
console.log(JSON.stringify({
  root: sandbox.root,
  dataDir: sandbox.dataDir,
  configDir: sandbox.configDir,
  config,
  home: sandbox.env.HOME,
  existed: existsSync(sandbox.root),
}));

if (end === 'exit') process.exit(status);
if (end === 'uncaught-error') throw new Error('the script failed');
