import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

/** Mutate one compiled semantic anchor; never accept missing or ambiguous evidence. */
export function mutateCompiledAsset(original, mutation) {
  if (Boolean(mutation.find) === Boolean(mutation.findRegex)) throw new Error(`Mutation ${mutation.name} requires one literal or regex anchor`);
  const pattern = mutation.findRegex ? new RegExp(mutation.findRegex, 'g') : null;
  const matches = pattern ? [...original.matchAll(pattern)].length : original.split(mutation.find).length - 1;
  if (matches === 0) return null;
  if (matches !== 1) throw new Error(`Mutation ${mutation.name} anchor is not unique; update its fixture before claiming rejection`);
  const changed = original.replace(pattern ?? mutation.find, mutation.replace);
  // A syntax error is not an observed semantic rejection, so the mutant must still parse.
  requireModuleSyntax(changed, mutation.name);
  return changed;
}

/**
 * Parse `source` as an ES module without evaluating it: `node --check` on a
 * private temporary `.mjs` file. Every compiled chunk the app serves parses as
 * a module, and module syntax (`import`/`export`) is accepted.
 */
function requireModuleSyntax(source, name) {
  const directory = mkdtempSync(join(tmpdir(), 'peasant-compiled-mutation-'));
  try {
    const file = join(directory, 'mutated-chunk.mjs');
    writeFileSync(file, source, { flag: 'wx', mode: 0o600 });
    const result = spawnSync(process.execPath, ['--check', file], { encoding: 'utf8', timeout: 30000 });
    if (result.error) {
      throw new Error(`Mutation ${name} could not be syntax-checked: running "${process.execPath} --check" failed (${result.error.message}) in mutateCompiledAsset (web/scripts/journey/lib/compiled-mutation.mjs) after the anchor was replaced. No mutant was served, so this mutation proves nothing; make sure the Node executable running the journey can start a child process, then retry.`);
    }
    if (result.status !== 0) {
      throw new Error(`Mutation ${name} produced a compiled chunk that no longer parses as an ES module, so the app would fail on a syntax error rather than on the behaviour the mutation removes. mutateCompiledAsset (web/scripts/journey/lib/compiled-mutation.mjs) refused it after replacing the anchor; no mutant was served. Fix the mutation's replace text in its fixture so the result still parses. node --check reported:\n${result.stderr.trim()}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}
