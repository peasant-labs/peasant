import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { classifyScanFailure, reindexFailureLine, reindexRetryable, type ReindexFailureCode, type ScanFailureKind } from './scanFailure';

interface Case { name: string; detail: string; kind: ScanFailureKind; line: string; message: string }
const fixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/scan-failures.yaml'), 'utf8')) as { requiredNames: string[]; cases: Case[] };

describe('classifyScanFailure', () => {
  it('holds exactly the required cases', () => {
    expect(fixture.cases.map((row) => row.name).sort()).toEqual([...fixture.requiredNames].sort());
  });
  for (const row of fixture.cases) {
    it(row.name, () => {
      const failure = classifyScanFailure(row.detail);
      expect(failure.kind).toBe(row.kind);
      expect(failure.line).toBe(row.line);
      expect(failure.message).toBe(row.message);
      // The transport wrapper never reaches the line.
      expect(failure.line).not.toMatch(/redaction scan failed \(\d+\)|\{"error"/);
      expect(failure.message).not.toMatch(/redaction scan failed \(\d+\)|\{"error"/);
      expect(failure.detail).toBe(row.detail);
    });
  }
});

interface ReindexCase { name: string; code: ReindexFailureCode; line: string; retryable: boolean }
const reindexFixture = parse(readFileSync(resolve(process.cwd(), 'src/lib/share/testdata/reindex-failures.yaml'), 'utf8')) as { requiredNames: string[]; cases: ReindexCase[] };

describe('reindexFailureLine', () => {
  it('holds exactly the required cases', () => {
    expect(reindexFixture.cases.map((row) => row.name).sort()).toEqual([...reindexFixture.requiredNames].sort());
  });
  for (const row of reindexFixture.cases) {
    it(row.name, () => {
      expect(reindexFailureLine(row.code)).toBe(row.line);
      expect(reindexRetryable(row.code)).toBe(row.retryable);
      expect(row.line).not.toMatch(/[{}]|https?:\/\/|village|peasant online/i);
    });
  }
});
