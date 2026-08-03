/**
 * Synthetic annotation values matching the public schema contract.
 */
import type { AnnotationSummary } from '@/types/messages';

// Core annotation templates — human/agent/rule × resolved/not_resolved

export const HUMAN_OUTCOME_RESOLVED: AnnotationSummary = {
  id: 'ann-fixture-0001',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'human',
  annotatorName: 'reviewer-1',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'resolved',
  createdAt: BigInt(1700000000000),
};

export const AGENT_OUTCOME_RESOLVED: AnnotationSummary = {
  id: 'ann-fixture-0003',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'agent',
  annotatorName: 'auto-evaluator',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'resolved',
  createdAt: BigInt(1700000002000),
};

export const AGENT_OUTCOME_NOT_RESOLVED: AnnotationSummary = {
  id: 'ann-fixture-0004',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'agent',
  annotatorName: 'auto-evaluator',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'not_resolved',
  createdAt: BigInt(1700000003000),
};

export const RULE_OUTCOME_RESOLVED: AnnotationSummary = {
  id: 'ann-fixture-0005',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'rule',
  annotatorName: 'heuristic-classifier',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'resolved',
  createdAt: BigInt(1700000004000),
};

export const RULE_OUTCOME_NOT_RESOLVED: AnnotationSummary = {
  id: 'ann-fixture-0006',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'rule',
  annotatorName: 'heuristic-classifier',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'not_resolved',
  createdAt: BigInt(1700000005000),
};

// Edge cases

export const WRONG_TYPE_ID: AnnotationSummary = {
  id: 'ann-fixture-0007',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'human',
  annotatorName: 'reviewer-1',
  typeId: 'quality.other_metric',
  typeName: 'Other Metric',
  value: 'high',
  createdAt: BigInt(1700000006000),
};

export const UNKNOWN_VALUE: AnnotationSummary = {
  id: 'ann-fixture-0008',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: true,
  annotatorKind: 'human',
  annotatorName: 'reviewer-1',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'unknown_value',
  createdAt: BigInt(1700000007000),
};

export const SECOND_HUMAN_ANNOTATION: AnnotationSummary = {
  id: 'ann-fixture-0009',
  targetKind: 'session',
  targetSessionId: 'quality-ann-1',
  isPrimary: false,
  annotatorKind: 'human',
  annotatorName: 'reviewer-2',
  typeId: 'quality.session_outcome',
  typeName: 'Session Outcome',
  value: 'not_resolved',
  createdAt: BigInt(1700000008000),
};
