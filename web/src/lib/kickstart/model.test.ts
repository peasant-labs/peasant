import { describe, expect, it } from 'vitest';
import { AllLocalKickstartErrorCodes } from '@peasant-labs/schema';
import {
  acceptKickstartEvent,
  KickstartContractError,
  parseKickstartErrorBody,
  parseKickstartEvent,
  parseKickstartTree,
} from './model';
import { ALL_KICKSTART_EVENT_KINDS } from './values';
import { expandRefs, loadCases, loadModels, loadTrees } from '@/test/fixtures/kickstart';

/**
 * The boundary of the kickstart model, played from fixtures: which topic
 * messages become events, which the contract refuses and why, and that a forest
 * with a row the contract does not list is refused whole rather than relabelled.
 */

const PATH = 'src/lib/kickstart/testdata/model.yaml';
const models = loadModels();
const trees = loadTrees();

const named = acceptKickstartEvent;
const reached = (frame: unknown): string =>
  named(parseKickstartEvent(frame), {
    snapshot: () => 'snapshot',
    scan: () => 'scan',
    ingest: () => 'ingest',
    closed: () => 'closed',
  });

describe('kickstart topic messages', () => {
  for (const entry of loadCases(PATH)) {
    it(entry.name, () => {
      const frame = expandRefs(entry.frame, models, trees);
      if (typeof entry.accepts === 'string') {
        expect(reached(frame)).toBe(entry.accepts);
        return;
      }
      let failure: unknown;
      try {
        parseKickstartEvent(frame);
      } catch (error) {
        failure = error;
      }
      expect(failure, 'the contract must refuse this frame').toBeInstanceOf(KickstartContractError);
      expect((failure as Error).message).toContain(entry.refuses as string);
      // The error says where it failed and what the person can do.
      expect((failure as Error).message).toContain('a kickstart topic message');
      expect((failure as Error).message).toMatch(/reload/);
    });
  }

  it('reaches every event kind the contract lists', () => {
    const reachedKinds = new Set(
      loadCases(PATH)
        .filter((entry) => typeof entry.accepts === 'string')
        .map((entry) => (expandRefs(entry.frame, models, trees) as { kind: string }).kind),
    );
    for (const kind of ALL_KICKSTART_EVENT_KINDS) {
      expect(reachedKinds.has(kind.wire), `no accepted fixture frame has the ${kind.wire} kind`).toBe(true);
    }
  });
});

describe('the forest', () => {
  for (const entry of loadCases(PATH, 'trees')) {
    it(entry.name, () => {
      const body = typeof entry.tree === 'string' ? trees[entry.tree] : entry.body;
      if (!Array.isArray(entry.refuses)) {
        // The rows are the contract's own: nothing is added, dropped or renamed.
        expect(parseKickstartTree(body)).toEqual(body);
        return;
      }
      let failure: unknown;
      try {
        parseKickstartTree(body);
      } catch (error) {
        failure = error;
      }
      expect(failure, 'the contract must refuse this forest').toBeInstanceOf(KickstartContractError);
      for (const text of entry.refuses as string[]) expect((failure as Error).message).toContain(text);
      expect((failure as Error).message).toContain('GET /api/v1/kickstart/sessions/{id}/tree');
    });
  }
});

describe('the refusal body', () => {
  const where = 'POST /api/v1/kickstart/sessions/ses-full/commit';
  for (const entry of loadCases(PATH, 'errors')) {
    it(entry.name, () => {
      if (entry.accepts === 'every-code') {
        expect(AllLocalKickstartErrorCodes.length, 'the contract lists refusal codes').toBeGreaterThan(0);
        for (const code of AllLocalKickstartErrorCodes) {
          expect(parseKickstartErrorBody({ code, error: `the sentence for ${code}` }, where), `the code ${code}`).toEqual({ code, error: `the sentence for ${code}` });
        }
        return;
      }
      let failure: unknown;
      try {
        parseKickstartErrorBody(entry.body, where);
      } catch (error) {
        failure = error;
      }
      expect(failure, 'the contract must refuse this body').toBeInstanceOf(KickstartContractError);
      expect((failure as Error).message).toContain(where);
      for (const text of entry.refuses as string[]) expect((failure as Error).message).toContain(text);
    });
  }
});
