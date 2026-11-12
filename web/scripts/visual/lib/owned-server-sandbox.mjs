import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { isolatedServerSandbox } from './isolated-sandbox.mjs';

/**
 * The sandbox of a server that a script starts and stops within its own run.
 *
 * It makes the throwaway root, lays the server's directories out under it (see
 * `isolatedServerSandbox`: the flags and the environment to start the binary
 * with), and removes the root when the script's process exits, however the
 * script gets there: its last line, an error, or `process.exit`. A script
 * killed by a signal leaves the root in the temporary directory, where it
 * holds only what the script's own server wrote.
 *
 * Only the root is removed. A directory the caller names lies outside it and is
 * kept, with whatever the server wrote there.
 *
 * @param {string} name names the script in the root's directory name
 * @param {{configDir?: string, dataDir?: string}} [named] directories the caller names itself and keeps
 * @param {Record<string, string | undefined>} [inherited] the environment to extend (default: the script's own)
 */
export function ownedServerSandbox(name, named = {}, inherited = process.env) {
  if (typeof name !== 'string' || !/^[a-z0-9-]+$/.test(name)) {
    throw new Error(`The server sandbox could not be made in ownedServerSandbox because its name is ${JSON.stringify(name)}, not lowercase letters, digits and dashes. Nothing was created or started. Pass the script's own short name, for example "share-geometry".`);
  }
  const root = mkdtempSync(join(tmpdir(), `peasant-${name}-`));
  let sandbox;
  try {
    sandbox = isolatedServerSandbox(root, named, inherited);
  } catch (error) {
    // A refused layout leaves nothing behind.
    rmSync(root, { recursive: true, force: true });
    throw error;
  }
  process.once('exit', () => {
    try { rmSync(sandbox.root, { recursive: true, force: true }); } catch { /* the temporary directory is cleared by the system */ }
  });
  return sandbox;
}

/**
 * The directory a person names through an environment variable of the script,
 * as an absolute path. A relative value is taken from the directory the script
 * was run in, the way a shell would take it; an unset or empty variable names
 * nothing, so that directory stays in the sandbox.
 *
 * @param {string} variable the name of the environment variable
 * @param {Record<string, string | undefined>} [environment]
 * @param {string} [workingDirectory]
 * @returns {string | undefined}
 */
export function directoryNamedBy(variable, environment = process.env, workingDirectory = process.cwd()) {
  const value = environment[variable];
  if (value === undefined || value === '') return undefined;
  return resolve(workingDirectory, value);
}
