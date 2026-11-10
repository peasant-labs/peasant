/**
 * Bridge from the REST control-flow error to the client fault model.
 *
 * `DiscoveryRequestError` stays the REST type: `useHomeSessions` still reads its
 * `code` to restart a changed page scope. The same error also becomes a
 * `WebFault`, and the code rides on `HttpEvidence` so one error drives both the
 * control flow and the message. The code is never rendered.
 */

import { DiscoveryRequestError, type DiscoveryErrorCode } from '@/lib/api/errors';
import type { FaultOperation } from './closed';
import { HttpEvidence, TopicEvidence } from './evidence';
import { WebFault } from './webFault';

/** Build the typed fault for a failed REST read, or `null` for an unknown error. */
export function faultFromDiscoveryError(operation: FaultOperation, error: unknown): WebFault | null {
  if (!(error instanceof DiscoveryRequestError)) return null;
  return WebFault.httpError(operation, error.status, error.detail, error.code);
}

/** The control-flow code a fault carries, if any. */
export function codeFromFault(fault: WebFault | null): DiscoveryErrorCode | undefined {
  if (fault?.evidence instanceof HttpEvidence) return fault.evidence.code;
  if (fault?.evidence instanceof TopicEvidence) return fault.evidence.code;
  return undefined;
}
