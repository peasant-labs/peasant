import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parseStrictYAML, requireExactFields, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import type { LocalPublicationAudienceMember, SyncPushSessionResult } from '@peasant-labs/schema';
import { addToDraft, removeFromDraft, restoreInDraft, publishOutcome, pushRequest, type AccessDraft } from './publishModel';

interface Case {
  name: string;
  operation?: 'add' | 'remove' | 'restore' | 'request';
  draft?: AccessDraft;
  readers?: string[];
  listed?: string[];
  expectedRequest?: unknown;
  id?: string;
  expectedDraft?: AccessDraft;
  result?: Omit<SyncPushSessionResult, 'sessionId'>;
  audience?: LocalPublicationAudienceMember[];
  requestedAdds?: string[];
  waiting?: number[];
  expected?: { kind: 'stopped' | 'done'; text?: string; collectives?: string[]; pullRequest?: number };
}
const fixture = requireRecord(parseStrictYAML(readFileSync(resolve(process.cwd(), 'src/components/session-detail/v2/publish/testdata/publish-model.yaml'), 'utf8'), 'publish model fixture'), 'publish model fixture') as unknown as { requiredNames: string[]; cases: Case[] };
requireExactRequiredFields(fixture as unknown as Record<string, unknown>, ['requiredNames', 'cases'], 'publish model fixture');
requireUniqueNames(fixture.cases as unknown as Record<string, unknown>[], 'publish model fixture.cases');
for (const entry of fixture.cases) requireExactFields(entry as unknown as Record<string, unknown>, ['name', 'operation', 'draft', 'readers', 'listed', 'expectedRequest', 'id', 'expectedDraft', 'result', 'audience', 'requestedAdds', 'waiting', 'expected'], `publish model fixture.${entry.name}`);
const names = new Map([['a', 'Platform'], ['b', 'Company']]);

describe('publication draft and outcomes', () => {
  it('keeps every required scenario', () => {
    expect(new Set(fixture.cases.map((entry) => entry.name))).toEqual(new Set(fixture.requiredNames));
    expect(fixture.cases.map((entry) => entry.name)).toHaveLength(new Set(fixture.cases.map((entry) => entry.name)).size);
  });
  it.each(fixture.cases)('$name', (entry) => {
    if (entry.operation === 'request' && entry.draft) {
      expect(pushRequest('session', 'standard', entry.draft, { listed: entry.listed ?? [], readers: entry.readers ?? [] })).toEqual(entry.expectedRequest);
      return;
    }
    if (entry.operation && entry.draft && entry.id) {
      const changed = entry.operation === 'add' ? addToDraft(entry.draft, entry.id)
        : entry.operation === 'remove' ? removeFromDraft(entry.draft, entry.id, entry.readers ?? [])
          : restoreInDraft(entry.draft, entry.id);
      expect(changed).toEqual(entry.expectedDraft);
      return;
    }
    const result = entry.result ? { ...entry.result, sessionId: 'session', ...(entry.waiting ? { waitingPullRequests: entry.waiting.map((number) => ({ owner: 'acme', name: 'api', number, remote: 'github.com:acme/api', head_remote: 'github.com:acme/api', requested_at: '2026-09-29T10:00:00Z', state: 'waiting' as const })) } : {}) } : undefined;
    const outcome = publishOutcome({ response: { new: 0, updated: 0, skipped: 0, errors: 0, sessions: result ? [result] : [] }, sessionId: 'session', names, audienceBefore: entry.audience ?? [], requestedAdds: entry.requestedAdds ?? [] });
    expect(outcome.kind).toBe(entry.expected?.kind);
    if (outcome.kind === 'stopped') expect(outcome.stoppedAt).toContain(entry.expected?.text);
    else {
      expect(outcome.done.collectives).toEqual(entry.expected?.collectives);
      expect(outcome.done.pullRequest?.number).toBe(entry.expected?.pullRequest);
    }
  });
});
