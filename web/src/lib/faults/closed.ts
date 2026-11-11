/**
 * Closed sets for the client-side fault model.
 *
 * These sets are client-only. They are not a schema surface: the server never
 * sends a fault, and every member's wire value is used only inside the web app
 * for storage keys and throwaway diagnostics.
 *
 * A member is a `ClosedValue` from `@peasant-labs/fairtrade/closed-value`, so a
 * caller compares `wire` or uses the member itself and never writes the literal
 * twice. `ALL_*` is the single list the provider and the tests walk, and the
 * `Missing*` type tests fail to compile when a new member is added without it.
 */

import { closed } from '@peasant-labs/fairtrade/closed-value';
import { ChannelTopic } from '@peasant-labs/schema';

export const FaultOperation = Object.freeze({
  QualityPage: closed('quality-page', 'quality'),
  SessionDetailRead: closed('session-detail-read', 'session detail'),
  TranscriptInitialRead: closed('transcript-initial-read', 'transcript'),
  TranscriptWindowRead: closed('transcript-window-read', 'transcript window'),
  SessionsRead: closed('sessions-read', 'sessions'),
  ProjectRead: closed('project-read', 'projects'),
  DashboardRead: closed('dashboard-read', 'dashboard'),
  TrendsRead: closed('trends-read', 'trends'),
  AnnotationsRead: closed('annotations-read', 'annotations'),
  ProjectFamiliarity: closed('project-familiarity', 'project familiarity'),
  Socket: closed('socket', 'connection'),
} as const);

export type FaultOperation = (typeof FaultOperation)[keyof typeof FaultOperation];

export const FaultCause = Object.freeze({
  PayloadTooLarge: closed('payload-too-large', 'the update is too large'),
  FrameInvalid: closed('frame-invalid', 'the update did not match this app'),
  TopicError: closed('topic-error', 'the server refused the read'),
  HttpError: closed('http-error', 'the server refused the page'),
  TranscriptReadFailed: closed('transcript-read-failed', 'the transcript read failed'),
  ConnectFailed: closed('connect-failed', 'the app is not answering'),
  SocketClosed: closed('socket-closed', 'the connection closed'),
} as const);

export type FaultCause = (typeof FaultCause)[keyof typeof FaultCause];

export const FaultRemedy = Object.freeze({
  Retry: closed('retry', 'try again'),
  LowerPageSize: closed('lower-page-size', 'lower the sessions per page'),
  NarrowProject: closed('narrow-project', 'narrow the project scope'),
  StartApp: closed('start-app', 'start it with peasant web start, then try again'),
  UpdateApp: closed('update-app', 'update peasant, then try again'),
} as const);

export type FaultRemedy = (typeof FaultRemedy)[keyof typeof FaultRemedy];

export const ALL_FAULT_OPERATIONS = Object.freeze([
  FaultOperation.QualityPage,
  FaultOperation.SessionDetailRead,
  FaultOperation.TranscriptInitialRead,
  FaultOperation.TranscriptWindowRead,
  FaultOperation.SessionsRead,
  FaultOperation.ProjectRead,
  FaultOperation.DashboardRead,
  FaultOperation.TrendsRead,
  FaultOperation.AnnotationsRead,
  FaultOperation.ProjectFamiliarity,
  FaultOperation.Socket,
] as const);
type MissingFaultOperation = Exclude<FaultOperation, (typeof ALL_FAULT_OPERATIONS)[number]>;
const _allFaultOperations: MissingFaultOperation extends never ? true : never = true;

export const ALL_FAULT_CAUSES = Object.freeze([
  FaultCause.PayloadTooLarge,
  FaultCause.FrameInvalid,
  FaultCause.TopicError,
  FaultCause.HttpError,
  FaultCause.TranscriptReadFailed,
  FaultCause.ConnectFailed,
  FaultCause.SocketClosed,
] as const);
type MissingFaultCause = Exclude<FaultCause, (typeof ALL_FAULT_CAUSES)[number]>;
const _allFaultCauses: MissingFaultCause extends never ? true : never = true;

export const ALL_FAULT_REMEDIES = Object.freeze([
  FaultRemedy.Retry,
  FaultRemedy.LowerPageSize,
  FaultRemedy.NarrowProject,
  FaultRemedy.StartApp,
  FaultRemedy.UpdateApp,
] as const);
type MissingFaultRemedy = Exclude<FaultRemedy, (typeof ALL_FAULT_REMEDIES)[number]>;
const _allFaultRemedies: MissingFaultRemedy extends never ? true : never = true;

/**
 * The operation a channel topic reads. Every topic the provider can subscribe
 * to has an operation; an unknown topic is treated as a connection fault rather
 * than silently dropped.
 */
const OPERATION_BY_TOPIC: Readonly<Record<string, FaultOperation>> = Object.freeze({
  [ChannelTopic.Dashboard]: FaultOperation.DashboardRead,
  [ChannelTopic.Sessions]: FaultOperation.SessionsRead,
  [ChannelTopic.Trends]: FaultOperation.TrendsRead,
  [ChannelTopic.Quality]: FaultOperation.QualityPage,
  [ChannelTopic.SessionDetail]: FaultOperation.SessionDetailRead,
  [ChannelTopic.Annotations]: FaultOperation.AnnotationsRead,
  [ChannelTopic.ProjectFamiliarity]: FaultOperation.ProjectFamiliarity,
});

export function faultOperationForTopic(topic: string): FaultOperation {
  return OPERATION_BY_TOPIC[topic] ?? FaultOperation.Socket;
}

/** The operation a raw server message type belongs to. */
export function faultOperationForMessageType(type: string): FaultOperation {
  return faultOperationForTopic(type);
}

// Keep the const bindings referenced so a reviewer can see the type tests are
// deliberate compile-time assertions rather than dead declarations.
void _allFaultOperations;
void _allFaultCauses;
void _allFaultRemedies;
