import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import {
  useEntryLabels,
  groupEntryAnnotations,
  summaryToTurnLabel,
} from './useEntryLabels';
import type { AnnotationSummary } from '@/lib/api/annotations';

function entryAnn(over: Partial<AnnotationSummary>): AnnotationSummary {
  return {
    id: 'a1',
    targetKind: 'entry',
    targetSessionId: 'sess-1',
    targetEntryIndex: 3,
    isPrimary: false,
    annotatorKind: 'human',
    annotatorName: 'human-web',
    typeId: 'quality.frustration_signal',
    typeName: 'Frustration Signal',
    value: 'detected',
    createdAt: BigInt(0),
    ...over,
  };
}

describe('summaryToTurnLabel', () => {
  it('maps an entry annotation to a SavedTurnLabel', () => {
    expect(summaryToTurnLabel(entryAnn({ id: 'x', targetEntryIndex: 5 }))).toEqual({
      entryIndex: 5,
      typeId: 'quality.frustration_signal',
      typeName: 'Frustration Signal',
      value: 'detected',
      id: 'x',
    });
  });
  it('drops session-level annotations', () => {
    expect(summaryToTurnLabel(entryAnn({ targetKind: 'session', targetEntryIndex: undefined }))).toBeNull();
  });
  it('drops entry annotations missing targetEntryIndex', () => {
    expect(summaryToTurnLabel(entryAnn({ targetEntryIndex: undefined }))).toBeNull();
  });
});

describe('groupEntryAnnotations', () => {
  it('groups by entry index and drops non-entry rows', () => {
    const map = groupEntryAnnotations([
      entryAnn({ id: '1', targetEntryIndex: 2 }),
      entryAnn({ id: '2', targetEntryIndex: 2, value: 'not_detected' }),
      entryAnn({ id: '3', targetEntryIndex: 9 }),
      entryAnn({ id: '4', targetKind: 'session', targetEntryIndex: undefined }),
    ]);
    expect(map.get(2)).toHaveLength(2);
    expect(map.get(9)).toHaveLength(1);
    expect([...map.keys()].sort()).toEqual([2, 9]);
  });
});

describe('useEntryLabels', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  function mockEndpoints(opts: {
    types?: unknown;
    annotations?: unknown;
  }) {
    fetchMock.mockImplementation((url: string) => {
      if (url.includes('/annotation-types')) {
        return Promise.resolve({ ok: true, json: async () => opts.types ?? [] });
      }
      if (url.includes('/annotations?session_id')) {
        return Promise.resolve({ ok: true, json: async () => opts.annotations ?? [] });
      }
      return Promise.reject(new Error(`unexpected url ${url}`));
    });
  }

  it('exposes only entry-applicable types and grouped backend labels', async () => {
    mockEndpoints({
      types: [
        { typeId: 'quality.frustration_signal', version: 1, displayName: 'Frustration', family: 'f', class: 'c', valueDomain: { kind: 'enumerated', datatype: 'text', permissibleValues: ['detected'] }, status: 'active', origin: 'system', allowedTargetKinds: ['entry'] },
        { typeId: 'quality.session_outcome', version: 1, displayName: 'Outcome', family: 'f', class: 'c', valueDomain: { kind: 'enumerated', datatype: 'text', permissibleValues: ['resolved'] }, status: 'active', origin: 'system', allowedTargetKinds: ['session'] },
      ],
      annotations: [entryAnn({ id: 'a', targetEntryIndex: 4 })],
    });

    const { result } = renderHook(() => useEntryLabels('sess-1'));

    await waitFor(() => expect(result.current.entryTypes).toHaveLength(1));
    expect(result.current.entryTypes[0].typeId).toBe('quality.frustration_signal');
    await waitFor(() => expect(result.current.labelsByEntry.get(4)).toBeTruthy());
    expect(result.current.labelsByEntry.get(4)).toHaveLength(1);
  });

  it('merges an optimistically-added label into the map', async () => {
    mockEndpoints({ types: [], annotations: [] });
    const { result } = renderHook(() => useEntryLabels('sess-1'));

    await waitFor(() => expect(result.current.labelsByEntry.size).toBe(0));

    act(() => {
      result.current.addLabel({
        entryIndex: 11,
        typeId: 'quality.resolution_evidence',
        typeName: 'Resolution Evidence',
        value: 'present',
        id: 'opt-1',
      });
    });

    await waitFor(() => expect(result.current.labelsByEntry.get(11)).toHaveLength(1));
    expect(result.current.labelsByEntry.get(11)?.[0].value).toBe('present');
  });

  it('does not duplicate an optimistic label that already arrived from the server', async () => {
    mockEndpoints({ types: [], annotations: [entryAnn({ id: 'dup', targetEntryIndex: 2 })] });
    const { result } = renderHook(() => useEntryLabels('sess-1'));

    await waitFor(() => expect(result.current.labelsByEntry.get(2)).toHaveLength(1));

    act(() => {
      result.current.addLabel({
        entryIndex: 2,
        typeId: 'quality.frustration_signal',
        typeName: 'Frustration Signal',
        value: 'detected',
        id: 'dup', // same DB id as the server row
      });
    });

    // Still one — the optimistic add was deduped by id.
    await waitFor(() => expect(result.current.labelsByEntry.get(2)).toHaveLength(1));
  });

  it('degrades gracefully when the types endpoint fails', async () => {
    fetchMock.mockImplementation((url: string) => {
      if (url.includes('/annotation-types')) return Promise.reject(new Error('down'));
      return Promise.resolve({ ok: true, json: async () => [] });
    });
    const { result } = renderHook(() => useEntryLabels('sess-1'));
    await waitFor(() => expect(result.current.entryTypes).toEqual([]));
  });
});
