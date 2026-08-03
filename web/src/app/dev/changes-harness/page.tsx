'use client';

import { useEffect, useState } from 'react';
import { notFound } from 'next/navigation';
import { Changes, ChangeDetail } from '@peasant-labs/fairtrade/graph';
import type {
  ChangesPayload,
  ChangeDetailPayload,
  ChangeDiffPayload,
} from '@peasant-labs/fairtrade/graph';
// The lifted graph surface needs the full fairtrade CSS the demo rendered over:
// tokens (incl. the [data-theme="light"] overrides that live ONLY in tokens.css),
// the base layer, and the `.gmp-`/`.cg-` surface rules (graph.css). components.css
// + fonts.css are already loaded globally by the app's root layout.tsx; we add the
// remaining three here so this route renders the surface identically to the demo.
import '@peasant-labs/fairtrade/tokens.css';
import '@peasant-labs/fairtrade/base.css';
import '@peasant-labs/fairtrade/graph.css';

type Theme = 'dark' | 'light';
type Surface = 'changes' | 'change-detail';

/**
 * Visual-regression harness for the lifted graph surfaces — a DEV-ONLY fixture
 * mount of the SAME shared `@peasant-labs/fairtrade/graph` `<Changes>` /
 * `<ChangeDetail>` the production `/review` surface (`ReviewSurface`) renders, fed
 * a bundled fixture instead of the `/api/v1/review` REST payloads. It lets the
 * capture harness drive the surfaces from a plain `next dev` with no backend, and
 * pair each shot against the canonical fairtrade demo (which renders these exact
 * lifted components over the same fixture) for a true same-component side-by-side.
 *
 * The fixtures (CHANGES_FIXTURE, CHANGE_DETAIL_FIXTURE, DIFF_BY_FILE), the pinned
 * clock (CHANGES_NOW), the selected row, the initially-open file and the annotation
 * are copied verbatim from the demo's mockups/inuse/GraphMap.jsx so demo and app
 * render byte-identically.
 *
 * It 404s in a production build (output: export), so it never ships as a public route.
 */

/* ---- <Changes> fixture (copied verbatim from the demo GraphMap.jsx) ---------- */
const CHANGES_NOW = Date.UTC(2026, 5, 27, 12, 0, 0);
const H = 3600e3;
const D = 24 * H;

function timelineCommit(
  hash: string,
  subject: string,
  timeMs: number,
  sessionIds: string[],
): ChangesPayload['recentCommits'][number] {
  return {
    hash,
    subject,
    timeMs,
    hasSession: sessionIds.length > 0,
    sessionIds,
    associations: sessionIds.map((sessionId) => ({
      id: `fixture:${hash}:${sessionId}`,
      sessionId,
      conclusion: 'confirmed',
      confidence: 'high',
      evidence: [{ kind: 'recorded_commit', recordedCommitHash: hash }],
    })),
  };
}

const CHANGES_FIXTURE: ChangesPayload = {
  repoFound: true,
  defaultBranch: 'develop',
  recentCommits: [
    timelineCommit('c1d4a3', 'squarify treemap', CHANGES_NOW - 5 * H, ['session-map']),
    timelineCommit('d14c0a', 'Merge fix--kickstart-config', CHANGES_NOW - 3 * D, []),
    timelineCommit('b7e220', 'stream replay path', CHANGES_NOW - D, ['session-replay']),
    timelineCommit('a3f9c1', 'Fix pipeline bug', CHANGES_NOW - 2 * D, ['session-pipeline', 'session-debug']),
    timelineCommit('8c0e41', 'Bump deps', CHANGES_NOW - 2 * D, []),
    timelineCommit('f1a920', 'Add redaction tests', CHANGES_NOW - 3 * D, ['session-redaction']),
  ],
  changes: [
    { branch: 'feat/map-review-contribute', merged: false, baseHash: 'a3f9c1', tipCommitMs: CHANGES_NOW - 5 * H, sessionCount: 2, aheadCount: 3, behindCount: 0, filesChanged: 136, taskCount: 70, newEdges: 39, removedEdges: 22, violations: 3 },
    { branch: 'fix--kickstart-config', merged: true, baseHash: 'a3f9c1', mergeCommitHash: 'd14c0a', mergedAtMs: CHANGES_NOW - 3 * D, sessionCount: 0, aheadCount: 1, behindCount: 0, filesChanged: 4, taskCount: 2, newEdges: 0, removedEdges: 0, violations: 0 },
    { branch: 'feat/experimental-cache', merged: true, reverted: true, mergedAtMs: CHANGES_NOW - 7 * D, sessionCount: 0, aheadCount: 0, behindCount: 0, filesChanged: 0, taskCount: 0, newEdges: 0, removedEdges: 0, violations: 0 },
  ],
  // Normalized sessions backing the recentCommits sessionIds above (every id
  // referenced there resolves here, matching the producer's referential-validity
  // contract) — the demo fixture predates this field, so there is no verbatim
  // source to copy; ids/titles are illustrative and topically match their commit.
  sessions: [
    { sessionId: 'session-map', harness: 'claude-code', hasCommitBinding: true, title: 'squarify the treemap layout' },
    { sessionId: 'session-replay', harness: 'claude-code', hasCommitBinding: true, title: 'add the stream replay path' },
    { sessionId: 'session-pipeline', harness: 'claude-code', hasCommitBinding: true, title: 'fix the ingest pipeline bug' },
    { sessionId: 'session-debug', harness: 'codex', hasCommitBinding: true, title: 'debug the pipeline regression' },
    { sessionId: 'session-redaction', harness: 'claude-code', hasCommitBinding: true, title: 'add redaction tests' },
  ],
  rewrittenCommits: [],
};

/* ---- <ChangeDetail> fixtures (copied verbatim from the demo GraphMap.jsx) ----- */
const DETAIL_BRANCH = 'feat/map-review-contribute';
const DIFF_BY_FILE: Record<string, ChangeDiffPayload> = {
  'internal/codegraph/build.go': {
    branch: DETAIL_BRANCH, file: 'internal/codegraph/build.go', status: 'A', binary: false, truncated: false,
    hunks: [{
      oldStart: 1, oldLines: 1, newStart: 1, newLines: 6,
      sessionId: 'c1d4a3f9', sessionTitle: 'Build deterministic node layout',
      lines: [
        { kind: 'context', text: 'package codegraph' },
        { kind: 'add', text: '' },
        { kind: 'add', text: 'func Build(g *Graph) *Layout {' },
        { kind: 'add', text: '  nodes := squarify(g.Nodes())' },
        { kind: 'add', text: '  return &Layout{Nodes: nodes}' },
        { kind: 'add', text: '}' },
      ],
    }],
  },
  'internal/ingest/pipeline.go': {
    branch: DETAIL_BRANCH, file: 'internal/ingest/pipeline.go', status: 'M', binary: false, truncated: false,
    hunks: [
      {
        oldStart: 211, oldLines: 3, newStart: 211, newLines: 3,
        sessionId: 'a3f9c1d4', sessionTitle: 'Refactor ingest pipeline to stream',
        lines: [
          { kind: 'context', text: 'func (p *Pipeline) Run(ctx context.Context) error {' },
          { kind: 'del', text: '  sessions, err := loadAll(ctx, p.src)' },
          { kind: 'add', text: '  stream, err := openStream(ctx, p.src)' },
          { kind: 'context', text: '  if err != nil { return err }' },
        ],
      },
      {
        oldStart: 240, oldLines: 2, newStart: 240, newLines: 4,
        sessionId: '7b21e0aa', sessionTitle: 'Add backpressure to the reader',
        lines: [
          { kind: 'context', text: '  for s := range stream {' },
          { kind: 'add', text: '    sem <- struct{}{}' },
          { kind: 'add', text: '    go p.process(s, sem)' },
          { kind: 'context', text: '  }' },
        ],
      },
    ],
  },
  'cmd/peasant/main.go': {
    branch: DETAIL_BRANCH, file: 'cmd/peasant/main.go', status: 'D', binary: false, truncated: false,
    hunks: [{
      oldStart: 1, oldLines: 2, newStart: 0, newLines: 0,
      sessionId: 'ff21e8a0', sessionTitle: 'Drop the legacy entrypoint',
      lines: [
        { kind: 'del', text: 'package main' },
        { kind: 'del', text: 'func main() { legacy.Run() }' },
      ],
    }],
  },
};

const rep = <T,>(n: number, f: (i: number) => T): T[] => Array.from({ length: n }, (_, i) => f(i));
const CHANGE_DETAIL_FIXTURE: ChangeDetailPayload = {
  branch: DETAIL_BRANCH,
  baseRef: 'a3f9c1',
  defaultBranch: 'develop',
  files: [
    { path: 'internal/codegraph/build.go', status: 'A', linesAdded: 5, linesRemoved: 0 },
    { path: 'internal/ingest/pipeline.go', status: 'M', linesAdded: 3, linesRemoved: 1 },
    { path: 'cmd/peasant/main.go', status: 'D', linesAdded: 0, linesRemoved: 2 },
  ],
  slice: { nodes: [], structureEdges: [], activityEdges: [] },
  newEdges: rep(39, (i) => ({ from: `n${i}`, to: `m${i}`, count: 1 })),
  removedEdges: rep(22, (i) => ({ from: `r${i}`, to: `s${i}`, count: 1 })),
  newNodes: [],
  removedNodes: [],
  violations: rep(3, (i) => ({ kind: 'cycle', from: `c${i}`, to: `d${i}` })),
  work: rep(70, (i) => ({ sessionId: `sess-${i}`, title: `conversation ${i}`, harness: 'claude-code', binding: 'bound', tasks: [] })),
  unrecordedCommits: [],
  unusual: [],
  frictions: [
    { kind: 'retryLoop', label: 'retry loop', file: 'pipeline.go', count: 4, sessions: 2 },
    { kind: 'recurring', label: 'unparsed import', file: 'build.go', count: 2, sessions: 2 },
  ],
  insights: [],
  filesChanged: 136,
  linesAdded: 8,
  linesRemoved: 3,
  outputTokens: BigInt(12400),
  costUsd: 4.81,
};

export default function ChangesHarnessPage() {
  // Not a product surface — only reachable under `next dev`. In a production build
  // (output: export) it 404s so it never ships as a public route.
  if (process.env.NODE_ENV === 'production') notFound();

  // Client-only render (effective ssr:false for this DEV-ONLY route): theme +
  // surface ride on the query string, read after mount so there is no SSR HTML to
  // mismatch. Theme is driven exactly like the real app — `[data-theme]` on the
  // document element (fairtrade's token selectors are attribute-based). Default dark.
  const [mounted, setMounted] = useState(false);
  const [surface, setSurface] = useState<Surface>('changes');
  const [theme, setTheme] = useState<Theme>('dark');
  const [annotation, setAnnotation] = useState('good handoff');

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    if (params.get('surface') === 'change-detail') setSurface('change-detail');
    const t: Theme = params.get('theme') === 'light' ? 'light' : 'dark';
    setTheme(t);

    // No harness-local font injection: the Atkinson typefaces are loaded by the
    // PRODUCTION font path — the root layout's <link rel="stylesheet"> in <head>
    // (fonts.css Option B) — which this dev route inherits like every other route.
    // The captured SUBJECT must use the same font path as the real /review surface.

    setMounted(true);
  }, []);

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
  }, [theme]);

  if (!mounted) return null;

  // The capture is scoped to `.gmp-changes-root` / `.gmp-detail-root`. Two demo-shell
  // geometry facts have to be reproduced for a byte-exact, DIM-free imgdiff:
  //   1) WIDTH: the demo's in-use shell renders the surface in a 1364px-wide content
  //      box. A fixed 1364px wrapper reproduces it exactly (the demo's inert
  //      `max-width:1400px` override is a no-op at 1364px, and `--gmp-pad-x` keys off
  //      element width).
  //   2) HEIGHT: `.gmp-root` carries `height:100%`, so inside the demo's stage it
  //      STRETCHES to fill the viewport when content is short (e.g. <Changes>, ~549px
  //      of content stretched to 863px — the rest is blank --canvas), and GROWS past
  //      it when content is tall (<ChangeDetail>). A plain wrapper let it collapse to
  //      content height (→ a height/DIM mismatch). We reproduce the stretch with a
  //      `min-height` equal to the demo stage height = viewport − the in-use chrome
  //      (header var(--nav-h)=56 + the section subnav ≈49 + the .iu-view padding
  //      2×var(--sp-4)=32 ≈ 137px); tall content still overflows past it.
  const holderStyle = { width: 1364 } as const;
  return (
    <div data-theme={theme} style={{ background: 'var(--canvas)', minHeight: '100vh' }}>
      <style>{`.cx-holder > .gmp-root { min-height: calc(100svh - 137px); }`}</style>
      <div className="cx-holder" style={holderStyle}>
        {surface === 'changes' ? (
          <Changes
            payload={CHANGES_FIXTURE}
            projectLabel="peasant-labs/peasant"
            nowMs={CHANGES_NOW}
            selectedId="tip:feat/map-review-contribute"
            onSelectChange={() => {}}
            onOpenMap={() => {}}
          />
        ) : (
          <ChangeDetail
            payload={CHANGE_DETAIL_FIXTURE}
            getDiff={(f) => DIFF_BY_FILE[f.path] ?? null}
            initialOpenFiles={{ 'internal/ingest/pipeline.go': true }}
            annotation={annotation}
            onSaveAnnotation={setAnnotation}
            onRemoveAnnotation={() => setAnnotation('')}
          />
        )}
      </div>
    </div>
  );
}
