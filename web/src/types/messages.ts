/**
 * Peasant web's local WebSocket facade.
 *
 * The shared wire contract and its runtime enum/value objects come from
 * `@peasant-labs/schema`. This module keeps only the browser-specific
 * subscription visitor and UI redaction envelopes that are not part of that
 * contract.
 */

import {
  AnnotationAxis,
  ChannelTopic,
  EntryType,
  Harness,
  MessageType,
  StopReason,
  ToolCallKind,
} from '@peasant-labs/schema';
import type {
  ChannelSubscription,
  ClientMessage as SchemaClientMessage,
  ServerMessage as SchemaServerMessage,
} from '@peasant-labs/schema';

export {
  AnnotationAxis,
  ChannelTopic,
  EntryType,
  Harness,
  MessageType,
  StopReason,
  ToolCallKind,
} from '@peasant-labs/schema';

export type {
  AnnotationSummary,
  DashboardPayload,
  QualityPayload,
  QualitySession,
  Role,
  SessionDetailPayload,
  SessionOutcome,
  SessionScorecard,
  SessionSummary,
  SessionsPayload,
  ToolCallDetail,
  TrendsPayload,
  TurnDetail,
} from '@peasant-labs/schema';

/** @deprecated Pass a typed subscription from `subscribe` instead. */
export type ChannelName =
  | typeof ChannelTopic.Dashboard
  | typeof ChannelTopic.Sessions
  | typeof ChannelTopic.SessionDetail
  | typeof ChannelTopic.Trends
  | typeof ChannelTopic.Quality;

type SubscriptionBase = Omit<ChannelSubscription, 'topic' | 'axis' | 'id'>;

export type DashboardSubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.Dashboard;
};
export type SessionsSubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.Sessions;
};
export type TrendsSubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.Trends;
};
export type QualitySubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.Quality;
};
export type SessionDetailSubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.SessionDetail;
  readonly id: string;
};
export type AnnotationsSubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.Annotations;
  readonly axis: AnnotationAxis;
  readonly id: string;
};
export type ProjectFamiliaritySubscription = SubscriptionBase & {
  readonly topic: typeof ChannelTopic.ProjectFamiliarity;
  readonly id: string;
};

/** Every subscription shape accepted by the Peasant WebSocket client. */
export type SubscriptionMessage =
  | DashboardSubscription
  | SessionsSubscription
  | TrendsSubscription
  | QualitySubscription
  | SessionDetailSubscription
  | AnnotationsSubscription
  | ProjectFamiliaritySubscription;

/** Visitor over the locally refined subscription union. */
export interface SubscriptionVisitor<R> {
  dashboard(sub: DashboardSubscription): R;
  sessions(sub: SessionsSubscription): R;
  trends(sub: TrendsSubscription): R;
  quality(sub: QualitySubscription): R;
  sessionDetail(sub: SessionDetailSubscription): R;
  annotations(sub: AnnotationsSubscription): R;
  projectFamiliarity(sub: ProjectFamiliaritySubscription): R;
}

/**
 * Dispatch a subscription to its topic-specific visitor method.
 *
 * The `as <Subscription>` casts below are a deliberate, local workaround for
 * a `@peasant-labs/schema` codegen defect (every named property on a
 * generated enum const object — `ChannelTopic.Dashboard`,
 * `ChannelTopic.SessionDetail`, etc. — is typed as the FULL enum union
 * instead of its own specific literal; confirmed systemic across every
 * generated enum in enums.gen.d.ts, not just ChannelTopic). That widened
 * property type defeats TypeScript's discriminated-union narrowing here even
 * though the runtime `case` comparison is exact, so each branch is narrowed
 * by hand instead of by the compiler. This is a schema-module fix
 * (its own PR + tag, per the cross-repo contract ceremony), not a local one —
 * tracked separately, not worked around by widening `SubscriptionVisitor`.
 */
export function acceptSubscription<R>(
  sub: SubscriptionMessage,
  visitor: SubscriptionVisitor<R>,
): R {
  switch (sub.topic) {
    case ChannelTopic.Dashboard:
      return visitor.dashboard(sub as DashboardSubscription);
    case ChannelTopic.Sessions:
      return visitor.sessions(sub as SessionsSubscription);
    case ChannelTopic.Trends:
      return visitor.trends(sub as TrendsSubscription);
    case ChannelTopic.Quality:
      return visitor.quality(sub as QualitySubscription);
    case ChannelTopic.SessionDetail:
      return visitor.sessionDetail(sub as SessionDetailSubscription);
    case ChannelTopic.Annotations:
      return visitor.annotations(sub as AnnotationsSubscription);
    case ChannelTopic.ProjectFamiliarity:
      return visitor.projectFamiliarity(sub as ProjectFamiliaritySubscription);
    default:
      // Unreachable for any subscription built via `subscribe.*` or decoded
      // from a valid wire ChannelSubscription — the switch above covers
      // every ChannelTopic member. An actionable throw (not a silent
      // fallback) in case a future topic is added to the schema without a
      // matching case here.
      throw new Error(
        `acceptSubscription: unknown subscription topic ${JSON.stringify((sub as SubscriptionMessage).topic)} in src/types/messages.ts — the schema added a ChannelTopic member with no matching case; add one here and to SubscriptionVisitor.`,
      );
  }
}

/** Topic-safe factory functions for WebSocket subscriptions. */
export const subscribe = {
  dashboard: (): DashboardSubscription => ({ topic: ChannelTopic.Dashboard }),
  sessions: (): SessionsSubscription => ({ topic: ChannelTopic.Sessions }),
  trends: (): TrendsSubscription => ({ topic: ChannelTopic.Trends }),
  quality: (): QualitySubscription => ({ topic: ChannelTopic.Quality }),
  sessionDetail: (id: string): SessionDetailSubscription => ({
    topic: ChannelTopic.SessionDetail,
    id,
  }),
  annotations: (axis: AnnotationAxis, id: string): AnnotationsSubscription => ({
    topic: ChannelTopic.Annotations,
    axis,
    id,
  }),
  projectFamiliarity: (id: string): ProjectFamiliaritySubscription => ({
    topic: ChannelTopic.ProjectFamiliarity,
    id,
  }),
};

/** Stable ChannelStore key for a typed subscription. */
export function subscriptionKey(sub: SubscriptionMessage): string {
  return acceptSubscription(sub, {
    dashboard: () => ChannelTopic.Dashboard,
    sessions: () => ChannelTopic.Sessions,
    trends: () => ChannelTopic.Trends,
    quality: () => ChannelTopic.Quality,
    sessionDetail: (value) => `${ChannelTopic.SessionDetail}:${value.id}`,
    annotations: (value) =>
      `${ChannelTopic.Annotations}:${value.axis}:${value.id}`,
    projectFamiliarity: (value) =>
      `${ChannelTopic.ProjectFamiliarity}:${value.id}`,
  });
}

/** Browser-only category labels returned by the redaction review endpoint. */
export type RedactionCategory = 'CREDENTIAL' | 'PII' | 'PATH' | 'INTERNAL';

/** Browser-only review state for one proposed redaction. */
export interface Redaction {
  id: string;
  category: RedactionCategory;
  confidence: number;
  lineNumber: number;
  contextBefore: string[];
  contextAfter: string[];
  originalText: string;
  redactedReplacement: string;
  description: string;
  status: 'pending' | 'accepted' | 'rejected';
}

/** Refined server envelope used by the WebSocket dispatcher. */
export type ServerMessage = Omit<
  SchemaServerMessage,
  'type' | 'topic' | 'axis'
> & {
  type: MessageType;
  topic?: ChannelTopic;
  axis?: AnnotationAxis;
};

/** Refined client envelope used by the WebSocket sender. */
export type ClientMessage = Omit<SchemaClientMessage, 'type' | 'channels'> & {
  type: typeof MessageType.Subscribe | typeof MessageType.Unsubscribe;
  channels?: SubscriptionMessage[];
};
