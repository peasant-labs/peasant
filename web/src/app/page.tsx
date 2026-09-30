"use client";

import { useEffect, useId, useMemo, useState, type ReactNode } from "react";
import { useChannel } from "@/contexts/WebSocketContext";
import { discoveryErrorMessage } from "@/lib/selectionGuidance";
import { DiscoveryErrorCode, discoveryErrorCode } from "@/lib/api/errors";
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
import {
  subscribe,
  type DashboardPayload,
  type QualityPayload,
  type SessionsPayload,
  type SessionSummary,
  type TrendsPayload,
} from "@/types/messages";
import { cachedProjectSummaries, fetchProjectSummaries, type DecodedProjectSummariesPayload } from "@/lib/api/map";
import { formatRelative } from "@/app/review/[[...segments]]/format";
import {
  Button,
  StatGrid,
  DataState,
  FeedbackPanel,
  Input,
  PublishStateLabel,
  TeachingEmptyState,
  type TileSpec,
} from "@/lib/ft-ui";
import { FolderOpen, MessageSquare, Sparkles, GitBranch, EyeOff, ChevronDown, ChevronUp, Search, X } from "lucide-react";
import { AllSessions } from "@/components/sessions/AllSessions";
import { GroupedSessionsSection } from "@/components/sessions/GroupedSessionsSection";
import type { LocalSessionRow } from "@/lib/api/grouped";
import { RootSessionList } from "@/components/home/RootSessionList";
import { RootStats } from "@/components/home/RootStats";
import { useSessionTitles } from "@/hooks/useSessionTitles";
import { useRootSessions } from "@/hooks/useRootSessions";
import { publishFilterCounts, rowPublishState, type RootSessionRow } from "@/lib/home/publishState";
import { rootStatsItems, weeklySessions } from "@/lib/home/rootStats";

const CHANNELS: ["sessions"] = ["sessions"];

/** How long the search box waits after the last keystroke before it searches. */
const SEARCH_DEBOUNCE_MS = 250;

// ---------------------------------------------------------------------------
// Home: `/` lists your sessions. A stats strip, a search box, and one row per
// session with its publish state, filtered by publish state and project.
//
// The rows are the flat sync list joined with the publications read, which
// says whether the saved selection shows each session, so the list, its
// filter counts and its empty states come from one visible set. The earlier
// home flows stay reachable under "more ways to browse": the project picker
// with its aggregate cards (rows from GET /api/v1/projects/summary, falling
// back to sessions-channel grouping), the grouped list with helper threads,
// and the flat filter with its 25-row pager.
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
        className="inline-flex items-center gap-2 border border-rule px-3 py-1.5 font-mono text-sm text-ink-3 hover:text-ink hover:bg-surface-hover transition-colors focus-mono cursor-pointer"
      >
        <EyeOff size={14} aria-hidden />
        <span className="tabular-nums">{summary}</span>
        {open ? <ChevronUp size={14} aria-hidden /> : <ChevronDown size={14} aria-hidden />}
      </button>

      {open && (
        <div className="flex flex-col gap-1 border border-rule bg-surface-hover px-4 py-3 text-sm text-ink-2">
          <p>
            A saved project selection is limiting what&rsquo;s shown here.
            The data stays ingested and indexed — it is only hidden from this list.
          </p>
          <p className="text-sm text-ink-3">
            Run <code className="font-mono">peasant kickstart</code> to review or widen the selection.
          </p>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// More ways to browse.
//
// The session list took over Home's body, and with it the project picker (with
// its aggregate cards), the grouped list with saved helper threads, and the
// flat filter over session identity fields with its 25-row pager. Those are
// real flows, so they stay REACHABLE here, each behind its own disclosure and
// mounted only when opened, over the same selected session set.
// ---------------------------------------------------------------------------

type BrowseView = "projects" | "grouped" | "flat";

const BROWSE_LABELS: Readonly<Record<BrowseView, string>> = {
  projects: "browse by project",
  grouped: "sessions with their helper threads",
  flat: "filter and page through every session",
};

function MoreWaysToBrowse({
  views,
}: {
  views: Partial<Record<BrowseView, ReactNode>>;
}) {
  const baseId = useId();
  const [open, setOpen] = useState<ReadonlySet<BrowseView>>(() => new Set());
  const available = (Object.keys(BROWSE_LABELS) as BrowseView[]).filter((view) => views[view] !== undefined);
  if (available.length === 0) return null;
  const toggle = (view: BrowseView) =>
    setOpen((current) => {
      const next = new Set(current);
      if (next.has(view)) next.delete(view);
      else next.add(view);
      return next;
    });
  return (
    <section aria-labelledby={`${baseId}-heading`} className="mx-0 flex w-full max-w-none flex-col gap-4 border-t border-rule px-0 pt-6">
      <h2 id={`${baseId}-heading`} className="font-[family-name:var(--font-display)] text-lg font-semibold lowercase text-ink">
        more ways to browse
      </h2>
      <div className="flex flex-wrap gap-2">
        {available.map((view) => (
          <Button
            key={view}
            variant="secondary"
            size="sm"
            iconRight={open.has(view) ? ChevronUp : ChevronDown}
            aria-expanded={open.has(view)}
            aria-controls={`${baseId}-${view}`}
            onClick={() => toggle(view)}
          >
            {BROWSE_LABELS[view]}
          </Button>
        ))}
      </div>
      {available.map((view) =>
        open.has(view) ? (
          <div key={view} id={`${baseId}-${view}`} className="flex flex-col gap-4" data-browse-view={view}>
            {views[view]}
          </div>
        ) : null,
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Search box.
//
// Typing searches every selected session through the grouped search route
// (the same `search` variant of the grouped list the palette's results use).
// The list below is replaced by the results while a query is present.
// ---------------------------------------------------------------------------

function RootSearch({
  value,
  onChange,
  selectionActive,
}: {
  value: string;
  onChange: (next: string) => void;
  selectionActive: boolean;
}) {
  const scopeId = useId();
  return (
    <div className="flex flex-col gap-2" role="search">
      <div className="flex items-end gap-2">
        <div className="min-w-0 flex-1">
          <Input
            label="search transcripts"
            type="search"
            iconLeft={Search}
            placeholder="search titles, prompts, tool output"
            value={value}
            onChange={(event) => onChange(event.target.value)}
            aria-describedby={selectionActive ? scopeId : undefined}
          />
        </div>
        {value !== "" && (
          <Button variant="secondary" icon={X} onClick={() => onChange("")}>
            clear search
          </Button>
        )}
      </div>
      {selectionActive && (
        <p id={scopeId} className="m-0 text-base text-ink-2">
          search covers the projects in your saved selection.
        </p>
      )}
    </div>
  );
}

/** The value, once it has held still for `delayMs`. */
function useDebounced(value: string, delayMs: number): string {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    if (value === "") {
      setSettled("");
      return;
    }
    const timer = setTimeout(() => setSettled(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return settled;
}

/**
 * A search result's detail: the first match the search route returned, as
 * plain text, and the session's publish state when the list knows it.
 */
function SearchRowDetail({ row, published }: { row: LocalSessionRow; published?: RootSessionRow }) {
  const match = row.matches?.[0];
  const snippet = match?.snippet.replace(/\s+/g, " ").trim();
  if (!snippet && !published) return null;
  return (
    <div className="mt-1 flex flex-col gap-1">
      {snippet && (
        <p className="m-0 border-l border-rule-strong pl-3 text-base text-ink-2 [overflow-wrap:anywhere]">
          <span className="font-mono text-sm text-ink-3">{match?.role === "user" ? "you" : match?.role}: </span>
          {snippet}
        </p>
      )}
      {published && <PublishStateLabel state={rowPublishState(published)} />}
    </div>
  );
}

/** "38 of 1,284 published. the rest stay on this machine." */
function PublishedLedger({ rows }: { rows: readonly RootSessionRow[] }) {
  const counts = publishFilterCounts(rows);
  return (
    <p className="m-0 text-base text-ink-2">
      <span className="tabular-nums">{counts.published.toLocaleString("en-US")}</span> of{" "}
      <span className="tabular-nums">{counts.all.toLocaleString("en-US")}</span> published. the rest stay on this
      machine.
    </p>
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

  // Sessions the grouped REST list may render.
  //
  // VISIBILITY IS DERIVED FROM THE SERVER ROUTE, never from the sessions
  // channel directly. `/api/v1/sessions?view=grouped` applies the SAME
  // discovery/selection predicate the flat route applies, so the list can never
  // surface a project or session identity the picker is withholding. A
  // saved-selection failure stops the grouped section entirely (fail closed),
  // and a selection-recovery state keeps the recovery panel as the only body.
  const totalSessions =
    sessionsError || summariesSelectionFailed || selectionRecovery
      ? 0
      : sessions.length > 0
      ? sessions.length
      : rows.reduce((n, r) => n + r.sessions, 0);
  const stats = useMemo(() => summaryStats(rows, totalSessions), [rows, totalSessions]);

  // Nothing has resolved yet: summary fetch still in flight AND no sessions
  // message has arrived. Show a skeleton, not a teach/empty state.
  const loading = !summariesSettled && sessionsData === undefined;

  const groupedSectionVisible =
    !loading && !sessionsError && !summariesSelectionFailed && !selectionRecovery;

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

  // The session list: the sync list joined with the publications read. It
  // reads nothing while a selection failure or recovery state owns the page.
  const rootSessions = useRootSessions({
    enabled: !sessionsError && !summariesSelectionFailed && !selectionRecovery,
    invalidationKey: sessionsData,
  });
  const rootRows = groupedSectionVisible ? rootSessions.rows : null;
  const publicationById = useMemo(
    () => new Map((rootRows ?? []).map((row) => [row.sync.id, row])),
    [rootRows],
  );
  const previews = useMemo(
    () =>
      new Map(
        sessions
          .filter((session) => session.preview?.trim())
          .map((session) => [session.id, session.preview as string]),
      ),
    [sessions],
  );

  // The stats strip: each pair from its own topic, left out until it loads.
  const { data: dashboard } = useChannel<DashboardPayload>(subscribe.dashboard());
  const { data: trends } = useChannel<TrendsPayload>(subscribe.trends());
  const { data: quality } = useChannel<QualityPayload>(subscribe.quality());
  const nowMs = Date.now();
  const statsItems = groupedSectionVisible
    ? rootStatsItems({
        dashboardSessions: dashboard ? dashboard.totalSessions : null,
        projects: summaries && !summariesFailed ? summaries.projects.length : null,
        published: rootRows ? publishFilterCounts(rootRows).published : null,
        trendDays: trends ? trends.days ?? [] : null,
        qualityDurations: quality ? (quality.sessions ?? []).map((session) => session.durationMinutes) : null,
        nowMs,
      })
    : [];
  const weeklyCounts = groupedSectionVisible && trends ? weeklySessions(trends.days ?? [], nowMs) : null;
  // Eight empty weeks draw no bars; the strip's "this week" already says 0.
  const weekly = weeklyCounts && weeklyCounts.some((count) => count > 0) ? weeklyCounts : null;

  const [query, setQuery] = useState("");
  const searchQuery = useDebounced(query.trim(), SEARCH_DEBOUNCE_MS);

  const projectBrowser = (
    <>
      <div className="flex items-start gap-2">
        <h3 className="font-[family-name:var(--font-display)] text-base font-semibold lowercase text-ink">projects</h3>
        <ExplainerToggle explainer={explainer} />
      </div>
      {/* The "?" help box opens HERE, directly under the toggle that controls
          it, as a full-width row (returns null while collapsed). */}
      <PickerExplainer explainer={explainer} destination="sessions" />
      {/* KPI grid: aggregate stats across all projects, above the picker. */}
      {rows.length > 0 && <StatGrid tiles={kpiTiles} />}
      {/* Explain a screen of "—": the per-project stats couldn't load. */}
      {summariesFailed && rows.length > 0 && (
        <p className="text-sm text-ink-3">
          Per-project stats couldn&rsquo;t load, so coverage and unmerged-branch
          counts show &ldquo;—&rdquo;. Session counts and last-work are still accurate.
        </p>
      )}
      {/* statsPending until the summary fetch settles → the coverage +
          unmerged cells shimmer instead of popping empty→value. */}
      <ProjectPicker rows={rows} destination="sessions" statsPending={!summariesSettled} />
    </>
  );

  return (
    <div className="mx-auto flex w-full max-w-[1184px] flex-col gap-6 px-6 pt-8 pb-12 animate-fade-up">
      {/* Title and the local-first ledger line. */}
      <header data-tour="home" className="flex flex-col gap-2">
        <h1 className="m-0 font-[family-name:var(--font-display)] text-[length:var(--fs-xl)] font-bold leading-tight lowercase text-[color:var(--ink-strong)]">
          your sessions
        </h1>
        {rootRows && rootRows.length > 0 ? (
          <PublishedLedger rows={rootRows} />
        ) : totalSessions > 0 ? (
          <p className="m-0 text-base text-ink-2">
            <span className="tabular-nums">{totalSessions.toLocaleString("en-US")}</span> session
            {totalSessions !== 1 ? "s" : ""} on this machine.
          </p>
        ) : null}
      </header>

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

      {/* Body state machine via DataState (connection ≠ content principle):
          loading       → skeleton rows (DataState's built-in shimmer)
          disconnected  → calm lost-connection panel (not an empty state)
          empty         → the selection recovery panel, or TeachingEmptyState
          data present  → stats, search, the session list, other views */}
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
        {groupedSectionVisible && (
          <div className="flex flex-col gap-6">
            <RootStats items={statsItems} weekly={weekly} />

            <RootSearch
              value={query}
              onChange={setQuery}
              selectionActive={summaries?.selection.active === true}
            />

            {searchQuery !== "" ? (
              <GroupedSessionsSection
                variant="search"
                query={searchQuery}
                titles={sessionTitles}
                heading="search results"
                emptyState={<p className="border-y border-rule py-6 text-base text-ink-2">no session matches this search.</p>}
                rowDetail={(row) => <SearchRowDetail row={row} published={publicationById.get(row.session.id)} />}
              />
            ) : rootSessions.status === "error" ? (
              <div className="flex flex-col items-start gap-3">
                <FeedbackPanel variant="error">{discoveryErrorMessage(rootSessions.error)}</FeedbackPanel>
                <Button variant="secondary" size="sm" onClick={rootSessions.reload}>
                  retry the session list
                </Button>
              </div>
            ) : rootRows === null ? (
              <SkeletonList rows={6} label="Loading your sessions" />
            ) : (
              <RootSessionList rows={rootRows} titles={sessionTitles} previews={previews} />
            )}

            <MoreWaysToBrowse
              views={{
                projects: projectBrowser,
                grouped: (
                  <GroupedSessionsSection
                    variant="sessions"
                    invalidationKey={sessionsData}
                    titles={sessionTitles}
                    heading="all sessions"
                  />
                ),
                flat:
                  sessions.length > 0 ? (
                    <AllSessions
                      sessions={sessions}
                      titles={sessionTitles}
                      title="every session"
                      subtitle="every ingested session across projects, filterable and paged."
                    />
                  ) : undefined,
              }}
            />
          </div>
        )}
      </DataState>
    </div>
  );
}
