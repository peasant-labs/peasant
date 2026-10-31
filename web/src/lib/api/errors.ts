export const DiscoveryErrorCode = {
  /**
   * Legacy code from servers that applied the saved ingestion selection to a
   * stored read. Stored browsing no longer depends on that selection, but an
   * older server's answer keeps its typed code and is shown as the ordinary,
   * retryable failure it is.
   */
  SelectionVisibility: 'selection_visibility',
  HomeParentNotFound: 'home_parent_not_found',
  SyncPageScopeChanged: 'sync_page_scope_changed',
} as const;

export type DiscoveryErrorCode =
  (typeof DiscoveryErrorCode)[keyof typeof DiscoveryErrorCode];

type ErrorEnvelope = {
  error?: unknown;
  code?: unknown;
};

export class DiscoveryRequestError extends Error {
  readonly status: number;
  readonly code?: DiscoveryErrorCode;
  readonly path: string;
  /** The server's explanation alone, without the request line, for display. */
  readonly detail: string;

  constructor(path: string, status: number, message: string, code?: DiscoveryErrorCode) {
    super(`GET ${path} failed (${status}): ${message}`);
    this.name = 'DiscoveryRequestError';
    this.path = path;
    this.detail = message;
    this.status = status;
    this.code = code;
  }
}

export function parseDiscoveryError(path: string, status: number, body: string): DiscoveryRequestError {
  let envelope: ErrorEnvelope = {};
  try {
    envelope = JSON.parse(body) as ErrorEnvelope;
  } catch {
    // Non-JSON upstream failures still retain their response text below.
  }
  const message = typeof envelope.error === 'string' && envelope.error.trim().length > 0
    ? envelope.error
    : body.trim().length > 0
      ? body
      : 'the server returned no error details; retry, then inspect the Peasant server logs';
  const code = envelope.code === DiscoveryErrorCode.SelectionVisibility
    ? DiscoveryErrorCode.SelectionVisibility
    : envelope.code === DiscoveryErrorCode.HomeParentNotFound
      ? DiscoveryErrorCode.HomeParentNotFound
    : envelope.code === DiscoveryErrorCode.SyncPageScopeChanged
      ? DiscoveryErrorCode.SyncPageScopeChanged
    : undefined;
  return new DiscoveryRequestError(path, status, message, code);
}

export function discoveryErrorCode(error: unknown): DiscoveryErrorCode | undefined {
  return error instanceof DiscoveryRequestError ? error.code : undefined;
}
