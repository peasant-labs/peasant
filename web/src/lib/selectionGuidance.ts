/**
 * Render a discovery failure for a surface. The server's message carries the
 * cause; the app adds no recovery instruction of its own.
 *
 * A legacy `selection_visibility` error keeps its place in the typed contract
 * (`DiscoveryErrorCode`), but stored browsing no longer depends on the saved
 * ingestion selection. When an older server still answers with that code, it is
 * shown as the ordinary, retryable failure it is, and the UI does not suggest
 * re-running `peasant kickstart` to reveal projects.
 */
export function discoveryErrorMessage(error: unknown): string {
  return error instanceof Error
    ? error.message
    : typeof error === 'string'
      ? error
      : 'Discovery failed without an error message. Retry, then inspect the Peasant server logs if the failure continues.';
}
