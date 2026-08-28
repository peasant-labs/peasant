'use client';

import { Fragment, Suspense, useCallback, useEffect, useMemo, useState } from 'react';
import Link from 'next/link';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { ChevronDown, ChevronUp, Copy, X } from 'lucide-react';
import { TrajectoryGraph } from '@peasant-labs/fairtrade/graph';
// The demo's drop-in composite + its one wire→view adapter, plus the shared
// transcript helpers, all render the SAME composite as the demo.
import {
  TranscriptViewer,
  adaptTranscript,
  annotateTranscript,
  computeAnalytics,
  computePersonalMedians,
  type TranscriptInitialPosition,
  type TranscriptTab,
} from '@peasant-labs/fairtrade/ui';
import { useTheme } from '@/hooks/useTheme';
import '@xyflow/react/dist/style.css';
import { FeedbackPanel, Skeleton } from '@/lib/ft-ui';
import { useChannel } from '@/contexts/WebSocketContext';
import { subscribe } from '@/types/messages';
import type { SessionDetailPayload, QualityPayload } from '@/types/messages';
import { detectPhases } from '@/lib/insights';
import { displayProject } from '@/lib/quality/utils';
import { sessionsHref, transcriptHref, TranscriptScope, type ProjectHash, type TranscriptRouteQuery } from '@/lib/navigation/projectRoutes';
import { useEntryLabels } from './lib/useEntryLabels';
import {
  clearScopeQuery,
  collectFileTouches,
  originCrumb,
  prefilterTurns,
  scopeChipLabel,
  scopeTurns,
  TurnParam,
  turnsToMarkdown,
  type CrumbItem,
} from './lib/scopeTurns';
import { TurnLabelPopover } from './canvas/TurnLabelPopover';
import { TurnTouchedFiles } from './panels/TurnTouchedFiles';
import { adaptQualitySessions } from '@/lib/quality/types';

/**
 * The heading shown for a session that has no generated title yet. The hero
 * always receives a title, because the alternative — letting the viewer derive
 * one from the first prompt — puts recorded harness markup in the heading.
 */
export const UNTITLED_SESSION_TITLE = 'Untitled session';

interface SessionDetailV2Props {
  sessionId: string;
  projectHash: ProjectHash;
  projectName: string;
  routeQuery: TranscriptRouteQuery;
}

/**
 * Renders the demo's drop-in composite (`TranscriptViewer`) through the one
 * wire-to-view adapter (`adaptTranscript`). Peasant owns the *data layer* (the
 * WebSocket `session_detail` + `quality` subscriptions, phase detection, the
 * annotation REST client) and feeds it into the package via props/callbacks;
 * the package owns all rendering + view state.
 *
 * Peasant additionally owns the scoping layer: the viewer is the
 * destination of every "why" click from Map/Review, so it can open aimed —
 * `?scope=task|file|change&scopeVal=…` filters the turn list client-side,
 * `?origin=Map|Review` prepends an origin breadcrumb, and every turn that
 * touched files carries a touched-files panel (mounted through the package's
 * `renderTurnPanel` slot) linking its file touches back to the Map.
 * Scoping POLICY lives in this adapter; the rendering seams (`turns`,
 * `renderTurnPanel`, harness-derived provider) are package contract.
 */
export function SessionDetailV2(props: SessionDetailV2Props) {
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    setMounted(true);
  }, []);

  if (!mounted) {
    return <SessionDetailSkeleton />;
  }

  // useSearchParams requires a Suspense boundary above it for static export
  // (Next 15 CSR bailout); own it here so mounting pages need no changes.
  return (
    <Suspense fallback={<SessionDetailSkeleton />}>
      <SessionDetailV2Inner {...props} />
    </Suspense>
  );
}

function SessionDetailV2Inner({ sessionId, projectHash, projectName, routeQuery }: SessionDetailV2Props) {
  const { data: detail, error } = useChannel<SessionDetailPayload>(
    subscribe.sessionDetail(sessionId),
  );
  // The canonical wire permits `null` for an empty turn collection. Normalize
  // once at the mounted app boundary so every view/filter sees one stable list.
  const turns = useMemo(() => detail?.turns ?? [], [detail?.turns]);

  const searchParams = useSearchParams();
  const pathname = usePathname();
  const router = useRouter();
  const scope = routeQuery;
  // Clearing the scope removes the scope params but keeps the session (and
  // the origin params, so the origin breadcrumb still offers the way back).
  const clearScope = useCallback(() => {
    router.replace(`${pathname}${clearScopeQuery(searchParams)}`);
  }, [router, pathname, searchParams]);

  // The quality channel feeds the personal-median comparison line on the
  // scorecard. The package never fetches it — peasant computes the medians and
  // passes them in.
  const { data: quality } = useChannel<QualityPayload>(subscribe.quality());
  const medians = useMemo(
    () =>
      computePersonalMedians(
        // computePersonalMedians (fairtrade/ui) takes the raw wire
        // QualitySession from @peasant-labs/schema, where `outcome` is a
        // required (possibly-empty) string. adaptQualitySessions narrows an
        // absent/not-yet-computed outcome to `undefined` for THIS app's own
        // analytics-safe QualitySession type (see web/src/lib/quality/types.ts);
        // bridge back to an empty string here so a session whose quality
        // metrics haven't resolved yet still contributes to the personal
        // median comparison instead of failing the package's type contract.
        adaptQualitySessions(quality?.sessions ?? []).map((session) => {
          // Personal medians consume only scorecard inputs. Excluding the
          // annotation list keeps this older viewer dependency independent of
          // newly-valid annotation target kinds on the current wire contract.
          const { effectiveAnnotations: _effectiveAnnotations, ...medianSession } = session;
          return {
            ...medianSession,
            outcome: session.outcome ?? '',
          };
        }),
      ),
    [quality?.sessions],
  );

  // The generated title for THIS session, reused from the quality subscription
  // already mounted above for the medians. `SessionDetailPayload` carries no
  // title, and the server only stores one for some sessions, so this stays
  // optional and the breadcrumb falls back to the short id.
  const sessionTitle = useMemo(() => {
    const found = quality?.sessions?.find((s) => s.id === sessionId)?.title?.trim();
    return found ? found : undefined;
  }, [quality?.sessions, sessionId]);

  // Session metrics disclosure, collapsed by default: the transcript is what
  // you came to read, and duration/turns/tools/tokens are reference figures.
  const [metricsOpen, setMetricsOpen] = useState(false);

  // The viewer's tab, lifted so the host can react to it. The composite
  // renders its keyboard-hint strip on every tab; peasant shows it only on
  // `highlights` (see `.txn-hints-hidden` in globals.css), which needs to know
  // which tab is active. 'trace' is the composite's own default, so lifting the
  // state does not change which tab you land on.
  const [activeTab, setActiveTab] = useState<TranscriptTab>('trace');

  // Entry-level (per-turn) custom labels — backend annotations + optimistic
  // saves, keyed by entry index. Rendered as chips on each turn + driving the
  // host-owned label popover mounted via `renderTurnActions`.
  const { entryTypes, labelsByEntry, addLabel } = useEntryLabels(sessionId);

  // The list handed to the package: prefilter → active scope. When no scope
  // filter is active we pass nothing (whole-session, package owns prefilter).
  // The composite's FiltersRail owns thinking and tool-call visibility, so the
  // host does not apply a second focus filter.
  const filteringScope =
    scope.scope === TranscriptScope.Task ||
    scope.scope === TranscriptScope.File ||
    scope.scope === TranscriptScope.Change;
  const displayTurns = useMemo(() => {
    if (!detail || !filteringScope) return undefined;
    const list = scopeTurns(prefilterTurns(turns), scope.scope, scope.scopeVal);
    return list;
  }, [detail, filteringScope, scope.scope, scope.scopeVal, turns]);

  const requestedTurn = routeQuery.turn;
  const requestedTurnExists = requestedTurn == null || turns.some((turn) => turn.index === requestedTurn);
  const requestedTurnVisible = requestedTurn == null || (displayTurns ?? turns).some((turn) => turn.index === requestedTurn);
  const initialPosition = useMemo<TranscriptInitialPosition | undefined>(() => {
    if (requestedTurn == null) return { kind: 'top' };
    if (!requestedTurnExists) return { kind: 'top', requestKey: `missing-turn:${requestedTurn}` };
    if (!requestedTurnVisible) return undefined;
    return { kind: 'turn', turnIndex: requestedTurn };
  }, [requestedTurn, requestedTurnExists, requestedTurnVisible]);

  const revealRequestedTurn = useCallback(() => {
    if (filteringScope) router.replace(`${pathname}${clearScopeQuery(searchParams)}`);
  }, [filteringScope, pathname, router, searchParams]);

  const clearMissingTurn = useCallback(() => {
    const next = new URLSearchParams(searchParams);
    next.delete(TurnParam);
    const query = next.toString();
    router.replace(`${pathname}${query ? `?${query}` : ''}`);
  }, [pathname, router, searchParams]);

  // Copy-as-Markdown (roadmap 4.7): copies exactly what's shown (focus/scope
  // respected), so it pastes cleanly into an issue/PR/doc.
  const [copied, setCopied] = useState(false);
  const copyMarkdown = useCallback(() => {
    const md = turnsToMarkdown(displayTurns ?? turns);
    void navigator.clipboard?.writeText(md);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  }, [displayTurns, turns]);

  // Host-derived inputs the package takes as props (it never derives them).
  // Both are POSITIONAL over the rendered list, so they must be computed from
  // the exact list handed to the package — the filtered list while any filter is
  // active — or phase dividers and error/retry markers land on wrong turns.
  const phases = useMemo(
    () => (detail ? detectPhases(displayTurns ?? turns) : []),
    [detail, displayTurns, turns],
  );
  const annotations = useMemo(
    () => (detail ? annotateTranscript(displayTurns ?? turns) : []),
    [detail, displayTurns, turns],
  );

  // The ONE wire→view projection (fairtrade's adapter): scope/focus filtering
  // happens on the WIRE turns before cooking, so the composite renders exactly
  // the filtered conversation. Analytics carry the scorecard + personal
  // medians into the cooked scorecard bands.
  const vm = useMemo(() => {
    if (!detail) return null;
    const visibleTurns = displayTurns ?? turns;
    const analytics = computeAnalytics(visibleTurns, {
      scorecard: detail.scorecard ?? undefined,
      medians,
    });
    const adapted = adaptTranscript(
      { ...detail, turns: visibleTurns },
      undefined,
      analytics,
    );
    // `SessionVM.title` is documented as "render-when-present; else the consumer
    // derives one from the first prompt". Deriving is never right here: the
    // first prompt of a recorded session often opens with harness markup, so
    // the hero rendered that markup as the heading (the
    // "<local-command-caveat>Caveat: ..." heading). Title is not a wire field,
    // so the host has to supply one ALWAYS: the generated title when the
    // quality channel has one, and a plain placeholder when it does not, which
    // keeps the derived fallback from ever running.
    return {
      ...adapted,
      session: { ...adapted.session, title: sessionTitle ?? UNTITLED_SESSION_TITLE },
    };
  }, [detail, displayTurns, medians, turns, sessionTitle]);

  // Cooked tool calls by turn index, fed into the graph engine's tool nodes.
  const toolVMsByTurn = useMemo(
    () => new Map((vm?.turns ?? []).map((turn) => [turn.index, turn.toolCalls])),
    [vm],
  );

  const { theme } = useTheme();

  // Project + breadcrumb for the origin-aware trail (Map/Review origin crumb).
  const project = detail?.project ?? projectName;
  const crumb = originCrumb(scope, projectHash);
  // The trail is projects / {project} / {session} — the path you walked to get
  // here, each crumb being the page it names. The project crumb leads to that
  // project's SESSION LIST, not to the map: the map is capability-gated, so a
  // map crumb is a dead end on a default server and there is no reason to route
  // a reader there from a transcript.
  //
  // The project crumb keeps the project NAME rather than a literal "sessions",
  // because it is the only part of the trail that says which project you are in.
  //
  // The last crumb is the session ID, NOT its title: the hero renders the title
  // directly beneath the trail, so repeating it there says the same thing twice
  // and — titles being a sentence long — pushes the crumbs that actually
  // navigate off to the side. The ID is fixed-width and adds the one
  // identifying fact the hero does not show.
  //
  // An origin crumb (map · node, changes · branch) is inserted after the root
  // when you arrived from one of those surfaces, so the way back to where you
  // actually were survives alongside the way back to the top.
  const sessionCrumb = (detail?.id ?? sessionId).slice(0, 8);
  const breadcrumb: CrumbItem[] = [
    { label: 'projects', href: '/' },
    ...(crumb ? [crumb] : []),
    { label: displayProject(project), href: sessionsHref(projectHash) },
    { label: sessionCrumb },
  ];

  if (!detail) {
    // A channel failure with nothing loaded must be VISIBLE — an endless
    // skeleton with the error swallowed is how a dead backend hides.
    if (error) {
      return (
        <div className="max-w-[1600px] mx-auto px-6 pt-6">
          <FeedbackPanel variant="error" title="transcript unavailable">
            {String(error)}
          </FeedbackPanel>
        </div>
      );
    }
    return <SessionDetailSkeleton />;
  }

  // Relativizes touched-file paths to repo-relative Map node ids.
  const workingDirectory = detail.workingDirectory;

  // Host-owned status context contains state the reader needs: the active-scope
  // chip plus stale-link and connection-error status. View controls remain in
  // the shared composite, while copy-as-markdown is a header action below.
  const streamPrelude = (
    <div className="flex flex-col gap-3">
      {filteringScope && scope.scope && (
        <ScopeChipRow
          label={scopeChipLabel(scope.scope, scope.scopeVal)}
          onClear={clearScope}
        />
      )}
      {requestedTurn != null && !requestedTurnExists && (
        <div role="alert">
          <FeedbackPanel variant="empty" title="linked turn is no longer available">
            Turn {requestedTurn} is absent from this transcript, so the viewer opened at the top. Remove the stale turn target and copy a fresh link.
            {' '}
            <button type="button" onClick={clearMissingTurn}>remove stale turn target</button>
          </FeedbackPanel>
        </div>
      )}
      {requestedTurn != null && requestedTurnExists && !requestedTurnVisible && (
        <div role="status">
          <FeedbackPanel variant="empty" title="linked turn is hidden by the current view">
            Turn {requestedTurn} exists, but the current scope hides it. Reveal the full transcript to open the linked turn.
            {' '}
            <button type="button" onClick={revealRequestedTurn}>reveal linked turn</button>
          </FeedbackPanel>
        </div>
      )}
      {error != null && (
        <p className="text-[12px] text-danger">
          connection error; showing the last loaded transcript.
        </p>
      )}
    </div>
  );

  // Copy-as-Markdown's new home: a session-level header action
  // (via the composite's `headerActions` slot) rather than the removed
  // prelude's toggle bar. Still copies exactly what's shown (scope respected).
  const headerActions = (
    <>
      {/* Session metrics disclosure. The composite's hero always renders the
          duration/turns/tools/tokens strip; those are reference figures you
          consult occasionally, not identity you need on screen while reading a
          transcript. The composite exposes no prop for it, so the toggle lives
          here and the strip is hidden by a data attribute on the wrapper below
          (see `.txn-metrics-collapsed` in globals.css).

          Only the `.metaitem` figures collapse — the outcome / harness / model
          chips in the same row are identity, so they stay put. */}
      <button
        type="button"
        onClick={() => setMetricsOpen((open) => !open)}
        aria-expanded={metricsOpen}
        title={metricsOpen ? 'hide the session metrics' : 'show duration, turns, tools and tokens'}
        className="btn btn-secondary btn-sm"
      >
        {metricsOpen ? <ChevronUp size={14} aria-hidden /> : <ChevronDown size={14} aria-hidden />}
        details
      </button>
      <button
        type="button"
        onClick={copyMarkdown}
        title="copy the conversation shown above (respecting the active scope) as markdown text"
        className="btn btn-secondary btn-sm"
      >
        <Copy size={14} aria-hidden />
        {copied ? 'copied' : 'copy as markdown'}
      </button>
    </>
  );

  return (
    // The app shell publishes one responsive header height for both its main
    // offset and bounded viewers, including the mobile two-row header.
    <div
      data-tour="transcript-view"
      className={[
        'flex h-[calc(100dvh-var(--app-header-height))] flex-col',
        metricsOpen ? '' : 'txn-metrics-collapsed',
        activeTab === 'highlights' ? '' : 'txn-hints-hidden',
      ]
        .filter(Boolean)
        .join(' ')}
    >
      <div className="flex-1 min-h-0">
        <TranscriptViewer
          viewModel={vm!}
          theme={theme}
          activeTab={activeTab}
          onTabChange={setActiveTab}
          // Local capabilities: labelling is supported (via renderTurnActions);
          // contribute / visibility / edit / export are village-only or absent.
          capabilities={{
            canEdit: false,
            canLabel: entryTypes.length > 0,
            canContribute: false,
            canChangeVisibility: false,
            canExport: false,
          }}
          callbacks={{
            onCopyLink: () => {
              void navigator.clipboard?.writeText(
                `${window.location.origin}${transcriptHref(projectHash, detail.id)}`,
              );
            },
          }}
          // Origin-aware host trail through the app router.
          breadcrumb={breadcrumb}
          LinkComponent={Link}
          initialPosition={initialPosition}
          // Per-turn copied anchors are full permalinks that keep the live
          // scope/origin params (replacing only `turn`), so a copied link
          // stays inside the scoped "room" — the pre-composite link shape.
          anchorHref={(turnIndex) => {
            const params = new URLSearchParams(searchParams);
            params.set(TurnParam, String(turnIndex));
            const base = transcriptHref(projectHash, detail.id);
            return `${base}${params.toString() ? `?${params.toString()}` : ''}`;
          }}
          streamPrelude={streamPrelude}
          headerActions={headerActions}
          // The graph toggle mounts fairtrade's @xyflow engine (`/graph`), which
          // owns graph topology, pan, and zoom while Fairtrade owns visuals.
          graphSlot={() => (
            <TrajectoryGraph
              turns={displayTurns ?? turns}
              toolVMsByTurn={toolVMsByTurn}
              filteredTurns={displayTurns ?? turns}
              phases={phases}
              annotations={annotations}
              searchMatches={[]}
              provider={detail.harness}
            />
          )}
          // Each turn carries its touched files under the card. The
          // panel reads WIRE turns (tool-call paths), so look the turn back up.
          renderTurnPanel={(turn) => {
            const wire = (displayTurns ?? turns).find((w) => w.index === turn.index);
            if (!wire) return null;
            const touches = collectFileTouches([wire], workingDirectory)[0];
            if (!touches) return null;
            return (
              <TurnTouchedFiles
                touches={touches}
                projectHash={projectHash}
                activeFile={scope.scope === TranscriptScope.File ? scope.scopeVal : undefined}
              />
            );
          }}
          // Peasant's TYPED label model (annotation-type registry): the
          // restored outcome+flag modal is backed by the real
          // quality.turn_outcome/quality.turn_flag types, plus a
          // secondary "more labels" picker for the rest of the registry
          // (custom free text, system classifiers) — saved chips + both
          // popovers are host-owned, not the composite's built-in label.
          renderTurnActions={(turn) => (
            <span className="inline-flex items-center gap-1.5">
              {(labelsByEntry.get(turn.index) ?? []).map((label) => (
                <span key={label.id || `${label.typeId}:${label.value}`} className="chip" title={label.typeName}>
                  {label.value}
                </span>
              ))}
              {entryTypes.length > 0 && (
                <TurnLabelPopover
                  sessionId={sessionId}
                  entryIndex={turn.index}
                  types={entryTypes}
                  savedLabels={labelsByEntry.get(turn.index)}
                  onSaved={addLabel}
                />
              )}
            </span>
          )}
        />
      </div>
    </div>
  );
}

/**
 * Minimal breadcrumb for adapter-owned states (the change-scope empty state).
 * The package renders its own breadcrumb in the hero; this exists only where
 * the package is skipped, so the origin crumb's way back is never lost.
 */
function CrumbTrail({ items }: { items: CrumbItem[] }) {
  return (
    <nav
      aria-label="Breadcrumb"
      className="flex flex-wrap items-center gap-1.5 text-[12px] text-ink-3"
    >
      {items.map((item, i) => (
        <Fragment key={`${item.label}-${i}`}>
          {i > 0 && (
            <span aria-hidden className="text-ink-4">
              /
            </span>
          )}
          {item.href ? (
            <Link
              href={item.href}
              className="hover:text-ink hover:underline focus-mono cursor-pointer transition-colors"
            >
              {item.label}
            </Link>
          ) : (
            <span className="text-ink">{item.label}</span>
          )}
        </Fragment>
      ))}
    </nav>
  );
}

/** Square, font-mono chip naming the active scope, with an "x" to clear it. */
function ScopeChipRow({ label, onClear }: { label: string; onClear: () => void }) {
  return (
    <div className="flex min-w-0 items-center gap-2">
      {/* "Filtered" (plain word) over "Scope" (jargon): the chip text already
          spells out the filter in a sentence; the eyebrow just groups it. */}
      <span
        className="v2-eyebrow shrink-0"
        title="This page is showing a slice of the conversation, not the whole thing"
      >
        filtered
      </span>
      <span className="inline-flex min-w-0 items-center gap-1.5 border border-rule px-2 py-1 font-mono text-[11px] leading-none text-ink">
        {/* Truncate long file-scope paths so the chip never blows out the row. */}
        <span className="min-w-0 truncate" title={label}>
          {label}
        </span>
        <button
          type="button"
          aria-label="Clear scope"
          onClick={onClear}
          className="shrink-0 text-ink-4 hover:text-ink focus-mono cursor-pointer transition-colors"
        >
          <X size={11} strokeWidth={1.75} />
        </button>
      </span>
    </div>
  );
}

function SessionDetailSkeleton() {
  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12">
      {/* Header block: breadcrumb + title + subtitle stand-ins. */}
      <Skeleton avatar={false} lines={3} label="Loading session header" />
      {/* Transcript body stand-in: an avatar row plus a run of content lines. */}
      <Skeleton lines={10} label="Loading transcript" className="mt-6" />
    </div>
  );
}
