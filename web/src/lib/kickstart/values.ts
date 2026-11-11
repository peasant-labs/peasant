/**
 * The closed sets of the kickstart wire, one definition site each.
 *
 * `@peasant-labs/schema` owns every token below; each set here is a projection
 * of it. A member carries its wire value and the label a surface shows. No
 * other module writes a member's wire value: a consumer compares the member
 * itself, or uses its `label`, `wire` or `String()`.
 *
 * Each set has the same four parts:
 *
 *   - the frozen members (`KickstartScope.Full`, ...);
 *   - `ALL_*`, the ordered list the tests walk;
 *   - `parseKickstart*`, the one place a wire string becomes a member. An
 *     unknown token throws a {@link KickstartValueError}; it is never relabelled
 *     as another member;
 *   - a compile-time proof that the set equals the schema's set in both
 *     directions, so a token the schema adds, renames or removes stops the
 *     build instead of dropping a row on the page.
 */

import { closed } from '@peasant-labs/fairtrade/closed-value';
import type {
  LocalKickstartCloseReason,
  LocalKickstartConnectAnswer,
  LocalKickstartDiffLineType,
  LocalKickstartEventKind,
  LocalKickstartFieldKind,
  LocalKickstartIngestState,
  LocalKickstartLineKind,
  LocalKickstartNextStep,
  LocalKickstartNodeKind,
  LocalKickstartNodeState,
  LocalKickstartResolution,
  LocalKickstartScanState,
  LocalKickstartScope,
  LocalKickstartStopKind,
} from '@peasant-labs/schema';
import { settingsSectionHref } from '@/lib/settings/catalog';

/**
 * A wire token this app does not know. The page cannot draw what it cannot
 * name, so the read stops with the set, the token and what to do. Nothing is
 * mapped to a neighbouring member.
 */
export class KickstartValueError extends TypeError {
  readonly set: string;
  readonly raw: unknown;
  readonly where: string;

  constructor(set: string, raw: unknown, where: string, accepted: readonly string[]) {
    super(
      `peasant sent ${set} ${JSON.stringify(raw) ?? String(raw)}, which this page does not know. ` +
      `it knows ${accepted.join(', ')}. ` +
      `this happened while reading ${where}. nothing was changed and the page cannot show that value. ` +
      'update peasant and its web assets to the same version, then reload.',
    );
    this.name = 'KickstartValueError';
    this.set = set;
    this.raw = raw;
    this.where = where;
  }
}

interface Wired {
  readonly wire: string;
}

/** The parser every set exports: one frozen lookup built once at load. */
function parserFor<M extends Wired>(set: string, members: readonly M[]): (raw: unknown, where?: string) => M {
  const byWire = new Map<string, M>(members.map((member) => [member.wire, member]));
  const accepted = Object.freeze(members.map((member) => member.wire));
  return (raw, where = 'a kickstart response') => {
    const member = typeof raw === 'string' ? byWire.get(raw) : undefined;
    if (member === undefined) throw new KickstartValueError(set, raw, where, accepted);
    return member;
  };
}

/** Compile-time: the client wires and the schema tokens are the same set. */
type Same<Schema extends string, Client extends string> =
  [Exclude<Schema, Client>] extends [never] ? ([Exclude<Client, Schema>] extends [never] ? true : never) : never;
type WireOf<L extends readonly Wired[]> = L[number]['wire'];

// ── scope ────────────────────────────────────────────────────────────────

/** The answer set a session collects: the whole wizard, or the selection page. */
export const KickstartScope = Object.freeze({
  Full: closed('full', 'full'),
  Selection: closed('selection', 'selection'),
} as const);
export type KickstartScope = (typeof KickstartScope)[keyof typeof KickstartScope];
export const ALL_KICKSTART_SCOPES = Object.freeze([KickstartScope.Full, KickstartScope.Selection] as const);
export const parseKickstartScope = parserFor('a kickstart scope', ALL_KICKSTART_SCOPES);
const _scope: Same<LocalKickstartScope, WireOf<typeof ALL_KICKSTART_SCOPES>> = true;

// ── stop kind ────────────────────────────────────────────────────────────

/** The layout of one stop. The stop's key is its identity; this only picks the layout. */
export const KickstartStopKind = Object.freeze({
  Connection: closed('connection', 'connection'),
  Config: closed('config', 'config'),
  Review: closed('review', 'review'),
  Ingest: closed('ingest', 'ingest'),
} as const);
export type KickstartStopKind = (typeof KickstartStopKind)[keyof typeof KickstartStopKind];
export const ALL_KICKSTART_STOP_KINDS = Object.freeze([
  KickstartStopKind.Connection,
  KickstartStopKind.Config,
  KickstartStopKind.Review,
  KickstartStopKind.Ingest,
] as const);
export const parseKickstartStopKind = parserFor('a kickstart stop kind', ALL_KICKSTART_STOP_KINDS);
const _stopKind: Same<LocalKickstartStopKind, WireOf<typeof ALL_KICKSTART_STOP_KINDS>> = true;

// ── field kind ───────────────────────────────────────────────────────────

/** The control shape of one field. The server owns everything else about it. */
export const KickstartFieldKind = Object.freeze({
  Toggle: closed('toggle', 'toggle'),
  Choice: closed('choice', 'choice'),
  Action: closed('action', 'action'),
  Tree: closed('tree', 'tree'),
  Notice: closed('notice', 'notice'),
} as const);
export type KickstartFieldKind = (typeof KickstartFieldKind)[keyof typeof KickstartFieldKind];
export const ALL_KICKSTART_FIELD_KINDS = Object.freeze([
  KickstartFieldKind.Toggle,
  KickstartFieldKind.Choice,
  KickstartFieldKind.Action,
  KickstartFieldKind.Tree,
  KickstartFieldKind.Notice,
] as const);
export const parseKickstartFieldKind = parserFor('a kickstart field kind', ALL_KICKSTART_FIELD_KINDS);
const _fieldKind: Same<LocalKickstartFieldKind, WireOf<typeof ALL_KICKSTART_FIELD_KINDS>> = true;

// ── line kind ────────────────────────────────────────────────────────────

/** One typed line of a preview or a privacy example. */
export const KickstartLineKind = Object.freeze({
  Text: closed('text', 'text'),
  Label: closed('label', 'label'),
  Before: closed('before', 'before'),
  After: closed('after', 'after'),
  Spacer: closed('spacer', 'spacer'),
} as const);
export type KickstartLineKind = (typeof KickstartLineKind)[keyof typeof KickstartLineKind];
export const ALL_KICKSTART_LINE_KINDS = Object.freeze([
  KickstartLineKind.Text,
  KickstartLineKind.Label,
  KickstartLineKind.Before,
  KickstartLineKind.After,
  KickstartLineKind.Spacer,
] as const);
export const parseKickstartLineKind = parserFor('a kickstart line kind', ALL_KICKSTART_LINE_KINDS);
const _lineKind: Same<LocalKickstartLineKind, WireOf<typeof ALL_KICKSTART_LINE_KINDS>> = true;

// ── node kind and state ──────────────────────────────────────────────────

/** One row of the selection forest. */
export const KickstartNodeKind = Object.freeze({
  Project: closed('project', 'project'),
  Group: closed('group', 'group'),
  Branch: closed('branch', 'branch'),
  Session: closed('session', 'session'),
} as const);
export type KickstartNodeKind = (typeof KickstartNodeKind)[keyof typeof KickstartNodeKind];
export const ALL_KICKSTART_NODE_KINDS = Object.freeze([
  KickstartNodeKind.Project,
  KickstartNodeKind.Group,
  KickstartNodeKind.Branch,
  KickstartNodeKind.Session,
] as const);
export const parseKickstartNodeKind = parserFor('a kickstart node kind', ALL_KICKSTART_NODE_KINDS);
const _nodeKind: Same<LocalKickstartNodeKind, WireOf<typeof ALL_KICKSTART_NODE_KINDS>> = true;

/**
 * The tri-state the server computed for a row. Ambiguity is the node's
 * `locked` flag, never a fourth state.
 */
export const KickstartNodeState = Object.freeze({
  None: closed('none', 'none'),
  Some: closed('some', 'some'),
  All: closed('all', 'all'),
} as const);
export type KickstartNodeState = (typeof KickstartNodeState)[keyof typeof KickstartNodeState];
export const ALL_KICKSTART_NODE_STATES = Object.freeze([
  KickstartNodeState.None,
  KickstartNodeState.Some,
  KickstartNodeState.All,
] as const);
export const parseKickstartNodeState = parserFor('a kickstart node state', ALL_KICKSTART_NODE_STATES);
const _nodeState: Same<LocalKickstartNodeState, WireOf<typeof ALL_KICKSTART_NODE_STATES>> = true;

// ── scan and ingest ──────────────────────────────────────────────────────

/** The discovery job's state, pushed on the topic. */
export const KickstartScanState = Object.freeze({
  Idle: closed('idle', 'waiting to scan'),
  Running: closed('running', 'scanning'),
  Done: closed('done', 'scanned'),
  Error: closed('error', 'scan failed'),
} as const);
export type KickstartScanState = (typeof KickstartScanState)[keyof typeof KickstartScanState];
export const ALL_KICKSTART_SCAN_STATES = Object.freeze([
  KickstartScanState.Idle,
  KickstartScanState.Running,
  KickstartScanState.Done,
  KickstartScanState.Error,
] as const);
export const parseKickstartScanState = parserFor('a kickstart scan state', ALL_KICKSTART_SCAN_STATES);
const _scanState: Same<LocalKickstartScanState, WireOf<typeof ALL_KICKSTART_SCAN_STATES>> = true;

/** The import's state, pushed on the topic. `busy` means an earlier import still runs. */
export const KickstartIngestState = Object.freeze({
  Idle: closed('idle', 'import finished'),
  Running: closed('running', 'importing'),
  Busy: closed('busy', 'an earlier import is still running'),
  Failed: closed('failed', 'import failed'),
} as const);
export type KickstartIngestState = (typeof KickstartIngestState)[keyof typeof KickstartIngestState];
export const ALL_KICKSTART_INGEST_STATES = Object.freeze([
  KickstartIngestState.Idle,
  KickstartIngestState.Running,
  KickstartIngestState.Busy,
  KickstartIngestState.Failed,
] as const);
export const parseKickstartIngestState = parserFor('a kickstart ingest state', ALL_KICKSTART_INGEST_STATES);
const _ingestState: Same<LocalKickstartIngestState, WireOf<typeof ALL_KICKSTART_INGEST_STATES>> = true;

// ── resolution and connect answer ────────────────────────────────────────

/** Which side of a conflict row to keep. The words are page copy; the tokens are the wire. */
export const KickstartResolution = Object.freeze({
  Mine: closed('mine', 'keep mine'),
  Theirs: closed('theirs', 'take theirs'),
} as const);
export type KickstartResolution = (typeof KickstartResolution)[keyof typeof KickstartResolution];
export const ALL_KICKSTART_RESOLUTIONS = Object.freeze([KickstartResolution.Mine, KickstartResolution.Theirs] as const);
export const parseKickstartResolution = parserFor('a kickstart resolution', ALL_KICKSTART_RESOLUTIONS);
const _resolution: Same<LocalKickstartResolution, WireOf<typeof ALL_KICKSTART_RESOLUTIONS>> = true;

/** What the person did with a village connect offer. The village stop's two actions use the same tokens. */
export const KickstartConnectAnswer = Object.freeze({
  Accept: closed('accept', 'yes'),
  Skip: closed('skip', 'no'),
} as const);
export type KickstartConnectAnswer = (typeof KickstartConnectAnswer)[keyof typeof KickstartConnectAnswer];
export const ALL_KICKSTART_CONNECT_ANSWERS = Object.freeze([KickstartConnectAnswer.Accept, KickstartConnectAnswer.Skip] as const);
export const parseKickstartConnectAnswer = parserFor('a kickstart connect answer', ALL_KICKSTART_CONNECT_ANSWERS);
const _connectAnswer: Same<LocalKickstartConnectAnswer, WireOf<typeof ALL_KICKSTART_CONNECT_ANSWERS>> = true;

// ── next step ────────────────────────────────────────────────────────────

/**
 * One finish action. The server sends the tokens and their order; this page
 * adds only where each one leads inside the app. A step is a link: following
 * it installs nothing and publishes nothing.
 */
export const KickstartNextStep = Object.freeze({
  Config: closed('config', 'change a setting later', {
    detail: 'every choice you made here can be changed in settings.',
    href: '/settings',
  }),
  WebStart: closed('web-start', 'open the dashboard', {
    detail: 'see your projects and the sessions that were just imported.',
    href: '/',
  }),
  VillageLogin: closed('village-login', 'connect to peasant online later', {
    detail: 'sign in from your account settings. connecting publishes nothing.',
    href: settingsSectionHref('account'),
  }),
  VillagePush: closed('village-push', 'publish later, explicitly', {
    detail: 'choosing sessions to publish is a separate step that you start.',
    href: '/share',
  }),
  AutoPublishSettings: closed('auto-publish-settings', 'choose automatic publishing rules', {
    detail: 'pick folders and collectives in settings. saving a rule installs no hook.',
    href: settingsSectionHref('automatic-publishing'),
  }),
} as const);
export type KickstartNextStep = (typeof KickstartNextStep)[keyof typeof KickstartNextStep];
export const ALL_KICKSTART_NEXT_STEPS = Object.freeze([
  KickstartNextStep.Config,
  KickstartNextStep.WebStart,
  KickstartNextStep.VillageLogin,
  KickstartNextStep.VillagePush,
  KickstartNextStep.AutoPublishSettings,
] as const);
export const parseKickstartNextStep = parserFor('a kickstart next step', ALL_KICKSTART_NEXT_STEPS);
const _nextStep: Same<LocalKickstartNextStep, WireOf<typeof ALL_KICKSTART_NEXT_STEPS>> = true;

// ── diff line type ───────────────────────────────────────────────────────

/** One line of the structured config diff. */
export const KickstartDiffLineType = Object.freeze({
  Add: closed('add', 'added'),
  Del: closed('del', 'removed'),
  Ctx: closed('ctx', 'unchanged'),
} as const);
export type KickstartDiffLineType = (typeof KickstartDiffLineType)[keyof typeof KickstartDiffLineType];
export const ALL_KICKSTART_DIFF_LINE_TYPES = Object.freeze([
  KickstartDiffLineType.Add,
  KickstartDiffLineType.Del,
  KickstartDiffLineType.Ctx,
] as const);
export const parseKickstartDiffLineType = parserFor('a kickstart diff line type', ALL_KICKSTART_DIFF_LINE_TYPES);
const _diffLineType: Same<LocalKickstartDiffLineType, WireOf<typeof ALL_KICKSTART_DIFF_LINE_TYPES>> = true;

// ── live topic ───────────────────────────────────────────────────────────

/** The live topic's discriminator. `session`, `answers` and `conflicts` each carry the whole view. */
export const KickstartEventKind = Object.freeze({
  Session: closed('session', 'session'),
  Answers: closed('answers', 'answers'),
  Conflicts: closed('conflicts', 'conflicts'),
  Scan: closed('scan', 'scan'),
  Ingest: closed('ingest', 'ingest'),
  Closed: closed('closed', 'closed'),
} as const);
export type KickstartEventKind = (typeof KickstartEventKind)[keyof typeof KickstartEventKind];
export const ALL_KICKSTART_EVENT_KINDS = Object.freeze([
  KickstartEventKind.Session,
  KickstartEventKind.Answers,
  KickstartEventKind.Conflicts,
  KickstartEventKind.Scan,
  KickstartEventKind.Ingest,
  KickstartEventKind.Closed,
] as const);
export const parseKickstartEventKind = parserFor('a kickstart event kind', ALL_KICKSTART_EVENT_KINDS);
const _eventKind: Same<LocalKickstartEventKind, WireOf<typeof ALL_KICKSTART_EVENT_KINDS>> = true;

/** Why a session ended. */
export const KickstartCloseReason = Object.freeze({
  Committed: closed('committed', 'saved'),
  Expired: closed('expired', 'timed out'),
} as const);
export type KickstartCloseReason = (typeof KickstartCloseReason)[keyof typeof KickstartCloseReason];
export const ALL_KICKSTART_CLOSE_REASONS = Object.freeze([KickstartCloseReason.Committed, KickstartCloseReason.Expired] as const);
export const parseKickstartCloseReason = parserFor('a kickstart close reason', ALL_KICKSTART_CLOSE_REASONS);
const _closeReason: Same<LocalKickstartCloseReason, WireOf<typeof ALL_KICKSTART_CLOSE_REASONS>> = true;

// The proofs above are compile-time only; naming them keeps them from being
// reported as unused without emitting any runtime work.
void [
  _scope, _stopKind, _fieldKind, _lineKind, _nodeKind, _nodeState, _scanState, _ingestState,
  _resolution, _connectAnswer, _nextStep, _diffLineType, _eventKind, _closeReason,
];
