/**
 * Branded evidence for a `WebFault`.
 *
 * Every class carries a compile-time brand and no `kind` string: `instanceof`
 * is the runtime discriminator, so a reader cannot branch on a string that the
 * type system does not police. A detail is capped at 512 characters at the
 * boundary; the fault message never carries transcript text or raw payload
 * bytes.
 */

import type { ChannelTopic } from '@peasant-labs/schema';
import type { DiscoveryErrorCode } from '@/lib/api/errors';

/** The longest detail any evidence may carry, in characters. */
export const MAX_EVIDENCE_DETAIL = 512;

/**
 * Bound a server- or schema-supplied detail so a fault message cannot echo an
 * arbitrary payload. Never pass transcript content here.
 */
export function boundedDetail(detail: string): string {
  return detail.length > MAX_EVIDENCE_DETAIL ? detail.slice(0, MAX_EVIDENCE_DETAIL) : detail;
}

const payloadEvidenceBrand: unique symbol = Symbol('payloadEvidence');
const topicEvidenceBrand: unique symbol = Symbol('topicEvidence');
const httpEvidenceBrand: unique symbol = Symbol('httpEvidence');
const detailEvidenceBrand: unique symbol = Symbol('detailEvidence');
const transcriptWindowEvidenceBrand: unique symbol = Symbol('transcriptWindowEvidence');

/** A frame measured over the app's byte limit. */
export class PayloadEvidence {
  private readonly [payloadEvidenceBrand] = true;
  readonly bytes: number;
  readonly limit: number;

  constructor(bytes: number, limit: number) {
    this.bytes = bytes;
    this.limit = limit;
  }
}

/** A server refusal scoped to one channel topic. */
export class TopicEvidence {
  private readonly [topicEvidenceBrand] = true;
  readonly topic: ChannelTopic;
  readonly detail: string;
  /** Kept for control flow only; never rendered. */
  readonly code?: DiscoveryErrorCode;

  constructor(topic: ChannelTopic, detail: string, code?: DiscoveryErrorCode) {
    this.topic = topic;
    this.detail = boundedDetail(detail);
    this.code = code;
  }
}

/** A REST request that failed, with the type code retained for control flow. */
export class HttpEvidence {
  private readonly [httpEvidenceBrand] = true;
  readonly status: number;
  readonly detail: string;
  /** Kept for control flow only; never rendered. */
  readonly code?: DiscoveryErrorCode;

  constructor(status: number, detail: string, code?: DiscoveryErrorCode) {
    this.status = status;
    this.detail = boundedDetail(detail);
    this.code = code;
  }
}

/** A bounded, human-readable detail with no structured evidence to carry. */
export class DetailEvidence {
  private readonly [detailEvidenceBrand] = true;
  readonly detail: string;

  constructor(detail: string) {
    this.detail = boundedDetail(detail);
  }
}

/** The exact window requested and a safe explanation of the read failure. */
export class TranscriptWindowEvidence {
  private readonly [transcriptWindowEvidenceBrand] = true;
  readonly detail: string;

  constructor(readonly anchor: number, readonly span: number, detail: string) {
    this.detail = boundedDetail(detail);
  }
}

export type FaultEvidence = PayloadEvidence | TopicEvidence | HttpEvidence | DetailEvidence | TranscriptWindowEvidence;
