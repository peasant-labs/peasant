/**
 * What the transcript page's publish bar and popup show, derived from the Local
 * API answers the page already holds.
 *
 * Nothing here decides a rule the server owns. The bar and popup restate the
 * server's publication state, audience, collectives and push result in
 * fairtrade's vocabulary; the one comparison made here is "N new turns", the
 * turns this page received that are newer than the publication village holds.
 */

import type {
  LocalPublication,
  LocalPublicationAudienceMember,
  LocalVillageCollective,
  SyncPushRequest,
  SyncPushResponse,
  SyncPushStepResult,
} from '@peasant-labs/schema';
import type { PublishAccessItem as AccessItem, PublishState } from '@/lib/ft-ui';
import type { SelectableRedactionLevel } from '@/lib/share/redactions';
import type { Redaction } from '@/types/messages';

export type { AccessItem };

/** One suggestion in the collective picker. */
export interface PickerSuggestion {
  id: string;
  name: string;
  members?: number;
  note?: string;
}

/** A redaction match in the shape fairtrade's RedactionReview takes. */
export interface ReviewMatch {
  id: string;
  category: string;
  confidence: number;
  before: string;
  after: string;
  kept: boolean;
}

// ---------------------------------------------------------------------------
// The bar
// ---------------------------------------------------------------------------

/**
 * How many of the page's turns are newer than the publication. A turn whose
 * timestamp cannot be read is not counted: the count only ever names turns it
 * can show are new.
 */
export function countNewTurns(turns: readonly { timestamp: string }[], publishedAt: string | undefined): number {
  if (!publishedAt) return 0;
  const published = Date.parse(publishedAt);
  if (Number.isNaN(published)) return 0;
  return turns.filter((turn) => {
    const at = Date.parse(turn.timestamp);
    return !Number.isNaN(at) && at > published;
  }).length;
}

/** The collectives that can read the transcript now: the approved shares. */
export function approvedReaders(publication: LocalPublication | null): LocalPublicationAudienceMember[] {
  return (publication?.audience ?? []).filter((member) => member.status === 'approved');
}

export interface BarModel {
  state: PublishState;
  collectives?: number;
  newTurns?: number;
  collective?: string;
}

/**
 * The bar's status. A publish in flight wins; then a session this computer has
 * not published is `not published` (or outside the saved lists); a published one
 * is under auto-publish, has new turns, or is up to date. The collective count
 * is left out when village could not be asked who reads the transcript.
 */
export function barModel(options: {
  publication: LocalPublication;
  audienceKnown: boolean;
  newTurns: number;
  publishingTo?: number;
}): BarModel {
  const { publication, audienceKnown, newTurns, publishingTo } = options;
  if (publishingTo !== undefined) return { state: 'publishing', collectives: publishingTo };
  const readers = approvedReaders(publication);
  const collectives = audienceKnown ? readers.length : undefined;
  if (publication.state !== 'published') {
    if (publication.outsideSelection) return { state: 'outside-lists' };
    if (publication.autoPublish) return { state: 'auto-publish' };
    return { state: 'not-published' };
  }
  if (publication.autoPublish) {
    return { state: 'auto-publish', collective: readers.length ? readers.map((reader) => reader.name).join(', ') : undefined };
  }
  if (newTurns > 0) return { state: 'new-turns', collectives, newTurns };
  return { state: 'published', collectives };
}

// ---------------------------------------------------------------------------
// Who can read it
// ---------------------------------------------------------------------------

/**
 * The change of audience the reader has asked for in the popup, by collective
 * id, in the order they asked. A first publish starts with the suggested
 * collectives added; an update starts with no change, so it keeps the audience
 * the transcript has.
 */
export interface AccessDraft {
  add: string[];
  remove: string[];
}

export const NO_ACCESS_CHANGE: AccessDraft = Object.freeze({ add: [], remove: [] }) as AccessDraft;

export function initialDraft(mode: 'publish' | 'update', collectives: readonly LocalVillageCollective[]): AccessDraft {
  if (mode === 'update') return NO_ACCESS_CHANGE;
  return { add: collectives.filter((collective) => collective.suggestion).map((collective) => collective.group.id), remove: [] };
}

/** The server's suggestion reason, in words. The server compared the remotes. */
export function suggestionNote(collective: LocalVillageCollective): string | undefined {
  const suggestion = collective.suggestion;
  if (!suggestion) return undefined;
  if (suggestion.reason === 'linked_repository') {
    return `suggested · repo ${suggestion.match.replace(/^github\.com:/, '')} is linked`;
  }
  return `suggested · github org ${suggestion.match}`;
}

/**
 * A curated collective holds a new share for its owner's approval. This is
 * copy only: the push result says what village did.
 */
function waitsForApproval(collective: LocalVillageCollective | undefined): boolean {
  return collective?.group.acceptance_mode === 'curated' && collective.group.role !== 'owner';
}

/** The rows of "who can read it" after this publish. */
export function accessItems(options: {
  mode: 'publish' | 'update';
  publication: LocalPublication | null;
  collectives: readonly LocalVillageCollective[];
  draft: AccessDraft;
}): AccessItem[] {
  const { mode, publication, collectives, draft } = options;
  const byId = new Map(collectives.map((collective) => [collective.group.id, collective]));
  const items: AccessItem[] = [];
  if (mode === 'update') {
    for (const member of publication?.audience ?? []) {
      if (member.status !== 'approved' && member.status !== 'pending') continue;
      const collective = byId.get(member.collectiveId);
      items.push({
        id: member.collectiveId,
        name: member.name,
        members: collective?.group.member_count,
        pending: draft.remove.includes(member.collectiveId) ? 'removal' : member.status === 'pending' ? 'approval' : undefined,
      });
    }
  }
  for (const id of draft.add) {
    if (items.some((item) => item.id === id)) continue;
    const collective = byId.get(id);
    if (!collective) continue;
    items.push({
      id,
      name: collective.group.name,
      members: collective.group.member_count,
      note: suggestionNote(collective),
      pending: waitsForApproval(collective) ? 'approval' : mode === 'update' ? 'adding' : undefined,
    });
  }
  return items;
}

/** Add a collective to the draft; re-adding one pending removal keeps it instead. */
export function addToDraft(draft: AccessDraft, id: string): AccessDraft {
  if (draft.remove.includes(id)) return { ...draft, remove: draft.remove.filter((entry) => entry !== id) };
  if (draft.add.includes(id)) return draft;
  return { ...draft, add: [...draft.add, id] };
}

/**
 * Remove a collective from the draft: one this publish adds is simply dropped;
 * one that reads the transcript now is marked for removal.
 */
export function removeFromDraft(draft: AccessDraft, id: string): AccessDraft {
  if (draft.add.includes(id)) return { ...draft, add: draft.add.filter((entry) => entry !== id) };
  if (draft.remove.includes(id)) return draft;
  return { ...draft, remove: [...draft.remove, id] };
}

/** Keep a collective pending removal. */
export function restoreInDraft(draft: AccessDraft, id: string): AccessDraft {
  return { ...draft, remove: draft.remove.filter((entry) => entry !== id) };
}

/** The picker's rows: the collectives not already listed, matching the query. */
export function pickerSuggestions(collectives: readonly LocalVillageCollective[], listed: readonly AccessItem[], query: string): PickerSuggestion[] {
  const needle = query.trim().toLowerCase();
  return collectives
    .filter((collective) => !listed.some((item) => item.id === collective.group.id))
    .filter((collective) => !needle
      || collective.group.name.toLowerCase().includes(needle)
      || (collective.group.linked_github_org ?? '').toLowerCase().includes(needle))
    .map((collective) => ({
      id: collective.group.id,
      name: collective.group.name,
      members: collective.group.member_count,
      note: suggestionNote(collective) ?? (waitsForApproval(collective) ? 'curated · waits for the owner’s approval' : undefined),
    }));
}

/** An update's access change in words, as the popup's delta line. */
export function accessSummary(draft: AccessDraft, names: ReadonlyMap<string, string>): string {
  const adds = draft.add.map((id) => names.get(id)).filter((name): name is string => Boolean(name));
  const removes = draft.remove.map((id) => names.get(id)).filter((name): name is string => Boolean(name));
  if (!adds.length && !removes.length) return 'leave access as it is and update just sends the new turns.';
  return `${adds.length ? `adds ${adds.join(', ')}` : 'adds nothing'} · ${removes.length ? `removes ${removes.join(', ')}` : 'removes nothing'}`;
}

/**
 * The collective names the popup can name by id: the reader's collectives and
 * the transcript's current audience (which may include one the reader has
 * since left).
 */
export function collectiveNames(collectives: readonly LocalVillageCollective[], publication: LocalPublication | null): Map<string, string> {
  const names = new Map<string, string>();
  for (const member of publication?.audience ?? []) names.set(member.collectiveId, member.name);
  for (const collective of collectives) names.set(collective.group.id, collective.group.name);
  return names;
}

/** An update's content change in words. */
export function changesSummary(newTurns: number): string {
  if (newTurns === 0) return 'no new turns since you published';
  return `${newTurns} new ${newTurns === 1 ? 'turn' : 'turns'} since you published`;
}

// ---------------------------------------------------------------------------
// What leaves your machine
// ---------------------------------------------------------------------------

/**
 * A scan's findings as fairtrade review matches. The category is the engine's
 * rendered label (CREDENTIAL, PII, PATH, INTERNAL), validated at the fetch.
 * Nothing is kept un-redacted: the popup's review is read-only.
 */
export function reviewMatches(sessionId: string, redactions: readonly Redaction[]): ReviewMatch[] {
  return redactions.map((redaction) => ({
    id: `${sessionId}::${redaction.id}`,
    category: redaction.category,
    confidence: redaction.confidence / 100,
    before: redaction.originalText,
    after: redaction.redactedReplacement,
    kept: false,
  }));
}

// ---------------------------------------------------------------------------
// Publishing
// ---------------------------------------------------------------------------

/**
 * The typed push for one session. It names only the change the reader asked
 * for, so an update without one keeps the audience the transcript has.
 */
export function pushRequest(sessionId: string, redactionLevel: SelectableRedactionLevel, draft: AccessDraft): SyncPushRequest {
  const request: SyncPushRequest = { sessionIds: [sessionId], redactionLevel };
  if (draft.add.length || draft.remove.length) {
    request.collectives = {
      ...(draft.add.length ? { add: [...draft.add] } : {}),
      ...(draft.remove.length ? { remove: [...draft.remove] } : {}),
    };
  }
  return request;
}

export type PublishOutcome =
  | { kind: 'stopped'; stoppedAt: string }
  | {
    kind: 'done' | 'waits-approval';
    done: {
      url: string;
      collectives: string[];
      pending: string[];
      pullRequest?: { number: number; branch: string; href: string };
    };
  };

function stepSubject(step: SyncPushStepResult, names: ReadonlyMap<string, string>): string {
  const name = step.collectiveId ? names.get(step.collectiveId) ?? 'a collective' : 'a collective';
  switch (step.step) {
    case 'content':
      return 'sending the content';
    case 'remove_collective':
      return `taking it back from ${name}`;
    default:
      return `sharing it with ${name}`;
  }
}

/**
 * What the popup shows after the push answered: where it stopped, or the
 * village link with the collectives that read it and the ones that wait for an
 * owner's approval. Everything comes from the push result's steps.
 */
export function publishOutcome(options: {
  response: SyncPushResponse;
  sessionId: string;
  names: ReadonlyMap<string, string>;
  audience: readonly LocalPublicationAudienceMember[];
  fallbackUrl?: string;
}): PublishOutcome {
  const { response, sessionId, names, audience, fallbackUrl } = options;
  const result = response.sessions.find((session) => session.sessionId === sessionId);
  if (!result) return { kind: 'stopped', stoppedAt: 'reading village’s answer' };
  if (result.status === 'held') return { kind: 'stopped', stoppedAt: 'waiting for this session’s ingest to finish' };
  const steps = result.steps ?? [];
  if (result.status === 'error') {
    const failed = steps.find((step) => step.outcome === 'failed');
    return { kind: 'stopped', stoppedAt: failed ? stepSubject(failed, names) : 'publishing' };
  }
  const url = result.transcriptUrl ?? fallbackUrl;
  if (!url) return { kind: 'stopped', stoppedAt: 'reading the transcript’s link' };

  const removed = new Set(steps.filter((step) => step.step === 'remove_collective' && step.outcome === 'succeeded').map((step) => step.collectiveId));
  const readers = audience.filter((member) => member.status === 'approved' && !removed.has(member.collectiveId)).map((member) => member.name);
  const pending = audience.filter((member) => member.status === 'pending' && !removed.has(member.collectiveId)).map((member) => member.name);
  for (const step of steps) {
    if (step.step !== 'add_collective' || !step.collectiveId) continue;
    const name = names.get(step.collectiveId);
    if (!name) continue;
    if (step.outcome === 'succeeded' && !readers.includes(name)) readers.push(name);
    if (step.outcome === 'pending_approval' && !pending.includes(name)) pending.push(name);
  }
  const request = result.waitingPullRequests?.[0];
  const pullRequest = request
    ? { number: request.number, branch: `${request.owner}/${request.name}`, href: `https://github.com/${request.owner}/${request.name}/pull/${request.number}` }
    : undefined;
  // The heading names the collectives the transcript went to. When every one
  // waits for an owner's approval, it names those, and the line below says so.
  return {
    kind: pending.length ? 'waits-approval' : 'done',
    done: { url, collectives: readers.length ? readers : pending, pending, ...(pullRequest ? { pullRequest } : {}) },
  };
}
