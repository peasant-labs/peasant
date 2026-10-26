/**
 * The plain words a publish popup line may carry for a reason the server or
 * the network gave. A reason that says peasant online cannot be reached reads
 * "can’t reach peasant online right now". A reason that carries transport text
 * (a web address, a host and port, a Go request error, an HTTP status, JSON,
 * or the online service's internal name) carries nothing the reader can act
 * on, so the line keeps only its step and the reason goes to details. A
 * `peasant village …` command is a remedy, not that name, and stays. Any other
 * reason stays, on one line.
 */
const UNREACHABLE = /connection refused|dial tcp|no such host|i\/o timeout|network is unreachable|could not be reached|failed to fetch|econnrefused|connection reset|deadline exceeded|timed out/i;
const TRANSPORT = /https?:\/\/|\/api\/v\d|\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b|localhost:\d+|\b(?:get|post|put|patch|delete) "|\(\d{3}\)|\bhttp\b|[{}]|(?<!peasant )village/i;

export const UNREACHABLE_REASON = 'can’t reach peasant online right now';

/** One line of a message: its first line, trimmed, without a trailing stop. */
function firstLine(text: string, limit = 220): string {
  const line = text.split('\n')[0].trim().replace(/[.;:\s]+$/, '');
  return line.length > limit ? `${line.slice(0, limit - 1)}…` : line;
}

/**
 * The reason a line may show, or null when only details may carry it. A
 * reason's leading clauses stay up to the first clause that carries transport
 * text: "sharing failed: village answered 502" keeps "sharing failed". A Local
 * API path counts as transport text too.
 */
export function plainReason(message: string | undefined | null): string | null {
  const text = firstLine(message ?? '', Number.MAX_SAFE_INTEGER);
  if (!text) return null;
  if (UNREACHABLE.test(text)) return UNREACHABLE_REASON;
  const kept: string[] = [];
  for (const clause of text.split(/:\s+/)) {
    if (TRANSPORT.test(clause)) break;
    kept.push(clause);
  }
  return kept.length ? firstLine(kept.join(': ')) : null;
}

/** A step and, when it may stand there, its reason: "reading your collectives: can’t reach peasant online right now". */
export function stepWithReason(step: string, message: string | undefined | null): string {
  const reason = plainReason(message);
  return reason ? `${step}: ${reason}` : step;
}

/**
 * The informational line for a copy that stops before a recording line the
 * re-index could not read. It never blocks the publish: what is sent is
 * complete up to that line, and every check runs on it.
 */
export function unreadableNote(unreadable: readonly { line: number; reason: string }[]): { line: string; detail: string } | undefined {
  const first = unreadable[0];
  if (!first) return undefined;
  return {
    line: '1 line in this session couldn’t be read, so the copy being published stops before it.',
    detail: `line ${first.line} of the recording: ${first.reason}`,
  };
}
