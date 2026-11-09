/**
 * `FaultStore` — retained data faults, keyed by operation plus request key.
 *
 * One key is one (operation, request) pair, so a success for one operation or
 * one session never clears another. The store holds no connection fault: the
 * provider owns the socket's fault, because a socket failure is not any one
 * channel's data failure.
 */

import type { FaultOperation } from './closed';
import type { WebFault } from './webFault';

const SEPARATOR = '\u0000';

export class FaultStore {
  private readonly faults = new Map<string, WebFault>();

  private static key(operation: FaultOperation, requestKey: string): string {
    return `${operation.wire}${SEPARATOR}${requestKey}`;
  }

  get(operation: FaultOperation, requestKey: string): WebFault | null {
    return this.faults.get(FaultStore.key(operation, requestKey)) ?? null;
  }

  /** Store a fault. Returns whether the stored fault changed. */
  set(operation: FaultOperation, requestKey: string, fault: WebFault): boolean {
    if (fault.operation !== operation) {
      throw new TypeError(
        `FaultStore.set in src/lib/faults/faultStore.ts received a ${fault.operation.wire} fault for the ${operation.wire} key. ` +
          'A fault is stored under its own operation and request key; pass the operation the fault was created for.',
      );
    }
    const key = FaultStore.key(operation, requestKey);
    if (this.faults.get(key) === fault) return false;
    this.faults.set(key, fault);
    return true;
  }

  /** Clear one operation's fault for one request. Returns whether one was present. */
  clear(operation: FaultOperation, requestKey: string): boolean {
    return this.faults.delete(FaultStore.key(operation, requestKey));
  }

  /** Clear every operation's fault for one request key. */
  clearRequest(requestKey: string): void {
    const suffix = `${SEPARATOR}${requestKey}`;
    for (const key of this.faults.keys()) {
      if (key.endsWith(suffix)) this.faults.delete(key);
    }
  }

  /** Number of retained faults. Diagnostics and tests only. */
  get size(): number {
    return this.faults.size;
  }
}
