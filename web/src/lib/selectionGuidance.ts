import {
  DiscoveryErrorCode,
  discoveryErrorCode,
  type DiscoveryErrorCode as DiscoveryErrorCodeValue,
} from '@/lib/api/errors';

export const SELECTION_RECOVERY_GUIDANCE =
  'Your saved project selection no longer matches the indexed data. Run `peasant kickstart` to repair the persisted selection, then retry.';

/** Preserve the server's actionable cause and add kickstart recovery only for
 * the typed persisted-selection failure class. */

export function discoveryErrorMessage(
  error: unknown,
  code?: DiscoveryErrorCodeValue,
): string {
  const message = error instanceof Error
    ? error.message
    : typeof error === 'string'
      ? error
      : 'Discovery failed without an error message. Retry, then inspect the Peasant server logs if the failure continues.';
  const resolvedCode = code ?? discoveryErrorCode(error);
  if (resolvedCode !== DiscoveryErrorCode.SelectionVisibility) return message;
  if (message.includes('peasant kickstart')) return message;
  return `${message} ${SELECTION_RECOVERY_GUIDANCE}`;
}
