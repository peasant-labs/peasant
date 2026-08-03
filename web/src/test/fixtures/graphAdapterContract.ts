import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { RewrittenCommit, SessionInsight } from '@peasant-labs/schema';
import type { DecodedMapNode } from '@/lib/api/map';
import { parseStrictYAML, requireExactRequiredFields, requireRecord } from '@/test/strictYaml';

type MapComprehensionFields = Pick<
  DecodedMapNode,
  'agentEditedCount' | 'readCount' | 'readAttribution' | 'readState' | 'changedRegionCount' | 'attributedRegionCount' | 'reviewedRegionCount'
>;

export type GraphAdapterContractFixture = {
  mapNode: MapComprehensionFields;
  rewrittenCommits: RewrittenCommit[];
  insights: SessionInsight[];
};

const source = readFileSync(
  resolve(process.cwd(), 'src/test/testdata/graph_adapter_contract.yaml'),
  'utf8',
);

function requireInteger(value: unknown, path: string): number {
  if (typeof value !== 'number' || !Number.isInteger(value)) throw new Error(`${path} must be an integer`);
  return value;
}

function requireNonEmptyArray(value: unknown, path: string): Record<string, unknown>[] {
  if (!Array.isArray(value) || value.length === 0) throw new Error(`${path} must be a non-empty array`);
  return value.map((entry, index) => requireRecord(entry, `${path}[${index}]`));
}

function loadGraphAdapterContractFixture(): GraphAdapterContractFixture {
  const root = requireRecord(parseStrictYAML(source, 'graph adapter contract fixture'), 'graph adapter contract fixture');
  requireExactRequiredFields(root, ['mapNode', 'rewrittenCommits', 'insights'], 'graph adapter contract fixture');

  const mapNode = requireRecord(root.mapNode, 'graph adapter contract fixture.mapNode');
  requireExactRequiredFields(
    mapNode,
    ['agentEditedCount', 'readCount', 'readAttribution', 'readState', 'changedRegionCount', 'attributedRegionCount', 'reviewedRegionCount'],
    'graph adapter contract fixture.mapNode',
  );
  const readAttribution = mapNode.readAttribution;
  if (readAttribution !== 'complete' && readAttribution !== 'partial' && readAttribution !== 'unavailable') {
    throw new Error('graph adapter contract fixture.mapNode.readAttribution must be a canonical read attribution state');
  }
  const readState = mapNode.readState;
  if (readState !== 'none' && readState !== 'viewed' && readState !== 'reviewed' && readState !== 'reviewed_in_detail') {
    throw new Error('graph adapter contract fixture.mapNode.readState must be a canonical read state');
  }

  const rewrittenCommits = requireNonEmptyArray(root.rewrittenCommits, 'graph adapter contract fixture.rewrittenCommits');
  rewrittenCommits.forEach((commit, index) => {
    requireExactRequiredFields(
      commit,
      ['ghostHash', 'subject', 'authorTimeMs', 'sessionIds', 'associations', 'successorHash', 'resolution', 'method', 'confidence'],
      `graph adapter contract fixture.rewrittenCommits[${index}]`,
    );
    if (typeof commit.ghostHash !== 'string' || typeof commit.subject !== 'string' || typeof commit.successorHash !== 'string' || typeof commit.resolution !== 'string' || typeof commit.method !== 'string' || typeof commit.confidence !== 'string') {
      throw new Error(`graph adapter contract fixture.rewrittenCommits[${index}] must contain string rewrite evidence`);
    }
    requireInteger(commit.authorTimeMs, `graph adapter contract fixture.rewrittenCommits[${index}].authorTimeMs`);
    if (!Array.isArray(commit.sessionIds) || commit.sessionIds.length === 0 || commit.sessionIds.some((id) => typeof id !== 'string' || id.length === 0)) {
      throw new Error(`graph adapter contract fixture.rewrittenCommits[${index}].sessionIds must be non-empty strings`);
    }
    if (!Array.isArray(commit.associations) || commit.associations.length === 0) {
      throw new Error(`graph adapter contract fixture.rewrittenCommits[${index}].associations must be non-empty`);
    }
  });

  const insights = requireNonEmptyArray(root.insights, 'graph adapter contract fixture.insights');
  insights.forEach((insight, index) => {
    requireExactRequiredFields(
      insight,
      ['kind', 'provenance', 'confidence', 'title', 'summary', 'subjects', 'evidence'],
      `graph adapter contract fixture.insights[${index}]`,
    );
    if (typeof insight.kind !== 'string' || typeof insight.provenance !== 'string' || typeof insight.confidence !== 'string' || typeof insight.title !== 'string' || typeof insight.summary !== 'string') {
      throw new Error(`graph adapter contract fixture.insights[${index}] must contain a typed insight envelope`);
    }
    if (!Array.isArray(insight.subjects) || insight.subjects.length === 0 || !Array.isArray(insight.evidence) || insight.evidence.length === 0) {
      throw new Error(`graph adapter contract fixture.insights[${index}] must retain non-empty subjects and evidence`);
    }
  });

  return {
    mapNode: {
      agentEditedCount: requireInteger(mapNode.agentEditedCount, 'graph adapter contract fixture.mapNode.agentEditedCount'),
      readCount: requireInteger(mapNode.readCount, 'graph adapter contract fixture.mapNode.readCount'),
      readAttribution,
      readState,
      changedRegionCount: requireInteger(mapNode.changedRegionCount, 'graph adapter contract fixture.mapNode.changedRegionCount'),
      attributedRegionCount: requireInteger(mapNode.attributedRegionCount, 'graph adapter contract fixture.mapNode.attributedRegionCount'),
      reviewedRegionCount: requireInteger(mapNode.reviewedRegionCount, 'graph adapter contract fixture.mapNode.reviewedRegionCount'),
    },
    rewrittenCommits: rewrittenCommits as RewrittenCommit[],
    insights: insights as SessionInsight[],
  };
}

export const graphAdapterContractFixture = loadGraphAdapterContractFixture();
