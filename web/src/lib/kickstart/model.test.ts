import { describe, expect, it } from 'vitest';
import {
  acceptKickstartEvent,
  KickstartContractError,
  parseKickstartEvent,
  parseKickstartTree,
} from './model';
import { ALL_KICKSTART_EVENT_KINDS } from './values';
import { expandRefs, loadCases, loadModels, loadTrees } from '@/test/fixtures/kickstart';

/**
 * The boundary of the kickstart model, played from fixtures: which topic
 * messages become events, which the contract refuses and why, and how a forest
 * with rows the page cannot name is counted rather than relabelled.
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
      if (typeof entry.refuses === 'string') {
        expect(() => parseKickstartTree(body)).toThrow(entry.refuses);
        return;
      }
      const tree = parseKickstartTree(body);
      const expected = entry.expect as { kept: string[]; unknownKind: number; unknownState: number; unreadable: number };
      expect(tree.nodes.map((node) => node.id)).toEqual(expected.kept);
      expect(tree.dropped).toEqual({ unknownKind: expected.unknownKind, unknownState: expected.unknownState, unreadable: expected.unreadable });
    });
  }
});
