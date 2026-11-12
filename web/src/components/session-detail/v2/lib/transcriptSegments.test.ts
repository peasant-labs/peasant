import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type {
  TranscriptSegmentDirectory as SchemaDirectory,
  TranscriptSegmentRef as SchemaRef,
} from '@peasant-labs/schema';
import { parseStrictYAML, requireExactRequiredFields, requireRecord, requireUniqueNames } from '@/test/strictYaml';
import {
  parseTranscriptDirectory,
  type TranscriptSegmentDirectory,
  type TranscriptSegmentRef,
} from './transcriptSegments';

// The ref is the schema's ref, both ways: a field added to either side without
// the other fails to compile. The directory is checked one way: what this page
// holds is a legal schema directory, and the paging fields the schema also
// declares are not read here.
const _refCoversSchema: SchemaRef extends TranscriptSegmentRef ? true : never = true;
const _refIsLegal: TranscriptSegmentRef extends SchemaRef ? true : never = true;
const _directoryIsLegal: TranscriptSegmentDirectory extends SchemaDirectory ? true : never = true;
void _refCoversSchema; void _refIsLegal; void _directoryIsLegal;

const DIR = 'src/components/session-detail/v2/lib/testdata';
const fixture = requireRecord(
  parseStrictYAML(readFileSync(resolve(process.cwd(), DIR, 'transcript_directory_positions.yaml'), 'utf8'), 'transcript_directory_positions.yaml'),
  'transcript_directory_positions.yaml',
);
const manifest = requireRecord(
  parseStrictYAML(readFileSync(resolve(process.cwd(), DIR, 'transcript_directory_positions.manifest.yaml'), 'utf8'), 'transcript_directory_positions.manifest.yaml'),
  'transcript_directory_positions.manifest.yaml',
);

const ABSENT = 'absent';
type Positions = { total: unknown; spans: unknown[] };
type PositionCase = { name: string; positions: Positions; want: Positions; warn: string[] };

requireExactRequiredFields(fixture, ['path', 'base', 'cases'], 'transcript_directory_positions.yaml');
requireExactRequiredFields(manifest, ['requiredNames'], 'transcript_directory_positions.manifest.yaml');
const path = fixture.path as string;
const base = requireRecord(fixture.base, 'base');
const baseSegments = base.segments as Record<string, unknown>[];
const cases = (fixture.cases as unknown[]).map((row, index) => {
  const record = requireRecord(row, `cases[${index}]`);
  requireExactRequiredFields(record, ['name', 'positions', 'want', 'warn'], `cases[${index}]`);
  for (const key of ['positions', 'want'] as const) {
    const positions = requireRecord(record[key], `cases[${index}].${key}`);
    requireExactRequiredFields(positions, ['total', 'spans'], `cases[${index}].${key}`);
    if (!Array.isArray(positions.spans) || positions.spans.length !== baseSegments.length) {
      throw new Error(`cases[${index}].${key}.spans must state one entry per base segment (${baseSegments.length})`);
    }
  }
  return record as unknown as PositionCase;
});
requireUniqueNames(cases as unknown as Record<string, unknown>[], 'cases');

function wire(positions: Positions): unknown {
  return {
    ...base,
    ...(positions.total === ABSENT ? {} : { totalConversationTurns: positions.total }),
    segments: baseSegments.map((segment, index) => ({
      ...segment,
      ...(positions.spans[index] === ABSENT ? {} : { conversationTurns: positions.spans[index] }),
    })),
  };
}

function expected(positions: Positions): unknown {
  // The wanted directory is exactly the wire shape of the wanted positions: a
  // dropped field is a missing key, never an undefined or null value.
  return wire(positions);
}

describe('parseTranscriptDirectory conversation turn positions', () => {
  afterEach(() => vi.restoreAllMocks());

  it('holds every required fixture row', () => {
    const names = cases.map((row) => row.name);
    const required = manifest.requiredNames as string[];
    expect(required.filter((name) => !names.includes(name))).toEqual([]);
  });

  it.each(cases.map((row) => [row.name, row] as const))('%s', (_name, row) => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const parsed = parseTranscriptDirectory(wire(row.positions), path);
    expect(parsed).toStrictEqual(expected(row.want));
    expect('totalConversationTurns' in parsed).toBe(row.want.total !== ABSENT);
    parsed.segments.forEach((ref, index) => {
      expect('conversationTurns' in ref, `segments[${index}] conversationTurns key`).toBe(row.want.spans[index] !== ABSENT);
    });
    if (row.warn.length === 0) {
      expect(warn).not.toHaveBeenCalled();
      return;
    }
    expect(warn).toHaveBeenCalledTimes(1);
    const call = warn.mock.calls[0]!;
    expect(call).toHaveLength(1);
    const text = String(call[0]);
    for (const part of row.warn) expect(text).toContain(part);
  });
});
