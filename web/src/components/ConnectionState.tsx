import { EmptyState, FeedbackPanel } from "@/lib/ft-ui";

/**
 * One source of truth for how the app talks about its connection to the local
 * Peasant program. Before this, the same "disconnected" news was written three
 * different ways and leaked the word "WebSocket" to users. The model:
 *
 * - The **top-nav pill** (TopNavbar) is the single PERSISTENT, glanceable
 *   indicator — always on screen.
 * - A page renders <Disconnected/> only when losing the connection is the
 *   reason its content area is empty (so the user isn't left staring at an
 *   endless skeleton). It's contextual, not a second copy of the nav pill.
 *
 * The connection is to a program on THIS computer — no internet — so the copy
 * leads with that, and stays plain.
 */

export const CONNECTION = {
  /** Top-nav pill text. */
  liveLabel: "live · local",
  connectingLabel: "connecting…",
  /** Top-nav pill tooltips. */
  liveTitle:
    "Connected to the Peasant app running on this computer; no internet involved.",
  connectingTitle: "Trying to reach the Peasant app on this computer…",
  /** Content-area copy when disconnection blocks a view (initial or dropped). */
  blockedTitle: "waiting for the peasant app",
  blockedBody:
    "It runs on this computer; nothing has left your machine. This page comes back on its own.",
  /** One-line note when stale data is still on screen after a drop. */
  staleNote: "connection lost; showing the last loaded data.",
} as const;

/**
 * The content-area "we lost the local connection" state. Use where a page would
 * otherwise show a misleading empty state or an endless skeleton while
 * disconnected. `variant="note"` is a single quiet line (for canvases / strips);
 * `variant="card"` is a full teaching block.
 */
export function Disconnected({
  variant = "card",
}: {
  variant?: "card" | "note";
}) {
  if (variant === "note") {
    // role=status so the stale-data note is announced when it appears.
    return (
      <p role="status" className="text-xs text-ink-3">
        {CONNECTION.staleNote}
      </p>
    );
  }
  // role=status/aria-live so screen readers announce the lost-connection state,
  // which appears dynamically as the sole content-area signal.
  return (
    <div role="status" aria-live="polite">
      <EmptyState title={CONNECTION.blockedTitle} message={CONNECTION.blockedBody} />
    </div>
  );
}

/**
 * A single, plain disconnected strip for pages whose body is a skeleton/list
 * (not a teach state) — e.g. the Changes list. Returns null while connected (the
 * nav pill is the steady-state indicator), so it never doubles up. One strip,
 * keyed on whether data ever loaded — replaces the old stacking
 * "Waiting for WebSocket connection…" + raw `wsError` strips.
 */
export function ConnectionStatus({
  connected,
  hasData,
}: {
  connected: boolean;
  hasData?: boolean;
}) {
  if (connected) return null;
  // Stale data still on screen → a quiet "lost connection" feedback (not an
  // empty state; the data is real, just no longer live). No data yet → the
  // still-connecting state.
  return hasData ? (
    <FeedbackPanel variant="error" title="connection lost">
      Showing the last loaded data.
    </FeedbackPanel>
  ) : (
    <FeedbackPanel variant="loading" title="connecting…">
      {CONNECTION.connectingTitle}
    </FeedbackPanel>
  );
}
