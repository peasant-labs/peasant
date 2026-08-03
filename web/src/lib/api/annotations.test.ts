import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import {
  entryApplicableTypes,
  permissibleValues,
  fetchAnnotationTypes,
  fetchSessionAnnotations,
  batchCreateAnnotations,
  saveTurnLabel,
  saveTurnLabels,
  type AnnotationType,
} from './annotations';

function makeType(over: Partial<AnnotationType>): AnnotationType {
  return {
    typeId: 'quality.example',
    version: 1,
    displayName: 'Example',
    family: 'turn_quality',
    class: 'quality',
    valueDomain: { kind: 'enumerated', datatype: 'text', permissibleValues: ['a', 'b'] },
    status: 'active',
    origin: 'system',
    ...over,
  };
}

describe('entryApplicableTypes', () => {
  it('keeps only types whose allowedTargetKinds includes "entry"', () => {
    const types = [
      makeType({ typeId: 'quality.frustration_signal', allowedTargetKinds: ['session', 'entry'] }),
      makeType({ typeId: 'quality.resolution_evidence', allowedTargetKinds: ['entry'] }),
      makeType({ typeId: 'quality.session_outcome', allowedTargetKinds: ['session', 'project'] }),
    ];
    const result = entryApplicableTypes(types);
    expect(result.map((t) => t.typeId)).toEqual([
      'quality.frustration_signal',
      'quality.resolution_evidence',
    ]);
  });

  it('excludes types with no allowedTargetKinds (no explicit entry kind)', () => {
    // The critical correctness fix: session-only / unscoped types must NOT be
    // offered for a turn, even though the old UI flattened them all.
    const types = [
      makeType({ typeId: 'quality.unscoped', allowedTargetKinds: undefined }),
      makeType({ typeId: 'quality.empty', allowedTargetKinds: [] }),
      makeType({ typeId: 'quality.session_only', allowedTargetKinds: ['session'] }),
    ];
    expect(entryApplicableTypes(types)).toEqual([]);
  });

  it('returns empty for an empty input', () => {
    expect(entryApplicableTypes([])).toEqual([]);
  });
});

describe('permissibleValues', () => {
  it('returns enumerated values', () => {
    expect(permissibleValues(makeType({ valueDomain: { kind: 'enumerated', datatype: 'text', permissibleValues: ['x', 'y'] } }))).toEqual(['x', 'y']);
  });
  it('returns [] for described (free-text) domains', () => {
    expect(permissibleValues(makeType({ valueDomain: { kind: 'described', datatype: 'text', constraintSpec: '{}' } }))).toEqual([]);
  });
  it('returns [] when enumerated but no values present', () => {
    expect(permissibleValues(makeType({ valueDomain: { kind: 'enumerated', datatype: 'text' } }))).toEqual([]);
  });
});

describe('annotations API client', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetAllMocks();
  });

  it('fetchAnnotationTypes hits GET /api/v1/annotation-types and returns rows', async () => {
    const rows = [makeType({ typeId: 'quality.frustration_signal', allowedTargetKinds: ['entry'] })];
    fetchMock.mockResolvedValue({ ok: true, json: async () => rows });

    const result = await fetchAnnotationTypes();

    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('/api/v1/annotation-types'));
    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('status=active'));
    expect(result).toEqual(rows);
  });

  it('fetchAnnotationTypes appends status filter when provided', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => [] });
    await fetchAnnotationTypes({ status: 'active' });
    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('status=active'));
  });

  it('fetchAnnotationTypes throws on non-ok response', async () => {
    fetchMock.mockResolvedValue({ ok: false, status: 500, text: async () => 'boom' });
    await expect(fetchAnnotationTypes()).rejects.toThrow(/annotation-types failed \(500\)/);
  });

  it('fetchSessionAnnotations normalizes null payload to []', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => null });
    const result = await fetchSessionAnnotations('sess-1');
    expect(result).toEqual([]);
    expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining('session_id=sess-1'));
  });

  it('batchCreateAnnotations POSTs the annotations array and returns ids', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ ids: ['id-1'] }) });

    const res = await batchCreateAnnotations([
      { sessionId: 'sess-1', typeId: 'quality.resolution_evidence', value: 'present', isPrimary: false },
    ]);

    expect(res.ids).toEqual(['id-1']);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain('/api/v1/annotations/batch');
    expect(init.method).toBe('POST');
    const body = JSON.parse(init.body);
    expect(body.annotations).toHaveLength(1);
    expect(body.annotations[0].typeId).toBe('quality.resolution_evidence');
  });

  it('batchCreateAnnotations surfaces the failing index from a 400 body', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 400,
      text: async () => '{"error":"annotation[0]: invalid value","failingIndex":0}',
    });
    await expect(
      batchCreateAnnotations([{ sessionId: 's', typeId: 't', value: 'bad', isPrimary: false }]),
    ).rejects.toThrow(/batch failed \(400\)/);
  });

  it('saveTurnLabel sends a single entry-targeted item with half-open span', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ ids: ['new-id'] }) });

    const id = await saveTurnLabel({
      sessionId: 'sess-1',
      entryIndex: 7,
      typeId: 'quality.frustration_signal',
      value: 'detected',
    });

    expect(id).toBe('new-id');
    const body = JSON.parse(fetchMock.mock.calls[0][1].body);
    const item = body.annotations[0];
    expect(item.targetEntryIndex).toBe(7);
    expect(item.targetEntryEndIndex).toBe(8); // index + 1 (half-open)
    expect(item.annotatorName).toBe('human-web');
    expect(item.value).toBe('detected');
  });

  it('saveTurnLabels sends every item entry-targeted in ONE atomic batch, in order', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ ids: ['id-outcome', 'id-flag'] }) });

    const ids = await saveTurnLabels({
      sessionId: 'sess-1',
      entryIndex: 7,
      items: [
        { typeId: 'quality.turn_outcome', value: 'bad' },
        { typeId: 'quality.turn_flag', value: 'retry_loop' },
      ],
    });

    expect(ids).toEqual(['id-outcome', 'id-flag']);
    expect(fetchMock).toHaveBeenCalledTimes(1); // one atomic batch, not two POSTs
    const body = JSON.parse(fetchMock.mock.calls[0][1].body);
    expect(body.annotations).toHaveLength(2);
    for (const item of body.annotations) {
      expect(item.targetEntryIndex).toBe(7);
      expect(item.targetEntryEndIndex).toBe(8);
      expect(item.annotatorName).toBe('human-web');
    }
    expect(body.annotations[0]).toMatchObject({ typeId: 'quality.turn_outcome', value: 'bad' });
    expect(body.annotations[1]).toMatchObject({ typeId: 'quality.turn_flag', value: 'retry_loop' });
  });

  it('saveTurnLabels throws an actionable error when the server returns fewer ids than submitted items', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ ids: ['only-one'] }) });

    await expect(
      saveTurnLabels({
        sessionId: 'sess-1',
        entryIndex: 7,
        items: [
          { typeId: 'quality.turn_outcome', value: 'bad' },
          { typeId: 'quality.turn_flag', value: 'error' },
        ],
      }),
    ).rejects.toThrow(/returned 1 id\(s\) but 2 item\(s\)/);
  });
});
