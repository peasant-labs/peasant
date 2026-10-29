/** Mutate one compiled semantic anchor; never accept missing or ambiguous evidence. */
export function mutateCompiledAsset(original, mutation) {
  if (Boolean(mutation.find) === Boolean(mutation.findRegex)) throw new Error(`Mutation ${mutation.name} requires one literal or regex anchor`);
  const pattern = mutation.findRegex ? new RegExp(mutation.findRegex, 'g') : null;
  const matches = pattern ? [...original.matchAll(pattern)].length : original.split(mutation.find).length - 1;
  if (matches === 0) return null;
  if (matches !== 1) throw new Error(`Mutation ${mutation.name} anchor is not unique; update its fixture before claiming rejection`);
  const changed = original.replace(pattern ?? mutation.find, mutation.replace);
  new Function(changed); // Compile only: a syntax error is not an observed semantic rejection.
  return changed;
}
