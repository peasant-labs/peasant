"use client";

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useChannel } from "@/contexts/WebSocketContext";
import { discoveryErrorMessage } from "@/lib/selectionGuidance";
import { DiscoveryErrorCode, discoveryErrorCode } from "@/lib/api/errors";
import { Breadcrumbs } from "@/components/Breadcrumbs";
import {
  ProjectPicker,
  PickerExplainer,
  rowsFromSummaries,
  rowsFromSessions,
  type PickerRow,
} from "@/components/picker/ProjectPicker";
import {
  ProjectListState,
  SelectionRecoveryPanel,
  projectListState,
} from "@/components/picker/SelectionRecoveryPanel";
import { ExplainerToggle, useExplainer } from "@/components/Explainer";
import { Skeleton, SkeletonList } from "@/lib/skeleton";
import type { SessionsPayload, SessionSummary } from "@/types/messages";
import type { ReviewListPayload } from "@peasant-labs/schema";
import { cachedProjectSummaries, fetchProjectSummaries, fetchReviewChanges, type DecodedProjectSummariesPayload } from "@/lib/api/map";
import { displayProject } from "@/lib/quality/utils";
import { parseProjectHash } from "@/lib/navigation/projectRoutes";
import { ChangeGraph } from "@/app/review/[[...segments]]/ChangeGraph";
import { formatRelative } from "@/app/review/[[...segments]]/format";
import {
  StatGrid,
  DataState,
  FeedbackPanel,
  TeachingEmptyState,
  type TileSpec,
} from "@/lib/ft-ui";
import { FolderOpen, MessageSquare, Sparkles, GitBranch, EyeOff, ChevronDown, ChevronUp } from "lucide-react";
import { AllSessions } from "@/components/sessions/AllSessions";
import { useSessionTitles } from "@/hooks/useSessionTitles";
import { UNASSIGNED_PROJECT } from "@/app/review/[[...segments]]/sessions";

const CHANNELS: ["sessions"] = ["sessions"];

// ---------------------------------------------------------------------------
// Projects home: `/` is the PROJECT PICKER, with every ingested session listed
// beneath it.
// The ledger line stays on top; one row per project (sessions · recorded
// coverage · last work · open changes) from GET /api/v1/projects/summary,
// falling back to sessions-channel grouping (stats unavailable) while the
// fetch loads or fails. A row click lands on /sessions/{projectHash} — that
// project's session list (the shared ProjectPicker, destination="sessions").
// Single-project installs skip the picker and embed that project's Changes
// list directly. The Map lives at /map now.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Summary stats (E1) — aggregate across all projects, built from picker rows.
// ---------------------------------------------------------------------------

interface SummaryStats {
  projects: number;
  sessions: number;
  recorded: number;
  total: number;
  hasCoverage: boolean;
  open: number;
  hasOpen: boolean;
  lastWorkMs: number | null;
}

function summaryStats(rows: PickerRow[], totalSessions: number): SummaryStats {
  let recorded = 0;
  let total = 0;
  let open = 0;
  let hasCoverage = false;
  let hasOpen = false;
  let lastWorkMs: number | null = null;
  for (const r of rows) {
    if (r.recordedFiles !== null && r.totalFiles !== null) {
      recorded += r.recordedFiles;
      total += r.totalFiles;
      hasCoverage = true;
    }
    if (r.openChanges !== null) {
      open += r.openChanges;
      hasOpen = true;
    }
    if (r.lastWorkMs !== null && (lastWorkMs === null || r.lastWorkMs > lastWorkMs)) {
      lastWorkMs = r.lastWorkMs;
    }
  }
  return { projects: rows.length, sessions: totalSessions, recorded, total, hasCoverage, open, hasOpen, lastWorkMs };
}

// ---------------------------------------------------------------------------
// Single-project installs — skip the picker, embed the Changes graph directly
// (the same ChangeGraph the /review surface renders).
// ---------------------------------------------------------------------------

function SingleProjectChanges({ row }: { row: PickerRow }) {
  const [payload, setPayload] = useState<ReviewListPayload | null>(null);
  const [error, setError] = useState<string | null>(null);
  const projectHash = row.hash ? parseProjectHash(row.hash) : null;

  useEffect(() => {
    if (!projectHash) return;
    let cancelled = false;
    setPayload(null);
    setError(null);
    fetchReviewChanges(projectHash)
      .then((p) => {
        if (!cancelled) setPayload(p);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [projectHash]);

  let body: ReactNode;
  if (!row.hash) {
    body = (
      <div className="border border-danger/30 bg-danger-soft px-4 py-3 text-[13px] text-danger">
        No project hash is recorded for {displayProject(row.name)} — the server
        may predate this surface, or the project has not been ingested yet.
      </div>
    );
  } else if (!projectHash) {
    body = <div role="alert">This project has a malformed identity. Refresh project discovery before opening its changes. No changes request was sent.</div>;
  } else if (error) {
    body = (
      <div className="border border-danger/30 bg-danger-soft px-4 py-3 text-[13px] text-danger">
        Couldn&rsquo;t load the changes: {error}
      </div>
    );
  } else if (!payload) {
    // Same shimmer geometry as the /review list skeleton.
    body = <SkeletonList rows={4} label="Loading changes" />;
  } else {
    body = <ChangeGraph projectHash={projectHash} projectName={row.name} payload={payload} />;
  }

  return (
    // Same fairtrade bare-`section` neutralization as AllSessions: without it
    // this single-project view is capped at --maxw and centred inside its
    // flex-column parent instead of filling it. See the note in
    // components/sessions/AllSessions.tsx for the mechanism.
    <section
      className="flex flex-col gap-3 w-full max-w-none mx-0 px-0"
      data-tour="changes-list"
    >
      <h2 className="text-sm font-medium text-ink">{displayProject(row.name)}</h2>
      {body}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Saved-selection notice — a quiet, expandable disclosure.
//
// This is genuinely useful (a sparse picker otherwise reads as broken) but it
// is NOT urgent: the selection is working as configured. It used to occupy a
// full-width banner above the stats, which gave a working feature the visual
// weight of a problem. It now collapses to a single quiet line that expands on
// click, so the counts and the remedy are one interaction away instead of
// permanently on screen.
//
// Still role="status", never "alert" — and the summary carries the counts, so
// the fact that something is hidden survives without expanding. It never names
// which projects or sessions are hidden; counts only.
// ---------------------------------------------------------------------------

function SelectionNotice({
  notice,
}: {
  notice: { hiddenProjects: number; hiddenSessions: number };
}) {
  const [open, setOpen] = useState(false);
  const parts: string[] = [];
  if (notice.hiddenProjects > 0) {
    parts.push(`${notice.hiddenProjects.toLocaleString()} project${notice.hiddenProjects !== 1 ? "s" : ""}`);
  }
  if (notice.hiddenSessions > 0) {
    parts.push(`${notice.hiddenSessions.toLocaleString()} session${notice.hiddenSessions !== 1 ? "s" : ""}`);
  }
  const summary = `${parts.join(" and ")} hidden by a saved selection`;

  return (
    <div role="status" className="flex flex-col items-start gap-2">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="inline-flex items-center gap-2 border border-rule px-3 py-1.5 font-mono text-xs text-ink-3 hover:text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
      >
        <EyeOff size={13} aria-hidden />
        <span className="tabular-nums">{summary}</span>
        {open ? <ChevronUp size={13} aria-hidden /> : <ChevronDown size={13} aria-hidden />}
      </button>

      {open && (
        <div className="flex flex-col gap-1 border border-rule bg-surface-hover px-4 py-3 text-sm text-ink-2">
          <p>
            A saved project selection is limiting what&rsquo;s shown here.
            The data stays ingested and indexed — it is only hidden from this list.
          </p>
          <p className="text-xs text-ink-3">
            Run <code className="font-mono">peasant kickstart</code> to review or widen the selection.
          </p>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

export default function HomePage() {
  const { data: sessionsData, connected, error: sessionsError, errorCode: sessionsErrorCode } = useChannel<SessionsPayload>(CHANNELS);
  const explainer = useExplainer("home");
  // Titles come from the quality channel (see the hook) — the sessions channel
  // carries no title field.
  const sessionTitles = useSessionTitles();

  const sessions: SessionSummary[] = useMemo(
    () => sessionsData?.sessions ?? [],
    [sessionsData],
  );

  // Per-project stats from the summary endpoint; sessions-channel fallback
  // (stats unavailable) until it resolves — or for good if it fails.
  // Seed from the module cache so returning home renders the last-known
  // columns instantly; the effect below refreshes in the background.
  const [summaries, setSummaries] = useState<DecodedProjectSummariesPayload | null>(
    () => cachedProjectSummaries(),
  );
  // Tracks whether the summary fetch has settled (resolved OR failed), so we
  // can tell "still loading" from "loaded and genuinely empty".
  const [summariesSettled, setSummariesSettled] = useState(false);
  // Distinct from "settled empty": the fetch actually failed, so the per-project
  // coverage / open-changes columns fall back to "—".
  const [summariesFailed, setSummariesFailed] = useState(false);
  const [summariesError, setSummariesError] = useState<unknown>(null);
  // Once REST reports that saved-selection visibility could not be applied,
  // keep the surface fail-closed until a later REST request succeeds. A retry
  // being in flight (or failing for a different reason) must never reopen the
  // stale WebSocket fallback.
  const [selectionFailureLatched, setSelectionFailureLatched] = useState(false);
  const [summariesReload, setSummariesReload] = useState(0);
  useEffect(() => {
    if (sessionsError) {
      setSummaries(null);
      setSummariesSettled(true);
      setSummariesFailed(true);
      setSummariesError(null);
      return;
    }
    let cancelled = false;
    setSummariesSettled(false);
    setSummariesFailed(false);
    fetchProjectSummaries()
      .then((p) => {
        if (!cancelled) {
          setSummaries(p);
          setSummariesError(null);
          setSelectionFailureLatched(false);
        }
      })
      .catch((error: unknown) => {
        if (!cancelled) {
          setSummaries(null);
          setSummariesFailed(true);
          setSummariesError(error);
          if (discoveryErrorCode(error) === DiscoveryErrorCode.SelectionVisibility) {
            setSelectionFailureLatched(true);
          }
        }
      })
      .finally(() => {
        if (!cancelled) setSummariesSettled(true);
      });
    return () => {
      cancelled = true;
    };
  }, [sessionsError, summariesReload]);

  const summariesSelectionFailed =
    selectionFailureLatched ||
    discoveryErrorCode(summariesError) === DiscoveryErrorCode.SelectionVisibility;

  const summaryListState = summaries === null ? null : projectListState(summaries);
  const selectionRecovery =
    summaryListState === ProjectListState.SelectionRecovery ? summaries?.selection ?? null : null;

  // A persisted kickstart selection is narrowing this list AND actually
  // hiding something right now — distinct from summariesSelectionFailed
  // (a broken selection that errors outright). This is a working selection
  // doing its job; a sparse/near-empty picker must say so instead of
  // reading as broken. Never names which
  // projects/sessions are hidden — counts only.
  const activeSelectionNotice =
    !summariesSelectionFailed && !selectionRecovery && summaries && summaries.selection.active
      && (summaries.selection.hiddenProjects > 0 || summaries.selection.hiddenSessions > 0)
      ? summaries.selection
      : null;
  const rows = useMemo(
    () =>
      sessionsError || summariesSelectionFailed
        ? []
        : selectionRecovery
        ? []
        : summaries && summaries.projects.length > 0
        ? rowsFromSummaries(summaries)
        : rowsFromSessions(sessions),
    [summaries, sessions, sessionsError, summariesSelectionFailed, selectionRecovery],
  );

  // Sessions the All Sessions table is allowed to render.
  //
  // VISIBILITY IS DERIVED FROM `rows`, never from the sessions channel directly.
  // `rows` already encodes the whole visibility policy — it empties on a
  // discovery error, on a failed selection, and on selection recovery, and it
  // carries only the projects a working kickstart selection permits. The
  // sessions WS payload is NOT filtered by that selection, so listing it raw
  // would surface projects and session identities the picker is deliberately
  // withholding (regression covered by page.test.tsx's forbiddenIdentities).
  // Intersecting with the visible project set means the table can never expose a
  // project the picker is hiding, in any of those states.
  const visibleProjectNames = useMemo(() => new Set(rows.map((r) => r.name)), [rows]);
  const visibleSessions = useMemo(
    () => sessions.filter((s) => visibleProjectNames.has(s.project ?? UNASSIGNED_PROJECT)),
    [sessions, visibleProjectNames],
  );

  const totalSessions =
    sessionsError || summariesSelectionFailed || selectionRecovery
      ? 0
      : sessions.length > 0
      ? sessions.length
      : rows.reduce((n, r) => n + r.sessions, 0);
  const singleProject = rows.length === 1 ? rows[0] : null;
  const stats = useMemo(() => summaryStats(rows, totalSessions), [rows, totalSessions]);

  // Nothing has resolved yet: summary fetch still in flight AND no sessions
  // message has arrived. Show a skeleton, not a teach/empty state.
  const loading = !summariesSettled && sessionsData === undefined;

  // Pre-format coverage percentage for the KPI tile.
  const coveragePct =
    stats.hasCoverage && stats.total > 0 ? Math.round((stats.recorded / stats.total) * 100) : null;
  const statsPending = !summariesSettled;

  // Shimmer pill sized for a stat tile number — shown while per-project stats
  // are still loading so "—" never has to mean "loading".
  const tileShimmer = <Skeleton as="span" className="inline-block h-5 w-12 align-middle" />;

  // KPI tiles — pre-formatted values handed to StatGrid.
  const kpiTiles: TileSpec[] = [
    {
      key: "projects",
      label: "projects",
      value: stats.projects.toLocaleString(),
      icon: FolderOpen,
    },
    {
      key: "sessions",
      label: "ai conversations",
      value: stats.sessions.toLocaleString(),
      icon: MessageSquare,
    },
    {
      key: "files",
      label: "files built with ai",
      value: coveragePct !== null ? `${coveragePct}%` : statsPending ? tileShimmer : "—",
      sub: stats.hasCoverage
        ? `${stats.recorded.toLocaleString()} of ${stats.total.toLocaleString()} files`
        : undefined,
      icon: Sparkles,
    },
    {
      key: "open",
      label: "unmerged branches",
      value: stats.hasOpen ? stats.open.toLocaleString() : statsPending ? tileShimmer : "—",
      sub: stats.lastWorkMs !== null ? `last worked ${formatRelative(stats.lastWorkMs)}` : undefined,
      icon: GitBranch,
    },
  ];

  // DataState status: 'disconnected' only when the body would be empty AND no
  // local data source is reachable (WS down, REST not yet resolved). When data
  // is present, a dropped socket shows stale content — not a lost-connection
  // panel — matching the "connection ≠ content" principle from the demo.
  const wsStatus: "live" | "disconnected" =
    rows.length === 0 && !connected && summaries === null ? "disconnected" : "live";
  const isEmpty =
    !loading &&
    !sessionsError &&
    !summariesError &&
    rows.length === 0 &&
    (connected || summaries !== null);

  return (
    <div className="max-w-[1600px] mx-auto px-6 pt-6 pb-12 flex flex-col gap-6 animate-fade-up">
      <Breadcrumbs items={[{ label: "projects" }]} />

      {/* Hero — title + the local-first ledger line. */}
      <div data-tour="home">
        <div className="flex items-start gap-2">
          <h1 className="font-[family-name:var(--font-display)] text-2xl font-semibold tracking-tight text-ink">
            Your projects
          </h1>
          <ExplainerToggle explainer={explainer} />
        </div>
        {/* Bridges the nav label ("Changes") to this picker: you pick a project
            to see its changes. */}
        <p className="text-sm text-ink-3 mt-1">
          Pick a project to see what&rsquo;s changed.
          {totalSessions > 0 && (
            <>
              {" "}
              <span className="font-mono tabular-nums">{totalSessions.toLocaleString()}</span>{" "}
              AI conversation{totalSessions !== 1 ? "s" : ""}, on your machine. Nothing has left it.
            </>
          )}
        </p>
      </div>

      {/* The "?" help box opens HERE — directly under the toggle that controls
          it, as a full-width row (returns null while collapsed). */}
      <PickerExplainer explainer={explainer} destination="sessions" />

      {/* A saved selection is narrowing this list and something is actually
          hidden right now. See SelectionNotice above for the presentation
          rationale. */}
      {activeSelectionNotice && (
        <SelectionNotice notice={activeSelectionNotice} />
      )}

      {sessionsError && <p role="alert" className="text-base leading-relaxed text-danger">{discoveryErrorMessage(sessionsError, sessionsErrorCode)}</p>}
      {summariesSelectionFailed && (
        <div className="flex flex-col items-start gap-3">
          <FeedbackPanel variant="error">
            {discoveryErrorMessage(summariesError)}
          </FeedbackPanel>
          <button
            type="button"
            className="border border-rule px-3 py-2 font-mono text-sm text-ink focus-mono"
            onClick={() => {
              setSummaries(null);
              setSummariesReload((value) => value + 1);
            }}
          >
            retry project discovery
          </button>
        </div>
      )}
      {summariesError !== null && !summariesSelectionFailed && (
        <p role="alert" className="text-base leading-relaxed text-danger">
          {discoveryErrorMessage(summariesError)}
        </p>
      )}

      {/* KPI grid — aggregate stats across all projects (picker view only).
          StatGrid replaces the hand-rolled SummaryCard grid: responsive auto-fit
          columns, mono eyebrow labels, tabular display numbers, optional sub lines. */}
      {!loading && rows.length > 0 && !singleProject && (
        <StatGrid tiles={kpiTiles} />
      )}

      {/* Explain a screen of "—": the per-project stats couldn't load. */}
      {summariesFailed && !singleProject && rows.length > 0 && (
        <p className="text-xs text-ink-3">
          Per-project stats couldn&rsquo;t load, so coverage and unmerged-branch
          counts show &ldquo;—&rdquo;. Session counts and last-work are still accurate.
        </p>
      )}

      {/* Body state machine via DataState (connection ≠ content principle):
          loading       → skeleton rows (DataState's built-in shimmer)
          disconnected  → calm lost-connection panel (not an empty state)
          empty         → TeachingEmptyState — what to run and why
          data present  → picker or single-project changes view */}
      <DataState
        loading={loading}
        status={wsStatus}
        empty={isEmpty}
        emptyState={
          selectionRecovery ? (
            <SelectionRecoveryPanel {...selectionRecovery} />
          ) : (
            <TeachingEmptyState
              title="no ai work recorded yet"
              body="run the command below in your terminal. it scans this computer for your ai coding conversations (claude code, codex, and others) and shows what it finds here."
              command="peasant ingest"
            />
          )
        }
        skeletonRows={4}
      >
        {singleProject ? (
          // Keyed by project so a change of the single project resets the
          // embedded list's fetch state via a remount.
          <SingleProjectChanges key={singleProject.name} row={singleProject} />
        ) : (
          // statsPending until the summary fetch settles → the coverage +
          // unmerged cells shimmer instead of popping empty→value.
          <ProjectPicker rows={rows} destination="sessions" statsPending={!summariesSettled} />
        )}
      </DataState>

      {/* Every ingested session, flat and cross-project, beneath the picker —
          the pre-release archive's Projects page shape. Rows open the session
          viewer. Rendered outside DataState because it has its own empty rule
          (it returns null with no sessions) and must not replace the picker's
          teach/empty state. */}
      {!loading && !sessionsError && visibleSessions.length > 0 && (
        <AllSessions sessions={visibleSessions} titles={sessionTitles} />
      )}
    </div>
  );
}
