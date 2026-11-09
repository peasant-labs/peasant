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
  type FaultEvidence,
} from './evidence';

const webFaultBrand: unique symbol = Symbol('webFault');

/** Byte titles are binary: the app's limit is 8 MiB exactly. */
export function formatByteSize(bytes: number): string {
  const mib = bytes / (1024 * 1024);
  if (mib >= 1) return `${round(mib)} MiB`;
  const kib = bytes / 1024;
  if (kib >= 1) return `${Math.round(kib)} KB`;
  return `${bytes} bytes`;
}

function round(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(1);
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
      operation === FaultOperation.SessionsRead || operation === FaultOperation.ProjectRead
        ? FaultRemedy.LowerPageSize
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
