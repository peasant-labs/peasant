/**
 * The mounted-catalog runner's argument surface and the selection record it writes to the
 * build manifest. The whole catalog is the default evidence run; a local investigation may
 * narrow the mounted cases with Playwright's `--grep`. Any other argument is refused before
 * the build, so an evidence command cannot silently change the projects, the ports, or the
 * fixture the suite runs against.
 */
export function selectionArgs(raw) {
  const args = raw[0] === '--' ? raw.slice(1) : raw;
  if (args.length === 0) return [];
  const [flag, value] = args;
  const inline = flag.startsWith('--grep=') ? flag.slice('--grep='.length) : undefined;
  const pattern = flag === '--grep' && args.length === 2 ? value : inline !== undefined && args.length === 1 ? inline : '';
  if (pattern.trim()) return ['--grep', pattern];
  throw new Error(`journey:collective-observations got ${JSON.stringify(raw)}; it runs the whole catalog by default and accepts only --grep <pattern> to narrow the mounted cases`);
}

/**
 * What the manifest records about the requested run, before any case executes. Counterfactual
 * evidence keeps its fixed dark-desktop project and ignores arguments, so it is never a full
 * catalog run either.
 */
export function selectionRecord({ counterfactual, args }) {
  const forwardedArguments = counterfactual ? ['--project', 'dark-desktop'] : selectionArgs(args);
  return {
    requestedSelection: {
      counterfactual,
      arguments: args,
      forwardedArguments,
      grep: forwardedArguments[0] === '--grep' ? forwardedArguments[1] : null,
    },
    fullCatalogRequested: !counterfactual && forwardedArguments.length === 0,
  };
}
