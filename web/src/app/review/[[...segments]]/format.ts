/**
 * Small display formatters for the Changes surface. Pure functions — no React.
 */

/** Unicode minus (U+2212) used by removal counts in footnotes and captions. */
export const MINUS = '−';

/** Pluralize a counted noun: `plural(1, 'task') === '1 task'`. */
export function plural(n: number, word: string): string {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}

/**
 * Relative time for the "last work" column, such as "2h ago" or "1d ago".
 * Deterministic given `nowMs`; defaults to the wall clock.
 */
export function formatRelative(ms: number, nowMs: number = Date.now()): string {
  const diff = Math.max(0, nowMs - ms);
  const mins = Math.floor(diff / 60_000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

/** Abbreviated commit hash for plain commit rows. */
export function shortHash(hash: string): string {
  return hash.slice(0, 7);
}

/** Abbreviated session id for the work rail (full id stays in `title=`). */
export function shortSessionId(id: string): string {
  return id.length > 10 ? `${id.slice(0, 10)}…` : id;
}
