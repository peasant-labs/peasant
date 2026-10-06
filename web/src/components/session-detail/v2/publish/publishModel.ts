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
import { pushRequestBody } from '@/lib/share/push';
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
 * Village holds every share with a curated collective for its owner's
 * approval. This restates its acceptance mode as copy and decides nothing: the
 * push result says what village did.
 */
function waitsForApproval(collective: LocalVillageCollective | undefined): boolean {
  return collective?.group.acceptance_mode === 'curated';
}

/** The collectives that can read the transcript now or wait for approval. */
export function currentReaderIds(publication: LocalPublication | null): string[] {
  if (publication?.state !== 'published') return [];
  return (publication.audience ?? [])
    .filter((member) => member.status === 'approved' || member.status === 'pending')
    .map((member) => member.collectiveId);
}

/**
 * The rows of "who can read it" after this publish. They follow what village
 * holds now, whatever the popup's heading says: once the transcript is
 * published (an update, or a first publish that stopped after village took it)
 * its readers are listed, so removing one takes it back.
 */
export function accessItems(options: {
  publication: LocalPublication | null;
  collectives: readonly LocalVillageCollective[];
  draft: AccessDraft;
}): AccessItem[] {
  const { publication, collectives, draft } = options;
  const published = publication?.state === 'published';
  const byId = new Map(collectives.map((collective) => [collective.group.id, collective]));
  const items: AccessItem[] = [];
  if (published) {
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
      pending: waitsForApproval(collective) ? 'approval' : published ? 'adding' : undefined,
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
 * Remove a collective from the draft. One that can read the transcript now is
 * marked for removal, so the push takes the transcript back from it, even when
 * the draft also asked to add it; one this publish would only add is dropped.
 */
export function removeFromDraft(draft: AccessDraft, id: string, readers: readonly string[]): AccessDraft {
  const add = draft.add.filter((entry) => entry !== id);
  if (!readers.includes(id)) return { ...draft, add };
  return { add, remove: draft.remove.includes(id) ? draft.remove : [...draft.remove, id] };
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
 * The typed push for one session, through the builder the wizard shares. It
 * names only the change the reader asked for, so an update without one keeps
 * the audience. An add the reader cannot see (a collective no longer listed) is
 * not sent, and neither is an add of a collective that already reads it.
 */
export function pushRequest(
  sessionId: string,
  redactionLevel: SelectableRedactionLevel,
  draft: AccessDraft,
  options: { listed: readonly string[]; readers: readonly string[] },
): SyncPushRequest {
  return pushRequestBody([sessionId], redactionLevel, {
    add: draft.add.filter((id) => options.listed.includes(id) && !options.readers.includes(id)),
    remove: draft.remove.filter((id) => options.readers.includes(id)),
  });
}

/** One line of a server message, for the popup's stopped line. */
export function oneLine(text: string, limit = 220): string {
  const line = text.split('\n')[0].trim().replace(/[.;:\s]+$/, '');
  return line.length > limit ? `${line.slice(0, limit - 1)}…` : line;
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

/** The step's subject, with the server's reason when it gave one. */
function stepLine(step: SyncPushStepResult, names: ReadonlyMap<string, string>): string {
  const subject = stepSubject(step, names);
  return step.reason ? `${subject}: ${oneLine(step.reason)}` : subject;
}

/**
 * What the popup shows after the push answered: where it stopped and why, or
 * the village link with the collectives that read it and the ones that wait
 * for an owner's approval.
 *
 * Who can read it comes from the publication read after the push, the same
 * source as the bar, when that read is given; otherwise from the push's steps.
 * A collective the reader asked to add that village did not take (a skipped
 * or unattempted share) stops the popup on that collective with village's
 * reason, so the popup never says it published to a collective that cannot
 * read it.
 */
export function publishOutcome(options: {
  response: SyncPushResponse;
  sessionId: string;
  names: ReadonlyMap<string, string>;
  /** The audience before the push, used when no read after it is given. */
  audienceBefore: readonly LocalPublicationAudienceMember[];
  /** The audience the publication read reports after the push. */
  audienceAfter?: readonly LocalPublicationAudienceMember[];
  /** The collectives the push asked to add. */
  requestedAdds: readonly string[];
  fallbackUrl?: string;
}): PublishOutcome {
  const { response, sessionId, names, audienceBefore, audienceAfter, requestedAdds, fallbackUrl } = options;
  const result = response.sessions.find((session) => session.sessionId === sessionId);
  if (!result) return { kind: 'stopped', stoppedAt: 'reading village’s answer' };
  const steps = result.steps ?? [];
  if (result.status === 'held') {
    const reason = steps.find((step) => step.step === 'content')?.reason;
    return { kind: 'stopped', stoppedAt: reason ? `waiting for this session’s ingest to finish: ${oneLine(reason)}` : 'waiting for this session’s ingest to finish' };
  }
  if (result.status === 'error') {
    const failed = steps.find((step) => step.outcome === 'failed');
    if (failed) return { kind: 'stopped', stoppedAt: stepLine(failed, names) };
    return { kind: 'stopped', stoppedAt: result.error ? `publishing: ${oneLine(result.error)}` : 'publishing' };
  }
  const url = result.transcriptUrl ?? fallbackUrl;
  if (!url) return { kind: 'stopped', stoppedAt: 'reading the transcript’s link' };

  let readers: LocalPublicationAudienceMember[];
  let pending: LocalPublicationAudienceMember[];
  if (audienceAfter) {
    readers = audienceAfter.filter((member) => member.status === 'approved');
    pending = audienceAfter.filter((member) => member.status === 'pending');
  } else {
    const removed = new Set(steps.filter((step) => step.step === 'remove_collective' && step.outcome === 'succeeded').map((step) => step.collectiveId));
    readers = audienceBefore.filter((member) => member.status === 'approved' && !removed.has(member.collectiveId));
    pending = audienceBefore.filter((member) => member.status === 'pending' && !removed.has(member.collectiveId));
    for (const step of steps) {
      if (step.step !== 'add_collective' || !step.collectiveId) continue;
      const member = { collectiveId: step.collectiveId, name: names.get(step.collectiveId) ?? 'a collective' };
      if (step.outcome === 'succeeded' && !readers.some((entry) => entry.collectiveId === member.collectiveId)) readers.push({ ...member, status: 'approved' });
      if (step.outcome === 'pending_approval' && !pending.some((entry) => entry.collectiveId === member.collectiveId)) pending.push({ ...member, status: 'pending' });
    }
  }
  const holds = new Set([...readers, ...pending].map((member) => member.collectiveId));
  const missed = requestedAdds.find((id) => !holds.has(id));
  if (missed) {
    const step = steps.find((entry) => entry.step === 'add_collective' && entry.collectiveId === missed);
    const name = names.get(missed) ?? 'a collective';
    return { kind: 'stopped', stoppedAt: step ? stepLine(step, names) : `sharing it with ${name}` };
  }

  // A pull request is named only when exactly one waits for this repository:
  // with several, "comment /peasant attach on it" could point at the wrong one.
  const requests = result.waitingPullRequests ?? [];
  const request = requests.length === 1 ? requests[0] : undefined;
  // fairtrade names the place a pull request is open as its `branch`; the push
  // reports the repository, not the branch, so the repository is named.
  const pullRequest = request
    ? { number: request.number, branch: `${request.owner}/${request.name}`, href: `https://github.com/${request.owner}/${request.name}/pull/${request.number}` }
    : undefined;
  const readerNames = readers.map((member) => member.name);
  const pendingNames = pending.map((member) => member.name);
  // Approved readers and pending shares stay distinct: fairtrade names a
  // pending-only result as submitted, without claiming those members can read it.
  return {
    kind: pendingNames.length ? 'waits-approval' : 'done',
    done: { url, collectives: readerNames, pending: pendingNames, ...(pullRequest ? { pullRequest } : {}) },
  };
}
