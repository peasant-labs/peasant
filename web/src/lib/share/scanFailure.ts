/**
 * What a failed redaction scan means to the person publishing. A stale local
 * index is healed by the popup's re-scan (it re-indexes this one session, then
 * scans again), so it gets one short line. Any other failure keeps its actionable
 * message, the what, why and fix the redaction invariants require to stay
 * visible, without the transport wrapper: no HTTP status, no JSON envelope.
 */
export type ScanFailureKind = 'needs-reindex' | 'other';

export interface ScanFailure {
  kind: ScanFailureKind;
  /** The one line the dialog's failure box shows. */
  line: string;
  /** The server's whole reason without the transport wrapper, for the details view. */
  message: string;
  /** The text as it arrived. */
  detail: string;
}

/**
 * The local index, not the recording, is at fault, and re-indexing rewrites
 * it: the stored session holds evidence the strict publication reader refuses
 * (ingest.EvidenceIntegrityError and LegacyPositionUnavailableError share the
 * first prefix), or its stored capture is not a verified one yet (the store's
 * publication input refusal names the second).
 */
const STALE_INDEX = ['retain unknown source evidence in ingest', 'publication metadata needs ingest'];

/** fetchRedactionScan's wrapper around a refused GET /api/v1/sync/redactions. */
const TRANSPORT = /^redaction scan failed \(\d+\):\s*/;

/** The server's own message, out of its `{"error": ...}` envelope when it has one. */
function unwrap(detail: string): string {
  const body = detail.replace(TRANSPORT, '').trim();
  try {
    const parsed = JSON.parse(body) as { error?: unknown };
    if (parsed && typeof parsed.error === 'string' && parsed.error.trim()) return parsed.error.trim();
  } catch {
    // not JSON: the message is the body itself
  }
  return body || 're-scan to try again.';
}

/**
 * Text the line never carries: JSON, a web address, or the online service's
 * name. A reason with any of them waits behind details. Every other reason
 * stays in the line, on one line: a refused category or count is the what,
 * why and fix the redaction invariants keep visible.
 */
const NOT_FOR_THE_LINE = /[{}]|https?:\/\/|village|peasant online/i;

/** The failure line's reason: the server's own words, on one line, unless they cannot stand there. */
function lineReason(message: string): string {
  const reason = message.replace(/\s+/g, ' ').trim();
  if (reason === '' || NOT_FOR_THE_LINE.test(reason)) return 'details say why.';
  return /[.!?]$/.test(reason) ? reason : `${reason}.`;
}

export function classifyScanFailure(detail: string): ScanFailure {
  const message = unwrap(detail);
  if (STALE_INDEX.some((marker) => detail.includes(marker))) {
    return { kind: 'needs-reindex', line: 'this session needs a quick re-index on this computer before it can be published.', message, detail };
  }
  return { kind: 'other', line: `the scan failed, so publish is off. ${lineReason(message)}`, message, detail };
}

/** Why the one-session re-index stopped, as the client and the server name it. */
export type ReindexFailureCode =
  | 'ingest_running'
  | 'session_ingest_timeout'
  | 'session_not_recorded'
  | 'unreachable'
  | 'other';

const REINDEX_LINES: Record<ReindexFailureCode, string> = {
  ingest_running: 'another index update is running on this computer; try again when it finishes.',
  session_ingest_timeout: 'updating this session’s index took too long and was stopped.',
  session_not_recorded: 'this session is not recorded on this computer yet.',
  unreachable: 'the peasant app on this computer stopped answering.',
  other: 'details say why.',
};

/** Whether running the re-index again can fix it: a session not recorded here cannot be re-indexed. */
export function reindexRetryable(code: ReindexFailureCode): boolean {
  return code !== 'session_not_recorded';
}

/** Why a re-index did not finish, in one plain line. */
export function reindexReason(code: ReindexFailureCode): string {
  return REINDEX_LINES[code];
}

/** The line for a re-index that did not finish; its whole reason goes to details. */
export function reindexFailureLine(code: ReindexFailureCode): string {
  return `the re-index did not finish, so publish is off. ${reindexReason(code)}`;
}

/** The line for a re-index that finished while the scan still refuses the session. */
export const REINDEX_DID_NOT_REPAIR = 'the re-index ran, but the scan still refuses this session, so publish is off. details say why.';
