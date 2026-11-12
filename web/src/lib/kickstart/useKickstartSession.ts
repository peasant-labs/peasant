'use client';

/**
 * The live kickstart session of one scope, for any page that edits it.
 *
 * The wizard (`scope: full`) and the standalone selection page (`scope:
 * selection`) mount the same hook. It holds nothing the server owns: the
 * session snapshot, the scan, the forest and the import state are what the
 * server last said, and every answer is sent as a PATCH whose result and
 * pushed echo replace the snapshot.
 *
 * How it stays in step with other tabs, with no poll loop and no reload:
 *
 *   - mount opens (or joins) the scope's session, subscribes to the shared
 *     `kickstart` topic, and loads the forest;
 *   - every pushed event for this session replaces the matching part of the
 *     state; one for another session is ignored;
 *   - the forest is read again only when a scan finishes;
 *   - a write carries the revision this tab last rendered. The server applies
 *     it even when it is stale and names the fields it replaced, which the
 *     hook keeps as `overwritten` until the page dismisses them;
 *   - the socket coming back after a drop re-reads the snapshot once;
 *   - a closed event ends editing: `committed` carries the finish body,
 *     `expired` leaves {@link KickstartSessionApi.reopen} as the way forward.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { LocalKickstartErrorCode } from '@peasant-labs/schema';
import { useKickstartTopic } from '@/contexts/WebSocketContext';
import {
  commitKickstartSession,
  fetchKickstartSession,
  fetchKickstartTree,
  KickstartCommitRefused,
  KickstartRequestError,
  openKickstartSession,
  patchKickstartSession,
  scanKickstartSession,
  startKickstartImport,
  type KickstartAnswers,
} from './client';
import {
  acceptKickstartEvent,
  type KickstartClosedEvent,
  type KickstartCommitted,
  type KickstartConflict,
  type KickstartEvent,
  type KickstartIngest,
  type KickstartIngestEvent,
  type KickstartOverwrite,
  type KickstartScan,
  type KickstartScanEvent,
  type KickstartSession,
  type KickstartSnapshotEvent,
  type KickstartTree,
  type LocalSettingValue,
} from './model';
import { treeField } from './view';
import {
  KickstartCloseReason,
  KickstartIngestState,
  KickstartScanState,
  type KickstartConnectAnswer,
  type KickstartResolution,
  type KickstartScope,
} from './values';

/** How a session ended. A commit carries the finish body; an expiry carries none. */
export interface KickstartEnding {
  readonly reason: KickstartCloseReason;
  readonly committed: KickstartCommitted | null;
}

export interface KickstartSessionApi {
  /** The latest snapshot, or null until the session opens. */
  readonly session: KickstartSession | null;
  /** True from mount until the first snapshot or the first failure. */
  readonly opening: boolean;
  /** Why the session could not open. */
  readonly openError: Error | null;
  /** The discovery job as last pushed. */
  readonly scan: KickstartScan | null;
  /** The import as last pushed, or as the commit reported it. */
  readonly ingest: KickstartIngest | null;
  /** The forest, or null before the first read. */
  readonly tree: KickstartTree | null;
  readonly treeLoading: boolean;
  readonly treeError: Error | null;
  /** Set once the session closed; editing is over. */
  readonly ending: KickstartEnding | null;
  /** Fields a stale write of this tab replaced in another tab, until dismissed. */
  readonly overwritten: readonly KickstartOverwrite[];
  /** The rows of the last refused commit, until the next commit or answer. */
  readonly refusal: readonly KickstartConflict[];
  readonly committing: boolean;
  readonly commitError: Error | null;
  /** Why "import now" did not start an import, until the next try. */
  readonly importError: Error | null;
  /** Why the last answer was refused, until the next answer. */
  readonly answerError: Error | null;
  /** Answers sent and not yet settled. */
  readonly pending: number;

  /**
   * Each write resolves true once the server applied it and false when it was
   * refused or could not be sent (see `answerError`), so a caller moves on only
   * after a recorded answer.
   */
  answer(key: string, value: LocalSettingValue): Promise<boolean>;
  selectIds(ids: readonly string[]): Promise<boolean>;
  resolve(field: string, resolution: KickstartResolution): Promise<boolean>;
  answerConnect(answer: KickstartConnectAnswer): Promise<boolean>;
  commit(): Promise<void>;
  rescan(): Promise<void>;
  reloadTree(): Promise<void>;
  importNow(): Promise<void>;
  reopen(): Promise<void>;
  dismissOverwritten(): void;
}

function asError(failure: unknown): Error {
  return failure instanceof Error ? failure : new Error(String(failure));
}

/**
 * Whether a refusal says the server no longer has this session: it timed out,
 * was saved elsewhere, or the server restarted. The server names it with its
 * own code; a 404 for another reason (a forest row that is gone) does not end
 * the session.
 */
function sessionIsGone(failure: unknown): boolean {
  return failure instanceof KickstartRequestError && failure.code === LocalKickstartErrorCode.SessionNotFound;
}

/**
 * The tree field's answer as text, to tell when it changed between snapshots.
 * Node states are computed by the server and only the tree route carries them,
 * so the forest is read again when the answer moves.
 */
function selectionOf(view: KickstartSession): string | null {
  for (const stop of view.stops) {
    const field = treeField(stop);
    if (field !== undefined) return JSON.stringify(field.value);
  }
  return null;
}

export function useKickstartSession(scope: KickstartScope): KickstartSessionApi {
  const [session, setSession] = useState<KickstartSession | null>(null);
  const [opening, setOpening] = useState(true);
  const [openError, setOpenError] = useState<Error | null>(null);
  const [scan, setScan] = useState<KickstartScan | null>(null);
  const [ingest, setIngest] = useState<KickstartIngest | null>(null);
  const [tree, setTree] = useState<KickstartTree | null>(null);
  const [treeLoading, setTreeLoading] = useState(false);
  const [treeError, setTreeError] = useState<Error | null>(null);
  const [ending, setEnding] = useState<KickstartEnding | null>(null);
  const [overwritten, setOverwritten] = useState<readonly KickstartOverwrite[]>([]);
  const [refusal, setRefusal] = useState<readonly KickstartConflict[]>([]);
  const [committing, setCommitting] = useState(false);
  const [commitError, setCommitError] = useState<Error | null>(null);
  const [importError, setImportError] = useState<Error | null>(null);
  const [answerError, setAnswerError] = useState<Error | null>(null);
  const [pending, setPending] = useState(0);

  // What the next write must carry, and what events may be ignored, live in
  // refs: they change with every event and must never wait for a render.
  const sessionId = useRef<string | null>(null);
  const viewRevision = useRef(-1);
  const scanRevision = useRef(-1);
  const scanState = useRef<KickstartScan | null>(null);
  const selection = useRef<string | null>(null);
  const ingestSeen = useRef(false);
  const ended = useRef(false);
  const alive = useRef(true);
  const buffered = useRef<KickstartEvent[]>([]);
  const writes = useRef<Promise<unknown>>(Promise.resolve());
  const treeRead = useRef({ inFlight: false, queued: false });
  const generation = useRef(0);
  const socketDropped = useRef(false);
  const socketSeen = useRef(false);

  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const loadTree = useCallback(async (): Promise<void> => {
    const id = sessionId.current;
    if (id === null) return;
    if (treeRead.current.inFlight) {
      treeRead.current.queued = true;
      return;
    }
    treeRead.current.inFlight = true;
    const mine = generation.current;
    setTreeLoading(true);
    try {
      const next = await fetchKickstartTree(id);
      if (alive.current && mine === generation.current) {
        setTree(next);
        setTreeError(null);
      }
    } catch (failure) {
      if (alive.current && mine === generation.current) setTreeError(asError(failure));
    } finally {
      treeRead.current.inFlight = false;
      if (alive.current && mine === generation.current) setTreeLoading(false);
      if (treeRead.current.queued) {
        treeRead.current.queued = false;
        if (alive.current && mine === generation.current) void loadTree();
      }
    }
  }, []);

  /**
   * Take a scan state. Returns true when it reads the forest again: the forest
   * is not in any event, so it is read when a scan this page saw running ends.
   * The first scan state seen is not that: the open reads the forest itself.
   */
  const takeScan = useCallback((next: KickstartScan, revision: number): boolean => {
    if (revision < scanRevision.current) return false;
    const previous = scanState.current;
    scanRevision.current = revision;
    scanState.current = next;
    setScan(next);
    const ended = previous !== null && previous.state !== KickstartScanState.Done && next.state === KickstartScanState.Done;
    if (ended) void loadTree();
    return ended;
  }, [loadTree]);

  const takeSession = useCallback((view: KickstartSession): boolean => {
    if (view.id !== sessionId.current || view.revision < viewRevision.current) return false;
    viewRevision.current = view.revision;
    setSession(view);
    const answer = selectionOf(view);
    const moved = selection.current !== null && answer !== selection.current;
    selection.current = answer;
    const reloaded = takeScan(view.scan, view.revision);
    if (moved && !reloaded) void loadTree();
    return reloaded || moved;
  }, [loadTree, takeScan]);

  const takeIngest = useCallback((next: KickstartIngest): void => {
    ingestSeen.current = true;
    setIngest(next);
  }, []);

  const finish = useCallback((reason: KickstartCloseReason, committed: KickstartCommitted | null): void => {
    ended.current = true;
    setEnding((current) => current ?? { reason, committed });
    if (committed !== null && !ingestSeen.current) {
      ingestSeen.current = true;
      setIngest(committed.ingest);
    }
  }, []);

  const applyEvent = useCallback((event: KickstartEvent): void => {
    acceptKickstartEvent(event, {
      snapshot: (e: KickstartSnapshotEvent) => {
        if (e.sessionId === sessionId.current) takeSession(e.view);
      },
      scan: (e: KickstartScanEvent) => {
        if (e.sessionId === sessionId.current) takeScan(e.scan, e.revision);
      },
      ingest: (e: KickstartIngestEvent) => {
        if (e.sessionId === sessionId.current) takeIngest(e.ingest);
      },
      closed: (e: KickstartClosedEvent) => {
        if (e.sessionId !== sessionId.current) return;
        if (e.view !== null) takeSession(e.view);
        finish(e.reason, e.committed);
      },
    });
  }, [finish, takeIngest, takeScan, takeSession]);

  const onEvent = useCallback((event: KickstartEvent): void => {
    // Events can land between the POST being sent and its answer being read.
    // Keep them, then replay those for the session that opened.
    if (sessionId.current === null) {
      buffered.current.push(event);
      return;
    }
    applyEvent(event);
  }, [applyEvent]);

  const { connected } = useKickstartTopic(onEvent);

  const open = useCallback(async (): Promise<void> => {
    generation.current += 1;
    const mine = generation.current;
    sessionId.current = null;
    viewRevision.current = -1;
    scanRevision.current = -1;
    scanState.current = null;
    selection.current = null;
    ingestSeen.current = false;
    ended.current = false;
    buffered.current = [];
    setOpening(true);
    setOpenError(null);
    setEnding(null);
    setOverwritten([]);
    setRefusal([]);
    setCommitError(null);
    setImportError(null);
    setAnswerError(null);
    setIngest(null);
    setTree(null);
    setTreeError(null);
    try {
      const view = await openKickstartSession(scope);
      if (!alive.current || mine !== generation.current) return;
      sessionId.current = view.id;
      takeSession(view);
      for (const event of buffered.current) applyEvent(event);
      buffered.current = [];
      setOpening(false);
      void loadTree();
    } catch (failure) {
      if (!alive.current || mine !== generation.current) return;
      setOpenError(asError(failure));
      setOpening(false);
    }
  }, [applyEvent, loadTree, scope, takeSession]);

  useEffect(() => {
    void open();
    return () => {
      generation.current += 1;
    };
  }, [open]);

  // The socket coming back after a drop: events were missed, so read once.
  useEffect(() => {
    if (!connected) {
      if (socketSeen.current) socketDropped.current = true;
      return;
    }
    socketSeen.current = true;
    if (!socketDropped.current) return;
    socketDropped.current = false;
    const id = sessionId.current;
    if (id === null || ended.current) return;
    const mine = generation.current;
    fetchKickstartSession(id)
      .then((view) => {
        if (!alive.current || mine !== generation.current) return;
        const reloaded = takeSession(view);
        if (!reloaded && view.scan.state === KickstartScanState.Done) void loadTree();
      })
      .catch((failure: unknown) => {
        if (!alive.current || mine !== generation.current) return;
        // The server forgot the session while this tab was away: it ended, and
        // the page can say so and offer a new one.
        if (sessionIsGone(failure)) finish(KickstartCloseReason.Expired, null);
        else setAnswerError(asError(failure));
      });
  }, [connected, finish, loadTree, takeSession]);

  /** Queue one write behind the ones before it, so each carries the revision its predecessor left. */
  const write = useCallback((build: (revision: number) => KickstartAnswers): Promise<boolean> => {
    const run = writes.current.then(async (): Promise<boolean> => {
      const id = sessionId.current;
      if (id === null || ended.current) return false;
      const mine = generation.current;
      setPending((count) => count + 1);
      try {
        const result = await patchKickstartSession(id, build(viewRevision.current));
        if (!alive.current || mine !== generation.current) return false;
        setAnswerError(null);
        setRefusal([]);
        if (result.stale && result.overwritten.length > 0) {
          setOverwritten((current) => [...current, ...result.overwritten]);
        }
        if (result.view !== null) takeSession(result.view);
        return true;
      } catch (failure) {
        if (alive.current && mine === generation.current) {
          if (sessionIsGone(failure)) finish(KickstartCloseReason.Expired, null);
          else setAnswerError(asError(failure));
        }
        return false;
      } finally {
        if (alive.current) setPending((count) => count - 1);
      }
    });
    writes.current = run.then(() => undefined);
    return run;
  }, [finish, takeSession]);

  const answer = useCallback(
    (key: string, value: LocalSettingValue) => write((revision) => ({ revision, values: { [key]: value } })),
    [write],
  );
  const selectIds = useCallback(
    (ids: readonly string[]) => write((revision) => ({ revision, selectedIds: ids })),
    [write],
  );
  const resolve = useCallback(
    (field: string, resolution: KickstartResolution) => write((revision) => ({ revision, resolutions: { [field]: resolution } })),
    [write],
  );
  const answerConnect = useCallback(
    (connectAnswer: KickstartConnectAnswer) => write((revision) => ({ revision, connectAnswer })),
    [write],
  );

  const commit = useCallback(async (): Promise<void> => {
    await writes.current;
    const id = sessionId.current;
    if (id === null || ended.current) return;
    const mine = generation.current;
    setCommitting(true);
    setCommitError(null);
    setRefusal([]);
    try {
      const committed = await commitKickstartSession(id);
      if (alive.current && mine === generation.current) finish(KickstartCloseReason.Committed, committed);
    } catch (failure) {
      if (!alive.current || mine !== generation.current) return;
      if (failure instanceof KickstartCommitRefused) setRefusal(failure.conflicts);
      else if (sessionIsGone(failure)) finish(KickstartCloseReason.Expired, null);
      else setCommitError(asError(failure));
    } finally {
      if (alive.current) setCommitting(false);
    }
  }, [finish]);

  const rescan = useCallback(async (): Promise<void> => {
    const id = sessionId.current;
    if (id === null || ended.current) return;
    const mine = generation.current;
    try {
      const accepted = await scanKickstartSession(id);
      if (alive.current && mine === generation.current) takeScan(accepted, Math.max(scanRevision.current, viewRevision.current));
    } catch (failure) {
      if (alive.current && mine === generation.current) {
        if (sessionIsGone(failure)) finish(KickstartCloseReason.Expired, null);
        else setAnswerError(asError(failure));
      }
    }
  }, [finish, takeScan]);

  const importNow = useCallback(async (): Promise<void> => {
    setImportError(null);
    try {
      await startKickstartImport();
      if (!alive.current) return;
      // Started now, or already running: either way it is running. The topic
      // pushes every change after this.
      takeIngest({ state: KickstartIngestState.Running, note: null, error: null, new: 0, updated: 0 });
    } catch (failure) {
      if (alive.current) setImportError(asError(failure));
    }
  }, [takeIngest]);

  const dismissOverwritten = useCallback(() => setOverwritten([]), []);

  return {
    session,
    opening,
    openError,
    scan,
    ingest,
    tree,
    treeLoading,
    treeError,
    ending,
    overwritten,
    refusal,
    committing,
    commitError,
    importError,
    answerError,
    pending,
    answer,
    selectIds,
    resolve,
    answerConnect,
    commit,
    rescan,
    reloadTree: loadTree,
    importNow,
    reopen: open,
    dismissOverwritten,
  };
}
