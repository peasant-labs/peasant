'use client';

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import {
  LocalKickstartPreviewContinuation,
  LocalKickstartPreviewEventKind,
  LocalKickstartPreviewOp,
  mergeLocalKickstartPreview,
  parseLocalKickstartPreviewEvent,
  type LocalKickstartPreviewEvent,
  type LocalKickstartPreviewState,
} from '@peasant-labs/schema';
import { TranscriptContinuationState } from '@peasant-labs/fairtrade/ui';
import { useKickstartPreviewTransport } from '@/contexts/WebSocketContext';
import {
  PREVIEW_CLOSED_READ_AGAIN,
  PREVIEW_CLOSED_SENTENCE,
  PREVIEW_CLOSED_TONE,
  PREVIEW_CUT_SENTENCE,
  PREVIEW_CUT_TONE,
  PreviewNoticeTone,
  PreviewSentence,
} from './sourcePreview';

/**
 * The raw preview of one session row, streamed over this tab's socket.
 *
 * The server owns the capture: `open` reads the first segment and sends it
 * unasked, each `more` sends exactly one more, and the capture ends with the
 * directory complete (`end`, or `cut` with the server's sentence) or with a
 * `closed` frame. Every frame is applied with the contract's own merge, so this
 * hook holds no rule of its own about what a valid stream is.
 *
 * Only the reader asks for more: `loadMore` is the one place a `more` is sent,
 * at most one is in flight, and nothing here loads on mount, scroll or resize.
 * The stream lives in React state and refs only; it is never written to the
 * channel store, a URL, browser storage, a log or the clipboard. Leaving the
 * row, changing the setup session, unmounting, or a finished scan closes the
 * capture. A capture is never resumed after the socket drops: the pane keeps
 * what it shows and offers to read the session again from the start.
 */

type ContinuationState = (typeof TranscriptContinuationState)[keyof typeof TranscriptContinuationState];

export interface KickstartSourcePreview {
  /** The stream merged so far, or null before the server opened the capture. */
  readonly held: LocalKickstartPreviewState | null;
  /** The viewer's continuation state; null while there is nothing to continue. */
  readonly continuation: ContinuationState | null;
  /** The sentence to show: the server's at an end or a cut, or why the preview stopped. */
  readonly notice: string | null;
  /**
   * How the notice reads: an error card or a plain notice. Decided from the
   * contract's typed cut and close reasons, never from the sentence.
   */
  readonly noticeTone: PreviewNoticeTone;
  /** Whether the pane offers to read the session again from the start. */
  readonly canReadAgain: boolean;
  /** Ask for exactly one more segment. A no-op unless more can load and none is in flight. */
  readonly loadMore: () => void;
  /** Open a new capture of the same row, from the start. */
  readonly readAgain: () => void;
}

interface PreviewView {
  readonly key: string | null;
  readonly held: LocalKickstartPreviewState | null;
  readonly continuation: ContinuationState | null;
  readonly notice: string | null;
  readonly noticeTone: PreviewNoticeTone;
  readonly canReadAgain: boolean;
}

/** The protocol state of the one live capture. Refs, so a frame never waits for a render. */
interface LiveCapture {
  readonly key: string;
  readonly sessionId: string;
  readonly requestId: string;
  captureId: string | null;
  held: LocalKickstartPreviewState | null;
  /** The request id of the `more` in flight, or null when none is. */
  more: string | null;
  /** The first segment has not arrived yet: the open is the request in flight. */
  opening: boolean;
  /** The capture is over: complete, closed by the server, or failed here. */
  done: boolean;
}

const IDLE: PreviewView = Object.freeze({ key: null, held: null, continuation: null, notice: null, noticeTone: PreviewNoticeTone.Neutral, canReadAgain: false });

function openingView(key: string): PreviewView {
  return { key, held: null, continuation: null, notice: null, noticeTone: PreviewNoticeTone.Neutral, canReadAgain: false };
}

/** A request id the contract accepts: 16 base64url characters from 12 random bytes. */
function mintRequestId(): string {
  const bytes = new Uint8Array(12);
  crypto.getRandomValues(bytes);
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** What the viewer draws for a capture that is still open. */
function openContinuation(live: LiveCapture): ContinuationState {
  return live.more !== null || live.opening ? TranscriptContinuationState.Loading : TranscriptContinuationState.More;
}

/** The server's notice lines, as one sentence block. */
function linesText(event: LocalKickstartPreviewEvent): string | null {
  const text = (event.lines ?? []).map((line) => line.text).filter((line) => line.trim().length > 0).join(' ');
  return text.length > 0 ? text : null;
}

export function useKickstartSourcePreview(
  sessionId: string | null,
  nodeId: string | null,
  scanGeneration: number,
): KickstartSourcePreview {
  const transport = useKickstartPreviewTransport();
  const { send, listen, abandon, connected } = transport;
  const [attempt, setAttempt] = useState(0);
  const key = sessionId === null || nodeId === null ? null : `${sessionId}\u0000${nodeId}\u0000${attempt}`;
  const [view, setView] = useState<PreviewView>(IDLE);
  const live = useRef<LiveCapture | null>(null);

  // A new row or session starts from the first attempt again.
  const rowKey = sessionId === null || nodeId === null ? null : `${sessionId}\u0000${nodeId}`;
  const lastRow = useRef(rowKey);
  if (lastRow.current !== rowKey) {
    lastRow.current = rowKey;
    if (attempt !== 0) setAttempt(0);
  }

  /** Stop the live capture here and show why. Closes it on the server when it is still open there. */
  const fail = useCallback((current: LiveCapture, notice: string, noticeTone: PreviewNoticeTone, canReadAgain: boolean, closeOnServer: boolean) => {
    if (closeOnServer && current.captureId !== null) {
      send({ op: LocalKickstartPreviewOp.Close, requestId: mintRequestId(), sessionId: current.sessionId, captureId: current.captureId });
    }
    current.done = true;
    current.more = null;
    current.opening = false;
    setView({
      key: current.key,
      held: current.held,
      continuation: current.held === null ? null : TranscriptContinuationState.Failed,
      notice,
      noticeTone,
      canReadAgain,
    });
  }, [send]);

  // Open on activation. Cleanup runs in the commit that changes the row, so a
  // frame for the old capture can never be applied to the new one.
  useLayoutEffect(() => {
    if (key === null || sessionId === null || nodeId === null) {
      setView(IDLE);
      return;
    }
    const requestId = mintRequestId();
    const current: LiveCapture = { key, sessionId, requestId, captureId: null, held: null, more: null, opening: true, done: false };
    live.current = current;
    setView(openingView(key));
    if (!send({ op: LocalKickstartPreviewOp.Open, requestId, sessionId, nodeId })) {
      fail(current, PreviewSentence.ConnectionDropped, PreviewNoticeTone.Error, true, false);
    }
    return () => {
      if (live.current === current) live.current = null;
      if (current.done) return;
      current.done = true;
      if (current.captureId !== null) {
        send({ op: LocalKickstartPreviewOp.Close, requestId: mintRequestId(), sessionId, captureId: current.captureId });
      } else {
        abandon(requestId, sessionId);
      }
    };
  }, [key, sessionId, nodeId, send, abandon, fail]);

  // A capture dies with its connection. The pane keeps what it shows.
  useEffect(() => {
    const current = live.current;
    if (!connected && current !== null && !current.done) {
      current.captureId = null;
      fail(current, PreviewSentence.ConnectionDropped, PreviewNoticeTone.Error, true, false);
    }
  }, [connected, fail]);

  // A finished scan closes the capture: the rows it was read from may have moved.
  const lastScan = useRef(scanGeneration);
  useEffect(() => {
    if (lastScan.current === scanGeneration) return;
    lastScan.current = scanGeneration;
    const current = live.current;
    if (current !== null && !current.done) fail(current, PreviewSentence.Rescanned, PreviewNoticeTone.Neutral, true, true);
  }, [scanGeneration, fail]);

  const onFrame = useCallback((data: unknown) => {
    const current = live.current;
    if (current === null || current.done) return;
    let event: LocalKickstartPreviewEvent;
    try {
      event = parseLocalKickstartPreviewEvent(data);
    } catch {
      fail(current, PreviewSentence.FrameRefused, PreviewNoticeTone.Error, true, true);
      return;
    }
    if (event.kind === LocalKickstartPreviewEventKind.Closed) {
      const ours = (event.captureId !== undefined && event.captureId === current.captureId)
        || (event.requestId !== undefined && (event.requestId === current.requestId || event.requestId === current.more));
      if (!ours || event.reason === undefined) return;
      current.captureId = null;
      fail(current, event.message || PREVIEW_CLOSED_SENTENCE[event.reason], PREVIEW_CLOSED_TONE[event.reason], PREVIEW_CLOSED_READ_AGAIN[event.reason], false);
      return;
    }
    if (event.kind === LocalKickstartPreviewEventKind.Opened) {
      if (event.requestId !== current.requestId || current.captureId !== null) return;
    } else if (event.captureId === undefined || event.captureId !== current.captureId) {
      return;
    }
    let next: LocalKickstartPreviewState;
    try {
      next = mergeLocalKickstartPreview(current.held ?? undefined, data);
    } catch {
      if (event.kind === LocalKickstartPreviewEventKind.Opened && event.captureId !== undefined) current.captureId = event.captureId;
      fail(current, PreviewSentence.FrameRefused, PreviewNoticeTone.Error, true, true);
      return;
    }
    current.held = next;
    current.captureId = next.captureId;
    if (event.kind === LocalKickstartPreviewEventKind.Segment) {
      current.opening = false;
      current.more = null;
    }
    if (next.continuation === LocalKickstartPreviewContinuation.More) {
      setView({ key: current.key, held: next, continuation: openContinuation(current), notice: null, noticeTone: PreviewNoticeTone.Neutral, canReadAgain: false });
      return;
    }
    // The directory is complete: the server has freed the capture.
    current.done = true;
    const cut = next.continuation === LocalKickstartPreviewContinuation.Cut;
    const notice = linesText(event) ?? (cut && event.cutReason !== undefined ? PREVIEW_CUT_SENTENCE[event.cutReason] : null);
    setView({
      key: current.key,
      held: next,
      continuation: cut ? TranscriptContinuationState.Cut : TranscriptContinuationState.End,
      notice,
      // An end, and a cut at a designed limit, are information; a cut because a
      // record could not be shown is an error.
      noticeTone: cut && event.cutReason !== undefined ? PREVIEW_CUT_TONE[event.cutReason] : PreviewNoticeTone.Neutral,
      canReadAgain: false,
    });
  }, [fail]);

  const latestFrame = useRef(onFrame);
  useLayoutEffect(() => {
    latestFrame.current = onFrame;
  });
  useEffect(() => listen((data) => latestFrame.current(data)), [listen]);

  const loadMore = useCallback(() => {
    const current = live.current;
    if (current === null || current.done || current.opening || current.more !== null || current.captureId === null || current.held === null) return;
    if (current.held.continuation !== LocalKickstartPreviewContinuation.More) return;
    const requestId = mintRequestId();
    if (!send({ op: LocalKickstartPreviewOp.More, requestId, sessionId: current.sessionId, captureId: current.captureId, after: current.held.seq })) {
      current.captureId = null;
      fail(current, PreviewSentence.ConnectionDropped, PreviewNoticeTone.Error, true, false);
      return;
    }
    current.more = requestId;
    setView({ key: current.key, held: current.held, continuation: TranscriptContinuationState.Loading, notice: null, noticeTone: PreviewNoticeTone.Neutral, canReadAgain: false });
  }, [send, fail]);

  const readAgain = useCallback(() => setAttempt((value) => value + 1), []);

  // A render for a new row never exposes the previous row's stream, even
  // before the effect above has replaced it.
  const shown = view.key === key ? view : key === null ? IDLE : openingView(key);
  return {
    held: shown.held,
    continuation: shown.continuation,
    notice: shown.notice,
    noticeTone: shown.noticeTone,
    canReadAgain: shown.canReadAgain,
    loadMore,
    readAgain,
  };
}
