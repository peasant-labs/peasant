/**
 * `WebFault` — one typed, bounded, human-readable failure.
 *
 * The message is built once, in the constructor, from the typed members. A
 * surface renders `fault.message`; it never composes its own sentence from the
 * cause, the evidence, or the remedy.
 */

import type { ChannelTopic } from '@peasant-labs/schema';
import type { DiscoveryErrorCode } from '@/lib/api/errors';
import { FaultCause, FaultOperation, FaultRemedy } from './closed';
import {
  DetailEvidence,
  HttpEvidence,
  PayloadEvidence,
  TopicEvidence,
  TranscriptWindowEvidence,
  type FaultEvidence,
} from './evidence';

const webFaultBrand: unique symbol = Symbol('webFault');

/** Byte titles are binary: the app's limit is 8 MiB exactly. */
export function formatByteSize(bytes: number): string {
  const mib = bytes / (1024 * 1024);
  if (mib >= 1) {
    const whole = Math.floor(mib);
    const remainder = bytes - whole * 1024 * 1024;
    if (remainder === 0) return `${whole} MiB`;
    const rounded = Math.round(mib * 10) / 10;
    // A one-byte overflow rounds to the whole MiB, which would read as exactly
    // the limit. Keep the remainder visible instead.
    if (rounded > whole) return `${rounded.toFixed(1)} MiB`;
    return `${whole} MiB + ${formatByteSize(remainder)}`;
  }
  const kib = bytes / 1024;
  if (kib >= 1) {
    const whole = Math.floor(kib);
    const remainder = bytes - whole * 1024;
    return remainder === 0 ? `${whole} KB` : `${Math.round(kib)} KB`;
  }
  return `${bytes} B`;
}

function subject(operation: FaultOperation): string {
  return operation === FaultOperation.Socket ? 'a server' : `the ${operation.label}`;
}

function detailOf(evidence: FaultEvidence): string {
  return evidence instanceof DetailEvidence ||
    evidence instanceof HttpEvidence ||
    evidence instanceof TopicEvidence
    ? evidence.detail
    : '';
}

function buildMessage(
  operation: FaultOperation,
  cause: FaultCause,
  evidence: FaultEvidence,
  remedy: FaultRemedy,
): string {
  switch (cause) {
    case FaultCause.PayloadTooLarge: {
      if (!(evidence instanceof PayloadEvidence)) {
        throw new TypeError(
          'WebFault.payloadTooLarge in src/lib/faults/webFault.ts requires PayloadEvidence. ' +
            'A payload-too-large fault cannot name the measured size without it; ' +
            'construct it through WebFault.payloadTooLarge(operation, bytes, limit).',
        );
      }
      return `${subject(operation)} update was ${formatByteSize(evidence.bytes)}; the app accepts up to ${formatByteSize(evidence.limit)}. the update was not applied, so the page kept its last data. ${remedy.label}.`;
    }
    case FaultCause.FrameInvalid:
      return `${subject(operation)} update did not match this app${detailOf(evidence) ? `: ${detailOf(evidence)}` : ''}. the page kept its last data. update peasant, then try again.`;
    case FaultCause.TopicError: {
      const read = operation === FaultOperation.Socket ? 'read' : `${operation.label} read`;
      return `the server refused the ${read}: ${detailOf(evidence)}. the page kept its last data. try again.`;
    }
    case FaultCause.HttpError:
      return `the ${operation.label} page request failed with status ${(evidence as HttpEvidence).status}: ${detailOf(evidence)}. the page kept its last data. try again.`;
    case FaultCause.TranscriptReadFailed:
      if (evidence instanceof TranscriptWindowEvidence) {
        return `the ${operation.label} around turn ${evidence.anchor} (span ${evidence.span}) could not load: ${evidence.detail}. the surrounding transcript remains available. ${remedy.label}.`;
      }
      return `the ${operation.label} could not load: ${detailOf(evidence)}. the transcript is unavailable. ${remedy.label}.`;
    case FaultCause.ConnectFailed:
      return 'peasant is not answering on this computer. the page kept its last data. start it with peasant web start, then try again.';
    case FaultCause.SocketClosed:
      return 'the connection to peasant closed. the page kept its last data. it reconnects automatically.';
    default:
      throw new TypeError(
        `WebFault in src/lib/faults/webFault.ts received an unknown fault cause ${JSON.stringify(cause)}. ` +
          'Every renderable cause is a FaultCause member; add the new member to FaultCause, ALL_FAULT_CAUSES, and buildMessage.',
      );
  }
}

export class WebFault {
  private readonly [webFaultBrand] = true;
  readonly operation: FaultOperation;
  readonly cause: FaultCause;
  readonly evidence: FaultEvidence;
  readonly remedy: FaultRemedy;
  /** Built once, when the fault is created. */
  readonly message: string;

  private constructor(
    operation: FaultOperation,
    cause: FaultCause,
    evidence: FaultEvidence,
    remedy: FaultRemedy,
  ) {
    this.operation = operation;
    this.cause = cause;
    this.evidence = evidence;
    this.remedy = remedy;
    this.message = buildMessage(operation, cause, evidence, remedy);
  }

  static payloadTooLarge(operation: FaultOperation, bytes: number, limit: number): WebFault {
    const remedy =
      operation === FaultOperation.SessionsRead
        ? FaultRemedy.LowerPageSize
        : operation === FaultOperation.ProjectRead
          ? FaultRemedy.NarrowProject
          : FaultRemedy.Retry;
    return new WebFault(operation, FaultCause.PayloadTooLarge, new PayloadEvidence(bytes, limit), remedy);
  }

  static frameInvalid(operation: FaultOperation, detail: string): WebFault {
    return new WebFault(operation, FaultCause.FrameInvalid, new DetailEvidence(detail), FaultRemedy.UpdateApp);
  }

  static topicError(
    operation: FaultOperation,
    topic: ChannelTopic,
    detail: string,
    code?: DiscoveryErrorCode,
  ): WebFault {
    return new WebFault(
      operation,
      FaultCause.TopicError,
      new TopicEvidence(topic, detail, code),
      FaultRemedy.Retry,
    );
  }

  static httpError(
    operation: FaultOperation,
    status: number,
    detail: string,
    code?: DiscoveryErrorCode,
  ): WebFault {
    return new WebFault(
      operation,
      FaultCause.HttpError,
      new HttpEvidence(status, detail, code),
      FaultRemedy.Retry,
    );
  }

  static transcriptInitialRead(detail: string): WebFault {
    return new WebFault(FaultOperation.TranscriptInitialRead, FaultCause.TranscriptReadFailed, new DetailEvidence(detail), FaultRemedy.Retry);
  }

  static transcriptWindowRead(anchor: number, span: number, detail: string): WebFault {
    return new WebFault(FaultOperation.TranscriptWindowRead, FaultCause.TranscriptReadFailed, new TranscriptWindowEvidence(anchor, span, detail), FaultRemedy.Retry);
  }

  static connection(cause: FaultCause, detail: string): WebFault {
    if (cause !== FaultCause.ConnectFailed && cause !== FaultCause.SocketClosed && cause !== FaultCause.TopicError) {
      throw new TypeError(
        `WebFault.connection in src/lib/faults/webFault.ts received ${cause.wire}. ` +
          'A connection fault is ConnectFailed, SocketClosed, or a non-topic TopicError; ' +
          'a frame failure is created through WebFault.frameInvalid(operation, detail).',
      );
    }
    const remedy = cause === FaultCause.ConnectFailed ? FaultRemedy.StartApp : FaultRemedy.Retry;
    return new WebFault(FaultOperation.Socket, cause, new DetailEvidence(detail), remedy);
  }
}

/**
 * A frame failure or a non-topic refusal on a live socket can be retried by
 * re-reading the surface's channel. A failed or closed socket cannot: the
 * provider reconnects it on its own, so a manual control would be a second
 * retry loop.
 */
export function isRetryableConnectionFault(fault: WebFault | null): boolean {
  return (
    fault != null &&
    fault.cause !== FaultCause.SocketClosed &&
    fault.cause !== FaultCause.ConnectFailed
  );
}
