import { describe, it, expect } from 'vitest';
import type { DecodedChangeDetailPayload } from '@/lib/api/map';
import { buildCaption, captionText, retryTaskCount, topModules } from './caption';
import { CHANGE_DETAIL_PAYLOAD, SESSION_A, makeTask } from './test-fixtures';

describe('caption assembly (deterministic, facts only)', () => {
  it('assembles the exact caption string from the fixture payload', () => {
    expect(captionText(CHANGE_DETAIL_PAYLOAD)).toBe(
      '+2 connections (1 rule break) · 14 files in internal/ingest, internal/api · 3 conversations, 21 requests · 2 requests took several attempts',
    );
  });

  it('anchors every fragment to its evidence section', () => {
    const fragments = buildCaption(CHANGE_DETAIL_PAYLOAD);
    expect(fragments.map((f) => [f.key, f.anchor])).toEqual([
      ['edges', 'review-slice'],
      ['files', 'review-files'],
      ['work', 'review-work'],
      ['retry', 'review-work'],
    ]);
    // The wrong-way count rides on the edges fragment for the danger glyph.
    expect(fragments[0].wrongWay).toBe(1);
  });

  it('omits the retry fragment when no task has a retry loop', () => {
    const payload: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      work: CHANGE_DETAIL_PAYLOAD.work.map((ws) => ({
        ...ws,
        tasks: ws.tasks.map((t) => ({ ...t, retryLoop: false })),
      })),
    };
    expect(retryTaskCount(payload)).toBe(0);
    expect(captionText(payload)).toBe(
      '+2 connections (1 rule break) · 14 files in internal/ingest, internal/api · 3 conversations, 21 requests',
    );
  });

  it('states "no connection changes" when the change has no edge deltas', () => {
    const payload: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      newEdges: [],
      removedEdges: [],
      violations: [],
    };
    expect(buildCaption(payload)[0].text).toBe('no connection changes');
  });

  it('renders removed-only and mixed edge deltas with the minus glyph', () => {
    const removedOnly: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      newEdges: [],
      removedEdges: [{ from: 'a', to: 'b', count: 1 }],
      violations: [],
    };
    expect(buildCaption(removedOnly)[0].text).toBe('−1 connection');

    const mixed: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      removedEdges: [{ from: 'a', to: 'b', count: 1 }],
      violations: [],
    };
    expect(buildCaption(mixed)[0].text).toBe('+2/−1 connections');
  });

  it('singularizes one-count facts', () => {
    const payload: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      newEdges: [{ from: 'a', to: 'b', count: 1 }],
      violations: [],
      files: [{ path: 'internal/ingest/one.go', status: 'M', linesAdded: 0, linesRemoved: 0 }],
      work: [
        {
          sessionId: SESSION_A,
          title: 't',
          harness: 'claude-code',
          binding: 'bound',
          tasks: [makeTask(SESSION_A, 0, { retryLoop: true })],
        },
      ],
    };
    expect(captionText(payload)).toBe(
      '+1 connection · 1 file in internal/ingest · 1 conversation, 1 request · 1 request took several attempts',
    );
  });

  it('drops the module clause when only root-level files changed', () => {
    const payload: DecodedChangeDetailPayload = {
      ...CHANGE_DETAIL_PAYLOAD,
      files: [
        { path: 'README.md', status: 'M', linesAdded: 0, linesRemoved: 0 },
        { path: 'Makefile', status: 'M', linesAdded: 0, linesRemoved: 0 },
      ],
    };
    expect(buildCaption(payload)[1].text).toBe('2 files');
  });

  it('states "no file changes" when the file list is empty', () => {
    const payload: DecodedChangeDetailPayload = { ...CHANGE_DETAIL_PAYLOAD, files: [] };
    expect(buildCaption(payload)[1].text).toBe('no file changes');
  });
});

describe('topModules', () => {
  it('ranks modules by file count and displays the first two directory segments', () => {
    expect(topModules(CHANGE_DETAIL_PAYLOAD.files)).toEqual([
      'internal/ingest',
      'internal/api',
    ]);
  });

  it('produces the expected names for top-level directories', () => {
    // "14 files in ingest, api" uses a single-segment
    // containing directory displays as itself.
    const files = [
      { path: 'ingest/a.go', status: 'M' },
      { path: 'ingest/b.go', status: 'M' },
      { path: 'api/c.go', status: 'M' },
    ];
    expect(topModules(files)).toEqual(['ingest', 'api']);
  });

  it('collapses deep subdirectories into their module — no leaf noise', () => {
    // The immediate parent leaf would be "[[...segments]]" / "v2" / "lib";
    // the module display is the first two segments of the containing dir.
    const files = [
      { path: 'web/src/app/review/[[...segments]]/page.tsx', status: 'M' },
      { path: 'web/src/components/session-detail/v2/SessionDetailV2.tsx', status: 'M' },
      { path: 'web/src/app/map/lib/mapData.ts', status: 'M' },
      { path: 'internal/codemap/review.go', status: 'M' },
    ];
    expect(topModules(files)).toEqual(['web/src', 'internal/codemap']);
  });

  it('breaks count ties alphabetically by module for determinism', () => {
    const files = [
      { path: 'internal/zeta/a.go', status: 'M' },
      { path: 'internal/alpha/b.go', status: 'M' },
    ];
    expect(topModules(files)).toEqual(['internal/alpha', 'internal/zeta']);
  });

  it('keeps same-leaf modules in different trees distinct', () => {
    // "internal/api" and "web/api" no longer collide on the leaf "api".
    const files = [
      { path: 'internal/api/a.go', status: 'M' },
      { path: 'internal/api/b.go', status: 'M' },
      { path: 'internal/api/v2/c.go', status: 'M' },
      { path: 'web/api/d.ts', status: 'M' },
    ];
    expect(topModules(files)).toEqual(['internal/api', 'web/api']);
  });
});
