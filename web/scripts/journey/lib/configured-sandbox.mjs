import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

/**
 * Makes a throwaway computer one that has already been set up.
 *
 * An install with no configuration file is sent to the setup page on its first
 * visit, which is the right behaviour for a new user and the wrong starting
 * point for a journey about any other page. Those journeys start the binary on
 * a computer whose configuration file already exists.
 *
 * The file states only its format version. Every other field is absent, so the
 * binary fills it from the same defaults it uses when there is no file at all:
 * the seeded computer differs from an unconfigured one in exactly one fact, the
 * one `GET /api/v1/kickstart/status` reports as `configured`.
 */
export const CONFIGURED_SANDBOX_CONFIG = 'version: 1\n';

/** The configuration file the binary reads under this environment's XDG_CONFIG_HOME. */
export function sandboxConfigPath(env) {
  const base = env?.XDG_CONFIG_HOME;
  if (typeof base !== 'string' || !base.startsWith('/')) {
    throw new Error(`The journey sandbox could not be marked as set up in sandboxConfigPath because XDG_CONFIG_HOME is ${JSON.stringify(base)}, not an absolute path. No file was written, so the binary would start unconfigured and send every page to setup. Pass the same isolated environment the binary is launched with.`);
  }
  return join(base, 'peasant', 'config.yaml');
}

/**
 * Writes the configuration file. Call it before each start of the binary and
 * after anything that clears the configuration directory. Returns the path.
 */
export function seedConfiguredSandbox(env) {
  const path = sandboxConfigPath(env);
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  writeFileSync(path, CONFIGURED_SANDBOX_CONFIG, { mode: 0o600 });
  return path;
}
