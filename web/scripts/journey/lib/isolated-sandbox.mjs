import { join } from 'node:path';

/**
 * Where a binary that a script starts keeps everything it reads and writes.
 *
 * The binary finds its store, its configuration, its logs and its recordings
 * through five places: the data, configuration and state directories (each a
 * flag, `--data-dir`, `--config-dir`, `--state-dir`, or the XDG variable of the
 * same name), the cache directory, and HOME. A script that isolates only one of
 * them starts the binary on the person's own store for the rest: with only
 * `--config-dir` set, it opens `~/.local/share/peasant/peasant.db`. So a script
 * that owns its server takes all five from here, under one root it made, and
 * passes them both ways, as flags and as the environment.
 *
 * A caller may name a configuration directory or a data directory of its own
 * (to inspect it afterwards, or to start from a prepared store). Only the ones
 * it names are its own; every other place is still under the root. Nothing
 * here falls back to the person's directories, and a caller that wants the
 * binary on a real store has to name that store itself.
 */

function absolute(path, what) {
  if (typeof path !== 'string' || !path.startsWith('/')) {
    throw new Error(`The server sandbox could not be laid out in isolatedServerSandbox because ${what} is ${JSON.stringify(path)}, not an absolute path. Nothing was started, so no store was opened. Pass an absolute path: a relative one would be resolved against whatever directory the binary starts in.`);
  }
  return path;
}

/**
 * The directories, the flags and the environment for one server a script owns.
 *
 * @param {string} root an empty directory the script made for this run
 * @param {{configDir?: string, dataDir?: string}} [named] directories the caller names itself; an empty or missing value means "under the root"
 * @param {Record<string, string | undefined>} [inherited] the environment to extend (the script's own)
 */
export function isolatedServerSandbox(root, named = {}, inherited = {}) {
  absolute(root, 'its root');
  const ownsConfigDir = !named.configDir;
  const ownsDataDir = !named.dataDir;
  const configDir = ownsConfigDir ? join(root, 'config') : absolute(named.configDir, 'the configuration directory the caller named');
  const dataDir = ownsDataDir ? join(root, 'data') : absolute(named.dataDir, 'the data directory the caller named');
  const stateDir = join(root, 'state');
  return Object.freeze({
    root,
    configDir,
    dataDir,
    stateDir,
    ownsConfigDir,
    ownsDataDir,
    /** The flags that name the three directories the binary takes as flags. */
    args: Object.freeze(['--config-dir', configDir, '--data-dir', dataDir, '--state-dir', stateDir]),
    /** The environment for the binary: the same directories, plus the two it has no flag for. */
    env: Object.freeze({
      ...inherited,
      HOME: join(root, 'home'),
      XDG_CONFIG_HOME: configDir,
      XDG_DATA_HOME: dataDir,
      XDG_STATE_HOME: stateDir,
      XDG_CACHE_HOME: join(root, 'cache'),
    }),
  });
}
