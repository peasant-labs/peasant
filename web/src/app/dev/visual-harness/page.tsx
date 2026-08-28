'use client';

import { useEffect, useMemo, useState } from 'react';
import { notFound } from 'next/navigation';
import { TrajectoryGraph } from '@peasant-labs/fairtrade/graph';
import { TranscriptViewer, adaptTranscript, annotateTranscript, computeAnalytics } from '@peasant-labs/fairtrade/ui';
import '@xyflow/react/dist/style.css';
import { Moon, Sun } from 'lucide-react';
import { detectPhases } from '@/lib/insights';
import { displayProject } from '@/lib/quality/utils';
import type { AnnotationType } from '@/lib/api/annotations';
import { Copy } from 'lucide-react';
import { TurnLabelPopover } from '@/components/session-detail/v2/canvas/TurnLabelPopover';
import { sampleSession } from './sample-session';

type Theme = 'dark' | 'light';

/**
 * Visual-regression harness host — a DEV-ONLY fixture mount of the SAME shared
 * `@peasant-labs/fairtrade` `<TranscriptViewer>` composite the production
 * `/projects/[name]/[id]` page (the `SessionDetailV2` adapter) renders, fed a
 * bundled `SessionDetailPayload` fixture through the SAME `adaptTranscript` wire
 * adapter instead of the WebSocket `session_detail` subscription. This is
 * peasant's capture host: it lets the harness (`scripts/visual/`) drive every
 * transcript surface from a plain `next dev` with no backend/mock-store/auth, and
 * pair each shot against the canonical fairtrade demo (which renders the same
 * `sess_demo_0001`) for a true height-matched, same-data side-by-side.
 *
 * The route mounts the shared `<TranscriptViewer>` composite directly so the
 * capture host exercises the same component as production, not a separate
 * composer.
 *
 * It mounts the composite with the same shape the real adapter uses (detected
 * phases + derived pattern annotations feeding `computeAnalytics`, the @xyflow
 * graph in `graphSlot` (fairtrade's `/graph` entry owns that one engine), and
 * peasant's per-turn label popover in the `renderTurnActions` slot) but with all
 * capabilities on and host callbacks stubbed, so every action affordance renders
 * for capture. The composite owns a single bounded inner scroller
 * (`.txn-stream`) rather than scrolling the page, so the host below gives it a
 * fixed-height flex column matching the production shell's `--app-header-height`
 * contract; the capture script scrolls that inner container, not the window.
 * It 404s in a production build, so it never ships as a public route.
 */
export default function VisualHarnessPage() {
  // Not a product surface — only reachable under `next dev`. In a production build
  // (output: export) it 404s so it never ships as a public route.
  if (process.env.NODE_ENV === 'production') notFound();

  // Theme is driven exactly like the real app: `[data-theme]` on the document
  // element (fairtrade's token selectors are attribute-based). The harness owns it
  // deterministically — dark by default — and the `.theme-btn` flips it; the
  // capture script asserts the resulting `[data-theme]` value.
  const [theme, setTheme] = useState<Theme>('dark');
  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
  }, [theme]);

  // Client-only render (effective ssr:false for this DEV-ONLY route). The transcript
  // renders relative timestamps off the client clock, which the determinism harness
  // freezes — the server's real-clock SSR can't match it, producing a React hydration
  // mismatch (#418). Rendering nothing on the server + first paint, then mounting
  // client-side, removes the SSR HTML there is nothing to mismatch against. The
  // capture script waits for the surfaces to mount, so the one-tick delay is invisible.
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);

  const turns = useMemo(() => sampleSession.turns ?? [], []);
  const phases = useMemo(() => detectPhases(turns), [turns]);
  const annotations = useMemo(() => annotateTranscript(turns), [turns]);

  // The ONE wire→view projection (fairtrade's adapter) — same call shape
  // `SessionDetailV2` uses: derive analytics from the wire turns, then adapt
  // the fixture payload into the cooked `TranscriptViewModel` the composite
  // renders. No personal-medians context here (no quality WS in the harness);
  // `computeAnalytics` treats it as optional.
  const analytics = useMemo(
    () => computeAnalytics(turns, { scorecard: sampleSession.scorecard ?? undefined }),
    [turns],
  );
  const vm = useMemo(
    () => adaptTranscript({ ...sampleSession, turns }, undefined, analytics),
    [turns, analytics],
  );
  const toolVMsByTurn = useMemo(
    () => new Map(vm.turns.map((turn) => [turn.index, turn.toolCalls])),
    [vm],
  );

  // A self-contained per-turn label type so the label popover renders offline (the
  // real adapter fetches these from the backend). Entry-targetable + enumerated so
  // peasant's TurnLabelPopover shows a type list to capture.
  const labelTypes: AnnotationType[] = useMemo(
    () => [
      {
        typeId: 'turn-quality',
        version: 1,
        displayName: 'Turn quality',
        family: 'quality',
        class: 'manual',
        valueDomain: {
          kind: 'enumerated',
          datatype: 'text',
          permissibleValues: ['good', 'needs-work', 'blocker'],
        },
        status: 'active',
        origin: 'user',
        allowedTargetKinds: ['entry'],
      },
    ],
    [],
  );

  const project = sampleSession.project ?? 'transcript';

  // A stand-in streamPrelude mirroring SessionDetailV2's current production
  // shape. There is no real host state because scope is static here, but this is
  // enough to genuinely mount `.txn-stream-prelude` with real content inside
  // `.txn-stream`, which is what makes probe-peasant.mjs's prelude-position
  // diagnostic meaningful instead of vacuous (the element must actually exist
  // to check its computed position).
  const streamPrelude = (
    <div className="flex flex-col gap-3">
      <p className="text-[12px] text-ink-3">fixture session · no active scope</p>
    </div>
  );

  // A stand-in headerActions mirroring SessionDetailV2's "copy as markdown"
  // rehome — static here (no real clipboard write needed for
  // capture), but mounts the real button shape/location for the harness.
  const headerActions = (
    <button type="button" className="btn btn-secondary btn-sm">
      <Copy size={14} aria-hidden />
      copy as markdown
    </button>
  );

  // After all hooks: render nothing until mounted (see the client-only note above).
  if (!mounted) return null;

  return (
    <div
      data-theme={theme}
      // This dev route still mounts inside the shared root layout (`app/layout.tsx`'s
      // `<main className="... pt-[var(--app-header-height)] ...">`), which already reserves
      // `var(--app-header-height)` at the top even though no real TopNavbar renders on a `/dev/*`
      // route — the padding is unconditional. This wrapper uses the SAME Tailwind arbitrary-value
      // class `SessionDetailV2` uses (`h-[calc(100dvh-var(--app-header-height))]`) to claim
      // exactly the space below that reservation — a raw inline `style` calc() referencing the
      // custom property did not resolve reliably, so match production's proven class instead of
      // reinventing the subtraction. This harness's own 44px header strip is then a flex sibling
      // inside that same bound, with the composite taking the flex-1 remainder.
      className="flex h-[calc(100dvh-var(--app-header-height))] flex-col"
      style={{ background: 'var(--canvas)', color: 'var(--ink)' }}
    >
      {/* Harness chrome — identity + the theme toggle the capture script drives. Fixed height (not
          `minHeight`) so the composite's flex-1 remainder below stays exact regardless of content. */}
      <header
        style={{
          flexShrink: 0,
          display: 'flex',
          alignItems: 'center',
          gap: '1rem',
          padding: '0 1.25rem',
          height: '44px',
          background: 'var(--surface)',
          borderBottom: '1px solid var(--rule)',
          fontFamily: 'var(--font-mono)',
        }}
      >
        <strong style={{ fontSize: '16px', letterSpacing: '-0.01em' }}>
          Transcript visual harness
        </strong>
        <span style={{ color: 'var(--ink-3)', fontSize: '13px' }}>
          fixture · {sampleSession.id}
        </span>
        <button
          type="button"
          className="theme-btn"
          aria-label="toggle theme"
          title="toggle theme"
          onClick={() => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))}
          style={{
            marginLeft: 'auto',
            display: 'inline-flex',
            alignItems: 'center',
            gap: '0.4rem',
            padding: '0.35rem 0.6rem',
            background: 'var(--surface)',
            color: 'var(--ink)',
            border: '1px solid var(--rule)',
            borderRadius: '6px',
            cursor: 'pointer',
            font: 'inherit',
          }}
        >
          {theme === 'dark' ? <Moon size={16} aria-hidden /> : <Sun size={16} aria-hidden />}
          <span>{theme}</span>
        </button>
      </header>

      {/* The flex remainder below the harness's own 44px header, within the outer wrapper already
          bounded to `calc(100dvh-var(--app-header-height))` — `min-h-0` lets this item shrink
          below its content's natural size (without it, a flex item defaults to its content size
          and never actually clips): the composite owns exactly one inner scroller (`.txn-stream`),
          not the page. */}
      <div className="flex-1 min-h-0">
        <TranscriptViewer
          viewModel={vm}
          theme={theme}
          capabilities={{
            canEdit: false,
            canLabel: true,
            canContribute: false,
            canChangeVisibility: false,
            canExport: false,
          }}
          breadcrumb={[
            { label: 'Map', href: '/map' },
            {
              label: displayProject(project),
              href: `/map/${encodeURIComponent(project)}`,
            },
            { label: sampleSession.id.slice(0, 8) },
          ]}
          anchorHref={(turnIndex) => `#turn-${turnIndex}`}
          streamPrelude={streamPrelude}
          headerActions={headerActions}
          // The graph toggle mounts fairtrade's `/graph` @xyflow engine, the one
          // engine that entry still owns (graph topology/pan/zoom; visuals
          // are DS), matching `SessionDetailV2`'s `graphSlot` exactly.
          graphSlot={() => (
            <TrajectoryGraph
              turns={turns}
              toolVMsByTurn={toolVMsByTurn}
              filteredTurns={turns}
              phases={phases}
              annotations={annotations}
              searchMatches={[]}
              provider={sampleSession.harness}
            />
          )}
          // Mount peasant's real per-turn label control (the app-specific surface it
          // mounts in production via this same slot), so the harness captures the
          // actual label popover. Self-contained here (static type, no-op save).
          renderTurnActions={(turn) => (
            <TurnLabelPopover
              sessionId={sampleSession.id}
              entryIndex={turn.index}
              types={labelTypes}
              onSaved={() => {}}
            />
          )}
        />
      </div>
    </div>
  );
}
